package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neoauroraproject/Neo-Uptime/internal/store"
	"github.com/neoauroraproject/Neo-Uptime/internal/xdns"
)

func TestXDNSUsesNeoSessionAndMonitors(t *testing.T) {
	h := newHarness(t)
	stop, err := h.app.StartConfigRoutes(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	request := func(method, path, body string, csrf bool, want int) map[string]any {
		t.Helper()
		status, result := h.do(method, path, body, csrf)
		if status != want {
			t.Fatalf("%s %s: HTTP %d, wanted %d: %v", method, path, status, want, result)
		}
		return result
	}
	request("GET", "/api/xdns", "", false, 401)
	request("POST", "/api/auth/setup", `{"username":"admin","password":"long-enough-pw"}`, true, 200)
	initial := request("GET", "/api/xdns", "", false, 200)
	if initial["dryRun"] != true || initial["tokenConfigured"] != false {
		t.Fatal(initial)
	}
	request("POST", "/api/xdns/evaluate", "", false, 403)
	request("PUT", "/api/xdns/settings", `{"dryRun":false}`, true, 400)
	first := request("POST", "/api/monitors", `{"name":"sia","type":"push","interval":60}`, true, 200)
	second := request("POST", "/api/monitors", `{"name":"port","type":"push","interval":60}`, true, 200)
	group := xdns.Group{Name: "Tunnel", Zone: strings.Repeat("a", 32), RecordName: "tun", Domain: "example.net",
		Kind: "CNAME", TTL: 60, KeepLast: true, Members: []xdns.Member{
			{MonitorID: int64(first["id"].(float64)), Target: "sia.example.net", Priority: 1},
			{MonitorID: int64(second["id"].(float64)), Target: "port.example.net", Priority: 2},
		}}
	payload, _ := json.Marshal(group)
	saved := request("POST", "/api/xdns/groups", string(payload), true, 201)
	id := int64(saved["id"].(float64))
	request("POST", "/api/xdns/groups", string(payload), true, 409)
	request("GET", fmt.Sprintf("/api/xdns/groups/%d/history", id), "", false, 200)
	stats := request("GET", fmt.Sprintf("/api/xdns/groups/%d/insights?hours=24&tz=Asia/Tehran", id), "", false, 200)
	if len(stats["buckets"].([]any)) != 24 || stats["changes"] != float64(0) {
		t.Fatal("wrong empty insights overview")
	}
	request("POST", "/api/xdns/evaluate", "", true, 202)
	request("DELETE", fmt.Sprintf("/api/xdns/groups/%d?revision=99", id), "", true, 409)
	request("DELETE", fmt.Sprintf("/api/xdns/groups/%d?revision=1", id), "", true, 200)
	token := strings.Repeat("b", 40)
	settings := request("PUT", "/api/xdns/settings", fmt.Sprintf(`{"dryRun":true,"token":%q}`, token), true, 200)
	state := request("GET", "/api/xdns", "", false, 200)
	raw, _ := json.Marshal(state)
	if settings["tokenConfigured"] != true || strings.Contains(string(raw), token) {
		t.Fatal("write-only token contract violated")
	}
}

func TestXDNSDispatchUsesTheConfiguredNeoProvider(t *testing.T) {
	h := newHarness(t)
	events := make(chan map[string]any, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event map[string]any
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		events <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	channel := &store.Notification{Name: "Local delivery", Type: "webhook", Active: true,
		Config: map[string]string{"url": webhook.URL, "language": "fa", "contentType": "json"}}
	if err := h.app.db.SaveNotification(h.ctx, channel); err != nil {
		t.Fatal(err)
	}
	event := xdns.Event{GroupName: "تونل اصلی", Record: "tun.example.net", Before: "sia.example.net", After: "port.example.net",
		FromMonitor: "sia", ToMonitor: "port", Reason: xdns.Slow, Status: "changed", At: time.Now().UnixMilli()}
	if err := h.app.routeNotification(h.ctx, channel.ID, event); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-events:
		message, _ := received["msg"].(string)
		for _, part := range []string{"کندی", "تونل اصلی", "tun.example.net", "sia.example.net (sia)", "port.example.net (port)"} {
			if !strings.Contains(message, part) {
				t.Errorf("message missing %q: %s", part, message)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no notification delivered")
	}
}
