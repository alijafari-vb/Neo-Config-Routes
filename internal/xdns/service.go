package xdns

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"
)

const cadence = 15 * time.Second

type Sources struct {
	Snapshot func(context.Context, []int64) (map[int64]Monitor, error)
	Ping     func(context.Context, []int64, time.Time) (map[int64][]PingBucket, error)
	Validate func(context.Context, Group) error
	Notify   func(context.Context, int64, Event) error
}

type View struct {
	Group
	Health         []Health    `json:"health"`
	Current        string      `json:"current"`
	Simulated      string      `json:"simulated"`
	LastError      string      `json:"lastError"`
	Cloud          CloudHealth `json:"cloud"`
	ErrorAlerts    bool        `json:"errorAlerts"`
	LastEvaluation int64       `json:"lastEvaluation"`
	LastSwitch     int64       `json:"lastSwitch"`
	Reason         string      `json:"reason"`
}

type runtime struct {
	View
	record    DNSRecord
	checked   time.Time
	retry     time.Time
	failures  int
	drySwitch int64
}

type Service struct {
	repo          *repository
	dns           DNS
	src           Sources
	wake          chan struct{}
	mutation      sync.Mutex // serializes rules/settings changes with DNS writes
	viewsMu       sync.RWMutex
	views         map[int64]View
	state         map[int64]*runtime // only the evaluator owns runtime state
	backoff       time.Time
	settingsEpoch uint64 // protected by mutation
	observedEpoch uint64 // evaluator only
}

func Open(dir string, src Sources) (*Service, error) {
	db, err := openRepository(dir)
	if err != nil {
		return nil, err
	}
	return &Service{repo: db, dns: NewCloudflare(), src: src, wake: make(chan struct{}, 1), views: map[int64]View{}, state: map[int64]*runtime{}}, nil
}

func (s *Service) Close() error { return s.repo.db.Close() }

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Run(ctx context.Context) {
	var notifications sync.WaitGroup
	if s.src.Notify != nil {
		notifications.Go(func() { s.deliver(ctx) })
	}
	defer notifications.Wait()
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	s.Wake()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		if err := s.tick(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Config Routes evaluation: %v", err)
		}
	}
}

