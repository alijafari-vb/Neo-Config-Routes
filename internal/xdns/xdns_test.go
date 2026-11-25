package xdns

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testGroup() Group {
	return Group{Name: "Tunnel", Zone: strings.Repeat("a", 32), RecordName: "tun", Domain: "example.net",
		Kind: "CNAME", TTL: 60, Enabled: true, KeepLast: true, DownSeconds: 30, SlowMs: 120, SlowSeconds: 45,
		RecoverSeconds: 60, CooldownSeconds: 90, Notify: true, Channels: []int64{10, 20},
		Events:  []string{Down, Slow, Recover, Failure, AllDown},
		Members: []Member{{1, "sia.example.net", 1}, {2, "port.example.net", 2}, {3, "other.example.net", 2}},
	}
}

func makeHealth(g Group, i int, state string, seconds int, ping float64) Health {
	return Health{Member: g.Members[i], State: state, At: 1000000, Since: 1000000 - int64(seconds)*1000, Ping: &ping}
}

func TestPolicy(t *testing.T) {
	g := testGroup()
	d := func(i, elapsed int) Health { return makeHealth(g, i, Down, elapsed, 0) }
	u := func(i, elapsed int, ping float64) Health { return makeHealth(g, i, "up", elapsed, ping) }
	slow := func(elapsed int) Health { return makeHealth(g, 0, Slow, elapsed, 450) }
	for _, tc := range []struct {
		name, current string
		members       []Health
		want          Decision
	}{
		{"transient down", "sia.example.net", []Health{d(0, 29), u(1, 40, 20), u(2, 60, 10)}, Decision{}},
		{"select lowest ping at same priority", "sia.example.net", []Health{d(0, 30), u(1, 40, 20), u(2, 60, 10)}, Decision{"other.example.net", Down}},
		{"slow not yet sustained", "sia.example.net", []Health{slow(44), u(1, 30, 20), u(2, 15, 10)}, Decision{}},
		{"sustained slow", "sia.example.net", []Health{slow(45), u(1, 30, 20), u(2, 15, 10)}, Decision{"other.example.net", Slow}},
		{"recovery still warming", "port.example.net", []Health{u(0, 59, 100), u(1, 90, 20), u(2, 90, 45)}, Decision{}},
		{"recover preferred despite higher ping", "port.example.net", []Health{u(0, 60, 100), u(1, 90, 20), u(2, 90, 45)}, Decision{"sia.example.net", Recover}},
		{"stable when scores tie", "other.example.net", []Health{d(0, 90), u(1, 90, 20), u(2, 90, 20)}, Decision{}},
		{"all down keeps existing", "port.example.net", []Health{d(0, 40), d(1, 40), d(2, 40)}, Decision{Reason: AllDown}},
		{"unknown is not down", "sia.example.net", []Health{makeHealth(g, 0, "unknown", 0, 0), u(1, 90, 20), u(2, 90, 22)}, Decision{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := choose(g, tc.current, tc.members); got != tc.want {
				t.Fatalf("got %+v; want %+v", got, tc.want)
			}
		})
	}
	g.KeepLast = false
	if got := choose(g, "port.example.net", []Health{d(0, 40), d(1, 40), d(2, 40)}); got != (Decision{"sia.example.net", AllDown}) {
		t.Fatal(got)
	}
}

