package xdns

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

const (
	Down    = "down"
	Slow    = "slow"
	Recover = "recover"
	Route   = "route"
	Failure = "error"
	AllDown = "all_down"
)

type Member struct {
	MonitorID int64  `json:"monitorId"`
	Target    string `json:"target"`
	Priority  int    `json:"priority"`
}

type Group struct {
	ID              int64    `json:"id"`
	Revision        int64    `json:"revision"`
	Name            string   `json:"name"`
	Zone            string   `json:"zone"`
	RecordName      string   `json:"recordName"`
	Domain          string   `json:"domain"`
	Kind            string   `json:"kind"`
	TTL             int      `json:"ttl"`
	Proxied         bool     `json:"proxied"`
	Enabled         bool     `json:"enabled"`
	DownSeconds     int      `json:"downSeconds"`
	SlowMs          int      `json:"slowMs"`
	SlowSeconds     int      `json:"slowSeconds"`
	RecoverSeconds  int      `json:"recoverSeconds"`
	CooldownSeconds int      `json:"cooldownSeconds"`
	KeepLast        bool     `json:"keepLast"`
	Members         []Member `json:"members"`
	Notify          bool     `json:"notify"`
	Channels        []int64  `json:"channels"`
	Events          []string `json:"events"`
}

func (g Group) Record() string {
	if g.RecordName == "@" {
		return g.Domain
	}
	return g.RecordName + "." + g.Domain
}

