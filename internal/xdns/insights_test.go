package xdns

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRouteCoverageUsesSwitchesSamplesAndGaps(t *testing.T) {
	ctx := context.Background()
	r, err := openRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.db.Close()
	g, err := r.save(ctx, testGroup())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	point := func(minutes int) int64 { return start.Add(time.Duration(minutes) * time.Minute).UnixMilli() }
	for _, e := range []Event{
		{At: point(-10), Status: "changed", After: "sia.example.net"},
		{At: point(15), Status: "simulated", After: "port.example.net"},
		{At: point(30), Status: "failed"},
		{At: point(50), Status: "changed", After: "port.example.net"},
	} {
		if _, err := r.addEvent(ctx, g, e, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []routePoint{{at: point(60), target: "port.example.net"}, {at: point(61), target: "port.example.net"}, {at: point(62)}, {at: point(70), target: "sia.example.net"}} {
		if err := r.observeRoute(ctx, g, time.UnixMilli(p.at), p.target); err != nil {
			t.Fatal(err)
		}
	}
	points, err := r.routePoints(ctx, g, point(0), point(74))
	if err != nil {
		t.Fatal(err)
	}
	slots := routeBuckets(points, point(0), point(74))
	minutes := time.Minute.Milliseconds()
	if len(slots) != 2 || slots[0].Targets["sia.example.net"] != 30*minutes ||
		slots[0].Targets["port.example.net"] != 10*minutes || slots[1].Targets["port.example.net"] != 2*minutes ||
		slots[1].Targets["sia.example.net"] != 2*minutes {
		t.Fatalf("unexpected route coverage: %+v", slots)
	}
}

func TestDailyCoverageSplitsAtLocalMidnight(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 26, 20, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	days := dayBuckets([]routePoint{{at: start.UnixMilli(), target: "sia"}}, start, end, loc)
	want := 30 * time.Minute.Milliseconds()
	if len(days) != 2 || days[0].Targets["sia"] != want || days[1].Targets["sia"] != want {
		t.Fatalf("midnight should split at 20:30 UTC: %+v", days)
	}
}

type disconnectedCloud struct {
	fakeCloud
	offline bool
}

func (c *disconnectedCloud) Lookup(ctx context.Context, token string, g Group) (DNSRecord, error) {
	if c.offline {
		return DNSRecord{}, errors.New("Cloudflare lookup: connection or response timed out")
	}
	return c.fakeCloud.Lookup(ctx, token, g)
}

func TestCloudIssuePersistsUntilVerifiedAndQueuesOneAlert(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir(), Sources{Snapshot: func(context.Context, []int64) (map[int64]Monitor, error) {
		return map[int64]Monitor{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.repo.save(ctx, testGroup())
	check(err)
	token := "placeholder"
	check(s.repo.setSettings(ctx, &token, false))
	cloud := &disconnectedCloud{fakeCloud: fakeCloud{record: DNSRecord{ID: "id", Content: g.Members[0].Target}}, offline: true}
	s.dns = cloud
	check(s.tick(ctx))
	if got := s.views[g.ID]; got.LastError == "" || got.Cloud.Operation != "lookup" || got.Cloud.RetryAt <= got.Cloud.FailedAt || !got.ErrorAlerts {
		t.Fatalf("missing actionable issue: %+v", got)
	}
	s.state[g.ID].retry = time.Time{}
	check(s.tick(ctx))
	jobs, err := s.repo.deliveries(ctx)
	check(err)
	if len(jobs) != len(g.Channels) {
		t.Fatalf("expected one delivery per configured channel, got %d", len(jobs))
	}
	h, err := s.repo.cloudHealth(ctx, g)
	check(err)
	if h.Operation != "lookup" || h.Message == "" {
		t.Fatalf("missing durable issue: %+v", h)
	}
	cloud.offline = false
	s.state = map[int64]*runtime{}
	check(s.tick(ctx))
	if got := s.views[g.ID]; got.LastError != "" || got.Cloud.VerifiedAt == 0 {
		t.Fatalf("issue did not clear after successful lookup: %+v", got)
	}
	cloud.offline = true
	s.state[g.ID].checked = time.Time{}
	check(s.tick(ctx))
	jobs, err = s.repo.deliveries(ctx)
	check(err)
	if len(jobs) != 2*len(g.Channels) {
		t.Fatalf("new issue should send new alerts, got %d", len(jobs))
	}
}