func TestHeartbeatContinuity(t *testing.T) {
	g := testGroup()
	m := g.Members[0]
	now := time.UnixMilli(500000)
	mon := Monitor{Available: true, MaxGap: 70 * time.Second, Beats: []Beat{{460000, 0, nil}, {480000, 0, nil}, {500000, 0, nil}}}
	h := observe(m, g, mon, Health{}, now)
	if h.State != Down || !held(h, 30) {
		t.Fatalf("continuous history ignored: %+v", h)
	}
	if h2 := observe(m, g, mon, h, now.Add(30*time.Second)); held(h2, 60) {
		t.Fatal("repeated evaluation advanced evidence")
	}
	if h2 := observe(m, g, mon, h, now.Add(71*time.Second)); h2.State != "unknown" {
		t.Fatal("old heartbeat treated as usable")
	}
	mon.Beats = []Beat{{500000, 0, nil}, {510000, 2, nil}, {520000, 0, nil}}
	if h2 := observe(m, g, mon, h, now.Add(20*time.Second)); h2.Since != 520000 || held(h2, 30) {
		t.Fatalf("missed transient: %+v", h2)
	}
	mon.Beats = []Beat{{500000, 0, nil}, {580000, 0, nil}}
	if h2 := observe(m, g, mon, h, now.Add(80*time.Second)); h2.Since != 580000 {
		t.Fatal("unexpected heartbeat gap preserved old evidence")
	}
}

type fakeCloud struct {
	record            DNSRecord
	writes            int
	timeoutAfterWrite bool
}

func (c *fakeCloud) Lookup(context.Context, string, Group) (DNSRecord, error) { return c.record, nil }
func (c *fakeCloud) Update(_ context.Context, _ string, _ Group, _ DNSRecord, target string) (DNSRecord, error) {
	c.writes++
	c.record.Content = target
	if c.timeoutAfterWrite {
		return DNSRecord{}, errors.New("Cloudflare request failed or timed out")
	}
	return c.record, nil
}