var (
	zonePattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{20,128}$`)
)

func dnsName(value string) (string, error) {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil || len(ascii) > 253 {
		return "", errors.New("invalid DNS name")
	}
	ascii = strings.ToLower(ascii)
	for _, part := range strings.Split(ascii, ".") {
		if !labelPattern.MatchString(part) {
			return "", errors.New("invalid DNS label")
		}
	}
	return ascii, nil
}

func normalizeTarget(kind, value string) (string, error) {
	if kind == "A" {
		ip, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil || !ip.Is4() {
			return "", errors.New("an A record requires an IPv4 address")
		}
		return ip.String(), nil
	}
	host, err := dnsName(value)
	if err != nil || !strings.Contains(host, ".") {
		return "", errors.New("a CNAME target requires a full DNS name")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", errors.New("a CNAME target cannot be an IP address")
	}
	return host, nil
}

func (g *Group) normalize() error {
	g.Name = strings.TrimSpace(g.Name)
	g.Zone = strings.ToLower(strings.TrimSpace(g.Zone))
	g.Kind = strings.ToUpper(strings.TrimSpace(g.Kind))
	g.RecordName = strings.TrimSpace(g.RecordName)
	if g.Name == "" || len([]rune(g.Name)) > 80 || strings.ContainsAny(g.Name, "\r\n") {
		return errors.New("group name must be 1–80 characters on one line")
	}
	if !zonePattern.MatchString(g.Zone) {
		return errors.New("Zone ID must contain 32 hexadecimal characters")
	}
	var err error
	g.Domain, err = dnsName(g.Domain)
	if err != nil || !strings.Contains(g.Domain, ".") {
		return errors.New("enter the full domain name")
	}
	if g.RecordName != "@" {
		g.RecordName, err = dnsName(g.RecordName)
		if err != nil || g.RecordName == g.Domain || strings.HasSuffix(g.RecordName, "."+g.Domain) || len(g.Record()) > 253 {
			return errors.New("enter a relative record name, or @ for the zone apex")
		}
	}
	if g.Kind != "A" && g.Kind != "CNAME" {
		return errors.New("record type must be A or CNAME")
	}
	if g.Proxied {
		g.TTL = 1
	}
	if g.TTL != 1 && (g.TTL < 30 || g.TTL > 86400) {
		return errors.New("TTL must be 1 (Auto) or 30–86400 seconds")
	}
	for _, sec := range []int{g.DownSeconds, g.SlowSeconds, g.RecoverSeconds, g.CooldownSeconds} {
		if sec < 0 || sec > 86400 {
			return errors.New("durations must be 0–86400 seconds")
		}
	}
	if g.SlowMs < 0 || g.SlowMs > 600000 {
		return errors.New("latency threshold must be 0–600000 ms")
	}
	if len(g.Members) < 2 || len(g.Members) > 20 {
		return errors.New("use 2–20 members")
	}
	monitors, targets := map[int64]bool{}, map[string]bool{}
	for i := range g.Members {
		m := &g.Members[i]
		if m.MonitorID < 1 || monitors[m.MonitorID] || m.Priority < 1 || m.Priority > 10000 {
			return errors.New("select distinct monitors and use priorities of 1–10000")
		}
		m.Target, err = normalizeTarget(g.Kind, m.Target)
		if err != nil {
			return fmt.Errorf("member %d: %w", i+1, err)
		}
		if targets[m.Target] || m.Target == g.Record() {
			return errors.New("targets must be distinct and cannot point to the managed record")
		}
		monitors[m.MonitorID], targets[m.Target] = true, true
	}
	slices.SortFunc(g.Members, func(a, b Member) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		if a.MonitorID < b.MonitorID {
			return -1
		}
		if a.MonitorID > b.MonitorID {
			return 1
		}
		return 0
	})
	if len(g.Channels) > 20 || len(g.Events) > 6 {
		return errors.New("too many notification choices")
	}
	slices.Sort(g.Channels)
	g.Channels = slices.Compact(g.Channels)
	slices.Sort(g.Events)
	g.Events = slices.Compact(g.Events)
	for _, event := range g.Events {
		if !slices.Contains([]string{Down, Slow, Recover, Route, Failure, AllDown}, event) {
			return errors.New("unknown notification event")
		}
	}
	if g.Notify && (len(g.Channels) == 0 || len(g.Events) == 0) {
		return errors.New("select notification channels and events")
	}
	if g.Channels == nil {
		g.Channels = []int64{}
	}
	if g.Events == nil {
		g.Events = []string{}
	}
	return nil
}

type Beat struct {
	Time   int64
	Status int
	Ping   *float64
}

type Monitor struct {
	Name      string
	Available bool
	MaxGap    time.Duration
	// Beats run oldest to newest. Include the previous beat on each refresh
	// when possible so loss of continuity is detectable.
	Beats []Beat
}

type Health struct {
	Member
	Name  string   `json:"name"`
	State string   `json:"state"`
	Ping  *float64 `json:"ping"`
	Since int64    `json:"since"`
	At    int64    `json:"at"`
}

func observe(m Member, g Group, source Monitor, prev Health, now time.Time) Health {
	h := Health{Member: m, Name: source.Name, State: "unknown"}
	if !source.Available || len(source.Beats) == 0 {
		return h
	}
	latest := source.Beats[len(source.Beats)-1]
	if age := now.Sub(time.UnixMilli(latest.Time)); age > source.MaxGap || age < -5*time.Second {
		return h
	}
	previous := false
	for _, beat := range source.Beats {
		if beat.Time == prev.At {
			previous = true
		}
	}
	if previous {
		h.State, h.At, h.Since, h.Ping = prev.State, prev.At, prev.Since, prev.Ping
	}
	for _, beat := range source.Beats {
		if beat.Time <= h.At {
			continue
		}
		state := "unknown"
		ping := beat.Ping
		if ping != nil && (*ping < 0 || math.IsNaN(*ping) || math.IsInf(*ping, 0)) {
			ping = nil
		}
		switch beat.Status {
		case 0:
			state, ping = Down, nil
		case 1:
			state = "up"
			if g.SlowMs > 0 {
				if ping == nil {
					state = "unknown"
				} else if *ping > float64(g.SlowMs) {
					state = Slow
				}
			}
		}
		if state != h.State || h.Since == 0 || time.Duration(beat.Time-h.At)*time.Millisecond > source.MaxGap {
			h.Since = beat.Time
		}
		h.State, h.At, h.Ping = state, beat.Time, ping
	}
	return h
}

func held(h Health, seconds int) bool {
	return h.Since > 0 && h.At-h.Since >= int64(seconds)*1000
}

func better(a, b Health) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.Ping != nil && (b.Ping == nil || *a.Ping < *b.Ping)
}

type Decision struct{ Target, Reason string }

func choose(g Group, current string, members []Health) Decision {
	active := Health{State: "unknown"}
	allDown := true
	for _, member := range members {
		if member.Target == current {
			active = member
		}
		allDown = allDown && member.State == Down && held(member, g.DownSeconds)
	}
	if allDown {
		if !g.KeepLast && current != members[0].Target {
			return Decision{members[0].Target, AllDown}
		}
		return Decision{Reason: AllDown}
	}
	reason := ""
	switch active.State {
	case Down:
		if held(active, g.DownSeconds) {
			reason = Down
		}
	case Slow:
		if held(active, g.SlowSeconds) {
			reason = Slow
		}
	case "up":
		reason = Recover
	default:
		if active.MonitorID == 0 {
			reason = Route
		}
	}
	if reason == "" {
		return Decision{}
	}
	var best Health
	for _, member := range members {
		if member.Target == current || member.State != "up" {
			continue
		}
		if (reason == Recover || reason == Route) && !held(member, g.RecoverSeconds) {
			continue
		}
		if reason == Recover && !better(member, active) {
			continue
		}
		if best.MonitorID == 0 || better(member, best) {
			best = member
		}
	}
	if best.MonitorID == 0 {
		return Decision{}
	}
	return Decision{best.Target, reason}
}
