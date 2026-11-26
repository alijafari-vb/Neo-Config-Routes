package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/neoauroraproject/Neo-Uptime/internal/monitor"
	"github.com/neoauroraproject/Neo-Uptime/internal/notify"
	"github.com/neoauroraproject/Neo-Uptime/internal/store"
	"github.com/neoauroraproject/Neo-Uptime/internal/xdns"
)

// StartConfigRoutes attaches the module before the panel begins serving.
func (s *Server) StartConfigRoutes(ctx context.Context) (func(), error) {
	module, err := xdns.Open(filepath.Join(s.dataDir, "xdns"), xdns.Sources{
		Snapshot: s.routeSnapshot, Validate: s.validateRoute, Notify: s.routeNotification, Ping: s.routePing,
	})
	if err != nil {
		unavailable := s.admin(func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusServiceUnavailable, "Config Routes could not open its data store; check the Neo Uptime logs")
		})
		s.mux.HandleFunc("/api/xdns", unavailable)
		s.mux.HandleFunc("/api/xdns/", unavailable)
		return nil, err
	}
	module.Mount(s.mux, s.admin)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); module.Run(runCtx) }()
	return func() { cancel(); <-done; _ = module.Close() }, nil
}

func (s *Server) routeSnapshot(ctx context.Context, ids []int64) (map[int64]xdns.Monitor, error) {
	monitors, err := s.db.ListMonitors(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*store.Monitor, len(monitors))
	for _, m := range monitors {
		byID[m.ID] = m
	}
	beats, err := s.db.RecentHeartbeats(ctx, ids, 16)
	if err != nil {
		return nil, err
	}
	result := make(map[int64]xdns.Monitor, len(ids))
	for _, id := range ids {
		m := byID[id]
		if m == nil {
			continue
		}
		source := xdns.Monitor{Name: m.Name,
			Available: m.Active && !s.mgr.InMaintenance(id),
			MaxGap:    time.Duration(max(45, 2*max(m.Interval, m.RetryInterval)+m.Timeout)) * time.Second,
		}
		if source.Available {
			history := beats[id]
			for i := len(history) - 1; i >= 0; i-- {
				h := history[i]
				source.Beats = append(source.Beats, xdns.Beat{Time: h.Time, Status: h.Status, Ping: h.Ping})
			}
			if h := s.mgr.StateOf(id).Last; h != nil && (len(history) == 0 || h.Time > history[0].Time) {
				source.Beats = append(source.Beats, xdns.Beat{Time: h.Time, Status: h.Status, Ping: h.Ping})
			}
		}
		result[id] = source
	}
	return result, nil
}

func (s *Server) validateRoute(ctx context.Context, g xdns.Group) error {
	for _, member := range g.Members {
		m, err := s.db.GetMonitor(ctx, member.MonitorID)
		if err != nil || m.Type == monitor.TypeGroup {
			if !g.Enabled && errors.Is(err, store.ErrNotFound) {
				continue
			}
			return fmt.Errorf("select an existing check monitor for member %d", member.MonitorID)
		}
	}
	for _, id := range g.Channels {
		if _, err := s.db.GetNotification(ctx, id); err != nil {
			if !g.Notify && errors.Is(err, store.ErrNotFound) {
				continue
			}
			return fmt.Errorf("notification channel %d no longer exists", id)
		}
	}
	return nil
}

func (s *Server) routeNotification(ctx context.Context, id int64, e xdns.Event) error {
	channel, err := s.db.GetNotification(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !channel.Active) {
		return nil
	}
	if err != nil {
		return err
	}
	lang := channel.Config["language"]
	titles := map[string][2]string{
		xdns.Down:    {"Failover · down", "جابه‌جایی · قطعی"},
		xdns.Slow:    {"Failover · slow", "جابه‌جایی · کندی"},
		xdns.Recover: {"Preferred route restored", "بازگشت به مسیر ترجیحی"},
		xdns.Route:   {"Route selected", "مسیر انتخاب شد"},
		xdns.AllDown: {"All members down", "همهٔ اعضا قطع‌اند"},
		xdns.Failure: {"DNS update or lookup failed", "خطا در بررسی یا تغییر DNS"},
	}
	kind := e.Reason
	status := notify.StatusUp
	if e.Status == "failed" || e.Status == "uncertain" {
		kind, status = xdns.Failure, notify.StatusDown
	}
	if e.Status == "held" {
		status = notify.StatusDown
	}
	i := 0
	if lang == "fa" {
		i = 1
	}
	title := titles[kind][i] + " · " + e.GroupName
	from, to := e.Before, e.After
	if e.FromMonitor != "" {
		from += " (" + e.FromMonitor + ")"
	}
	if e.ToMonitor != "" {
		to += " (" + e.ToMonitor + ")"
	}
	message := e.Message
	if to != "" {
		message = from + " → " + to + "\n" + message
	} else if from != "" {
		message = from + "\n" + message
	}
	tz := s.db.GetSetting(ctx, "timezone", "auto")
	base := strings.TrimRight(s.db.GetSetting(ctx, "primaryBaseURL", ""), "/")
	link := ""
	if base != "" {
		link = base + "/config-routes"
	}
	return notify.Send(ctx, channel.Type, channel.Config, notify.Event{
		Kind: "config-route", CustomTitle: title, MonitorName: e.GroupName, MonitorType: "config-route",
		Target: e.Record, Time: time.UnixMilli(e.At), Status: status, Msg: message, Lang: lang, TZ: tz, Link: link,
		SiteTitle: s.db.GetSetting(ctx, "siteTitle", "Neo Uptime"),
	})
}

func (s *Server) routePing(ctx context.Context, ids []int64, start time.Time) (map[int64][]xdns.PingBucket, error) {
	slots, err := s.db.Buckets(ctx, ids, false, start)
	if err != nil {
		return nil, err
	}
	result := make(map[int64][]xdns.PingBucket, len(slots))
	for id, buckets := range slots {
		for _, b := range buckets {
			if b.TS > time.Now().Unix() {
				break
			}
			result[id] = append(result[id], xdns.PingBucket{At: b.TS * 1000, Avg: b.AvgPing, Min: b.MinPing, Max: b.MaxPing})
		}
	}
	return result, nil
}