func TestEngineJournalDryRunCooldownAndNotifications(t *testing.T) {
	now := time.Now().UnixMilli()
	data := map[int64]Monitor{
		1: {Name: "sia", Available: true, MaxGap: 2 * time.Minute, Beats: []Beat{{now - 40000, 0, nil}, {now, 0, nil}}},
		2: {Name: "port", Available: true, MaxGap: 2 * time.Minute, Beats: []Beat{{now, 1, ptr(20.0)}}},
		3: {Name: "other", Available: true, MaxGap: 2 * time.Minute, Beats: []Beat{{now, 1, ptr(30.0)}}},
	}
	ctx := context.Background()
	dir := t.TempDir()
	src := Sources{Snapshot: func(context.Context, []int64) (map[int64]Monitor, error) { return data, nil }}
	s, err := Open(dir, src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.repo.save(ctx, testGroup())
	check(err)
	token := strings.Repeat("b", 32)
	check(s.repo.setSettings(ctx, &token, true))
	cf := &fakeCloud{record: DNSRecord{ID: strings.Repeat("1", 32), Content: "sia.example.net", TTL: 60}}
	s.dns = cf
	check(s.tick(ctx))
	check(s.tick(ctx))
	events, err := s.repo.history(ctx, g.ID, 0, 50)
	check(err)
	if cf.writes != 0 || len(events) != 1 || events[0].Status != "simulated" || s.views[g.ID].Simulated != "port.example.net" {
		t.Fatalf("unexpected dry run: %v %+v", cf.writes, events)
	}
	jobs, err := s.repo.deliveries(ctx)
	check(err)
	if len(jobs) != 0 {
		t.Fatal("dry run generated notification")
	}
	check(s.repo.setSettings(ctx, nil, false))
	s.settingsEpoch++
	cf.timeoutAfterWrite = true
	check(s.tick(ctx))
	if cf.writes != 1 {
		t.Fatalf("wanted a single update, got %d", cf.writes)
	}
	err = s.Close()
	check(err)
	// Startup reconciliation must recognize DNS applied before the old worker
	// received confirmation. It must not write a duplicate.
	s, err = Open(dir, src)
	check(err)
	s.dns = cf
	check(s.tick(ctx))
	events, err = s.repo.history(ctx, g.ID, 0, 50)
	check(err)
	if cf.writes != 1 || len(events) != 3 || events[1].Status != "changed" || events[1].After != "port.example.net" {
		t.Fatalf("reconciliation did not confirm the pending write: %v %+v", cf.writes, events)
	}
	jobs, err = s.repo.deliveries(ctx)
	check(err)
	if len(jobs) != 4 || jobs[0].Channel != 10 || jobs[1].Channel != 20 {
		t.Fatalf("unexpected delivery jobs: %+v", jobs)
	}
	data[1] = Monitor{Name: "sia", Available: true, MaxGap: 2 * time.Minute, Beats: []Beat{{now - 65000, 1, ptr(25.0)}, {now + 1, 1, ptr(25.0)}}}
	check(s.tick(ctx))
	if cf.writes != 1 || s.views[g.ID].Reason != "cooldown" {
		t.Fatalf("cooldown was lost after restart: %+v", s.views[g.ID])
	}
}

func ptr[T any](v T) *T { return &v }

func TestCloudflarePatchAndErrors(t *testing.T) {
	group := testGroup()
	group.Members[0].Target = "192.0.2.1"
	group.Members[1].Target = "192.0.2.2"
	group.Members[2].Target = "192.0.2.3"
	group.Kind = "A"
	if err := group.normalize(); err != nil {
		t.Fatal(err)
	}
	record := DNSRecord{ID: strings.Repeat("1", 32), Name: group.Record(), Kind: "A", Content: "192.0.2.1", TTL: 60}
	patches := 0
	token := strings.Repeat("a", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing bearer token")
		}
		if r.Method == "GET" {
			if r.URL.Query().Get("name.exact") != group.Record() || r.URL.Query().Get("type") != "A" {
				t.Error("incorrect filters")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []DNSRecord{record}})
			return
		}
		patches++
		if r.Method != "PATCH" || !strings.HasSuffix(r.URL.Path, "/dns_records/"+record.ID) {
			t.Error("unexpected update request")
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if len(in) != 3 || in["ttl"] != 60.0 || in["proxied"] != false {
			t.Errorf("incorrect patch: %v", in)
		}
		if in["content"] == "192.0.2.3" {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"Code": 10000, "Message": "Rejected " + token}}})
			return
		}
		record.Content, _ = in["content"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": record})
	}))
	defer srv.Close()
	cf := Cloudflare{base: srv.URL, client: srv.Client()}
	ctx := context.Background()
	got, err := cf.Lookup(ctx, token, group)
	if err != nil || got != record {
		t.Fatalf("incorrect lookup: %+v %v", got, err)
	}
	if _, err := cf.Update(ctx, token, group, got, got.Content); err != nil || patches != 0 {
		t.Fatal("sent an unnecessary patch")
	}
	got, err = cf.Update(ctx, token, group, got, "192.0.2.2")
	if err != nil || got.Content != "192.0.2.2" || patches != 1 {
		t.Fatalf("patch failed: %+v %v", got, err)
	}
	_, err = cf.Update(ctx, token, group, got, "192.0.2.3")
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "10000") {
		t.Fatalf("unsafe or unusable error: %v", err)
	}
}

func TestTokenStorageAndNotificationSelection(t *testing.T) {
	ctx := context.Background()
	s, err := openRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	token := strings.Repeat("b", 40)
	if err := s.setSettings(ctx, &token, true); err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := s.db.QueryRow("SELECT token FROM xdns_settings").Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), token) {
		t.Fatal("token saved in cleartext")
	}
	if got, dry, err := s.settings(ctx); got != token || !dry || err != nil {
		t.Fatal("token did not round trip")
	}
	g := testGroup()
	if got := channelsFor(g, Event{Reason: Slow, Status: "changed"}); len(got) != 2 {
		t.Fatal(got)
	}
	if got := channelsFor(g, Event{Reason: Route, Status: "changed"}); len(got) != 0 {
		t.Fatal(got)
	}
	if got := channelsFor(g, Event{Reason: Route, Status: "failed"}); len(got) != 2 {
		t.Fatal(got)
	}
	g.Notify = false
	if got := channelsFor(g, Event{Reason: Slow, Status: "changed"}); len(got) != 0 {
		t.Fatal(got)
	}
}