func (s *Service) tick(ctx context.Context) error {
	groups, err := s.repo.groups(ctx)
	if err != nil {
		return err
	}
	ids := map[int64]bool{}
	for _, g := range groups {
		for _, m := range g.Members {
			ids[m.MonitorID] = true
		}
	}
	if len(ids) == 0 {
		s.state = map[int64]*runtime{}
		return nil
	}
	monitorIDs := make([]int64, 0, len(ids))
	for id := range ids {
		monitorIDs = append(monitorIDs, id)
	}
	data, err := s.src.Snapshot(ctx, monitorIDs)
	if err != nil {
		return err
	}
	var failures []error
	active := map[int64]bool{}
	for _, g := range groups {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		active[g.ID] = true
		if err := s.process(ctx, g, data); err != nil {
			failures = append(failures, fmt.Errorf("group %d: %w", g.ID, err))
		}
	}
	for id := range s.state {
		if !active[id] {
			delete(s.state, id)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) process(ctx context.Context, g Group, monitors map[int64]Monitor) error {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	latest, err := s.repo.group(ctx, g.ID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && latest.Revision != g.Revision) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.observedEpoch != s.settingsEpoch {
		s.state = map[int64]*runtime{}
		s.backoff = time.Time{}
		s.observedEpoch = s.settingsEpoch
	}
	now := time.Now()
	rt := s.state[g.ID]
	if rt == nil || rt.Revision != g.Revision {
		rt = &runtime{View: View{Group: g, Health: []Health{}, ErrorAlerts: errorAlertsEnabled(g)}}
		s.state[g.ID] = rt
		rt.Cloud, err = s.repo.cloudHealth(ctx, g)
		if err != nil {
			delete(s.state, g.ID)
			return err
		}
		rt.LastError = rt.Cloud.Message
		rt.LastSwitch, err = s.repo.lastSwitch(ctx, g)
		if err != nil {
			delete(s.state, g.ID)
			return err
		}
	}
	health := make([]Health, 0, len(g.Members))
	for i, m := range g.Members {
		var previous Health
		if i < len(rt.Health) {
			previous = rt.Health[i]
		}
		health = append(health, observe(m, g, monitors[m.MonitorID], previous, now))
	}
	rt.Health, rt.LastEvaluation = health, now.UnixMilli()
	defer func() {
		s.viewsMu.Lock()
		s.views[g.ID] = rt.View
		s.viewsMu.Unlock()
	}()
	pending, err := s.repo.pending(ctx, g)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !g.Enabled {
		rt.Reason = "disabled"
	}
	if now.Before(rt.retry) || now.Before(s.backoff) {
		return nil
	}
	token, dry, err := s.repo.settings(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		if !g.Enabled && pending.ID == 0 {
			return nil
		}
		return s.recordError(ctx, rt, errors.New("Cloudflare API token is not configured"), now, dry)
	}
	fresh := false
	if rt.record.ID == "" || now.Sub(rt.checked) >= time.Minute || pending.ID != 0 {
		if err := s.refresh(ctx, rt, token, dry); err != nil {
			if !g.Enabled && pending.ID == 0 {
				return s.noteError(ctx, rt, err, "lookup")
			}
			return s.recordError(ctx, rt, err, now, dry)
		}
		fresh = true
	}
	if pending.ID != 0 {
		pending.At = time.Now().UnixMilli()
		if rt.record.ID == pending.RecordID && rt.record.Content == pending.After {
			pending.Status, pending.Message = "changed", "Verified by reading the record after an interrupted update"
			rt.LastSwitch = pending.At
		} else {
			pending.Status, pending.Message = "failed", "Cloudflare did not confirm the pending change"
		}
		if err := s.repo.finishIncident(ctx, pending, channelsFor(g, pending), Failure+"-update"); err != nil {
			return err
		}
		if pending.Status == "failed" {
			return s.noteError(ctx, rt, errors.New(pending.Message), "update")
		}
		if err := s.clearCloudError(ctx, rt); err != nil {
			return err
		}
	}
	if !g.Enabled {
		rt.Reason = "disabled"
		return nil
	}
	current := rt.Current
	lastSwitch := rt.LastSwitch
	if dry && rt.Simulated != "" {
		current, lastSwitch = rt.Simulated, rt.drySwitch
	}
	decision := choose(g, current, rt.Health)
	rt.Reason = decision.Reason
	if decision.Target != "" && now.UnixMilli()-lastSwitch >= int64(g.CooldownSeconds)*1000 && !fresh {
		if err := s.refresh(ctx, rt, token, dry); err != nil {
			return s.recordError(ctx, rt, err, now, dry)
		}
		current = rt.Current
		if dry && rt.Simulated != "" {
			current = rt.Simulated
		}
		decision = choose(g, current, rt.Health)
		rt.Reason = decision.Reason
	}
	incidentKind := AllDown
	if dry {
		incidentKind += "-dry"
	}
	if decision.Reason != AllDown {
		if err := s.repo.clearIncident(ctx, g.ID, incidentKind); err != nil {
			return err
		}
	}
	if decision.Reason == AllDown && g.KeepLast {
		event := eventFor(rt, Decision{Reason: AllDown}, current, "held", "All members are confirmed down", now)
		if _, err := s.repo.addEvent(ctx, g, event, incidentKind, alertChannels(g, event, dry)); err != nil {
			return err
		}
	}
	if decision.Target == "" {
		return nil
	}
	if now.UnixMilli()-lastSwitch < int64(g.CooldownSeconds)*1000 {
		rt.Reason = "cooldown"
		return nil
	}
	if dry {
		event := eventFor(rt, decision, current, "simulated", "Dry run · no DNS write", now)
		if _, err := s.repo.addEvent(ctx, g, event, "", nil); err != nil {
			return err
		}
		rt.Simulated, rt.drySwitch = decision.Target, now.UnixMilli()
		return nil
	}
	if decision.Target == rt.record.Content {
		return nil
	}
	event := eventFor(rt, decision, current, "pending", "", now)
	event.RecordID = rt.record.ID
	event.ID, err = s.repo.addEvent(ctx, g, event, "", nil)
	if err != nil {
		return err
	}
	record, err := s.dns.Update(ctx, token, g, rt.record, decision.Target)
	if err != nil {
		if noteErr := s.noteError(ctx, rt, err, "update"); noteErr != nil {
			return noteErr
		}
		event.At = time.Now().UnixMilli()
		event.Message = err.Error()
		// A timeout, 5xx or invalid response can follow a successful write.
		// Preserve the intent until a fresh read establishes the outcome.
		var apiErr *cloudError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 408 {
			event.Status = "failed"
			return s.repo.finishIncident(ctx, event, channelsFor(g, event), Failure+"-update")
		}
		event.Status = "uncertain"
		_, storeErr := s.repo.addEvent(ctx, g, event, Failure+"-update", channelsFor(g, event))
		return storeErr
	}
	event.At, event.Status, event.Message = time.Now().UnixMilli(), "changed", "Cloudflare confirmed the DNS change"
	if err := s.repo.finish(ctx, event, channelsFor(g, event)); err != nil {
		return err
	}
	rt.record, rt.Current, rt.LastSwitch = record, record.Content, event.At
	rt.checked = time.UnixMilli(event.At)
	rt.Cloud.VerifiedAt = event.At
	if err := s.repo.observeRoute(ctx, g, rt.checked, record.Content); err != nil {
		return err
	}
	return s.clearCloudError(ctx, rt)
}

func (s *Service) refresh(ctx context.Context, rt *runtime, token string, dry bool) error {
	record, err := s.dns.Lookup(ctx, token, rt.Group)
	if err != nil {
		return err
	}
	if rt.record.ID != "" && (rt.record.ID != record.ID || rt.record.Content != record.Content) {
		rt.Simulated, rt.drySwitch = "", 0
	}
	rt.record, rt.Current, rt.checked = record, record.Content, time.Now()
	rt.Cloud.VerifiedAt = rt.checked.UnixMilli()
	if err := s.repo.observeRoute(ctx, rt.Group, rt.checked, record.Content); err != nil {
		return err
	}
	if err := s.repo.clearIncident(ctx, rt.ID, failureIncident(dry)); err != nil {
		return err
	}
	if rt.Cloud.Operation != "update" {
		return s.clearCloudError(ctx, rt)
	}
	return s.repo.setCloudHealth(ctx, rt.Group, rt.Cloud)
}

func (s *Service) clearCloudError(ctx context.Context, rt *runtime) error {
	operation := rt.Cloud.Operation
	rt.LastError, rt.failures = "", 0
	rt.retry = time.Time{}
	rt.Cloud = CloudHealth{VerifiedAt: rt.Cloud.VerifiedAt}
	if err := s.repo.setCloudHealth(ctx, rt.Group, rt.Cloud); err != nil {
		return err
	}
	if operation == "update" {
		return s.repo.clearIncident(ctx, rt.ID, Failure+"-update")
	}
	return nil
}

func (s *Service) noteError(ctx context.Context, rt *runtime, err error, op string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := time.Now()
	rt.LastError = err.Error()
	rt.failures = min(rt.failures+1, 6)
	rt.retry = now.Add(15 * time.Second << rt.failures)
	var apiErr *cloudError
	if errors.As(err, &apiErr) && apiErr.Retry > 0 {
		s.backoff = now.Add(apiErr.Retry)
		if rt.retry.Before(s.backoff) {
			rt.retry = s.backoff
		}
	}
	rt.Cloud.Operation, rt.Cloud.Message, rt.Cloud.FailedAt, rt.Cloud.RetryAt = op, rt.LastError, now.UnixMilli(), rt.retry.UnixMilli()
	if err := s.repo.setCloudHealth(ctx, rt.Group, rt.Cloud); err != nil {
		return err
	}
	return s.repo.observeRoute(ctx, rt.Group, now, "")
}

func (s *Service) recordError(ctx context.Context, rt *runtime, err error, now time.Time, dry bool) error {
	if err := s.noteError(ctx, rt, err, "lookup"); err != nil {
		return err
	}
	e := eventFor(rt, Decision{Reason: Failure}, rt.Current, "failed", rt.LastError, now)
	_, err = s.repo.addEvent(ctx, rt.Group, e, failureIncident(dry), alertChannels(rt.Group, e, dry))
	return err
}

func eventFor(rt *runtime, d Decision, current, status, message string, now time.Time) Event {
	e := Event{GroupID: rt.ID, GroupName: rt.Name, Record: rt.Record(), Kind: rt.Kind, At: now.UnixMilli(),
		Reason: d.Reason, Status: status, Before: current, After: d.Target, Message: message}
	for _, h := range rt.Health {
		switch h.Target {
		case current:
			e.FromMonitor = h.Name
		case d.Target:
			e.ToMonitor = h.Name
		}
	}
	return e
}

func failureIncident(dry bool) string {
	if dry {
		return Failure + "-dry"
	}
	return Failure
}

func alertChannels(g Group, e Event, dry bool) []int64 {
	if dry {
		return nil
	}
	return channelsFor(g, e)
}

func channelsFor(g Group, e Event) []int64 {
	if !g.Notify || e.Status == "simulated" {
		return nil
	}
	kind := e.Reason
	if e.Status == "failed" || e.Status == "uncertain" {
		kind = Failure
	}
	if slices.Contains(g.Events, kind) {
		return g.Channels
	}
	return nil
}

func (s *Service) deliver(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		batch, err := s.repo.deliveries(ctx)
		if err != nil {
			log.Printf("Config Routes notification queue: %v", err)
			continue
		}
		var workers sync.WaitGroup
		for _, job := range batch {
			workers.Go(func() {
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				group, err := s.repo.group(callCtx, job.Event.GroupID)
				if errors.Is(err, sql.ErrNoRows) {
					err = nil
				} else if err == nil && slices.Contains(channelsFor(group, job.Event), job.Channel) {
					err = s.src.Notify(callCtx, job.Channel, job.Event)
				}
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					log.Printf("Config Routes event %d: notification channel %d failed", job.Event.ID, job.Channel)
				}
				if err := s.repo.delivered(ctx, job, err != nil); err != nil {
					log.Printf("Config Routes: cannot update notification delivery for event %d", job.Event.ID)
				}
			})
		}
		workers.Wait()
	}
}
