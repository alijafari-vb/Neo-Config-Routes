package xdns

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func reply(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func problem(w http.ResponseWriter, code int, message string) {
	reply(w, code, map[string]string{"error": message})
}

func internal(w http.ResponseWriter, err error) {
	log.Printf("Config Routes storage: %v", err)
	problem(w, 500, "Config Routes storage is unavailable")
}

func body(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || !errors.Is(d.Decode(new(any)), io.EOF) {
		problem(w, 400, "invalid Config Routes request")
		return false
	}
	return true
}

func parseID(value string) int64 {
	n, _ := strconv.ParseInt(value, 10, 64)
	return n
}

func (s *Service) Mount(mux *http.ServeMux, admin func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/xdns", admin(s.list))
	mux.HandleFunc("PUT /api/xdns/settings", admin(s.settings))
	mux.HandleFunc("POST /api/xdns/groups", admin(s.save))
	mux.HandleFunc("PUT /api/xdns/groups/{id}", admin(s.save))
	mux.HandleFunc("DELETE /api/xdns/groups/{id}", admin(s.delete))
	mux.HandleFunc("GET /api/xdns/groups/{id}/history", admin(s.history))
	mux.HandleFunc("GET /api/xdns/groups/{id}/insights", admin(s.insights))
	mux.HandleFunc("POST /api/xdns/evaluate", admin(func(w http.ResponseWriter, r *http.Request) {
		s.Wake()
		reply(w, 202, map[string]bool{"queued": true})
	}))
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	groups, err := s.repo.groups(r.Context())
	if err != nil {
		internal(w, err)
		return
	}
	token, dry, err := s.repo.settings(r.Context())
	if err != nil {
		internal(w, err)
		return
	}
	views := make([]View, 0, len(groups))
	s.viewsMu.RLock()
	for _, g := range groups {
		v, ok := s.views[g.ID]
		if !ok || v.Revision != g.Revision {
			v = View{Group: g, Health: []Health{}}
		}
		if !dry {
			v.Simulated = ""
		}
		views = append(views, v)
	}
	s.viewsMu.RUnlock()
	reply(w, 200, map[string]any{"groups": views, "tokenConfigured": token != "", "dryRun": dry})
}

func (s *Service) settings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token  *string `json:"token"`
		DryRun *bool   `json:"dryRun"`
	}
	if !body(w, r, &input) {
		return
	}
	if input.DryRun == nil {
		problem(w, 400, "dryRun is required")
		return
	}
	if input.Token != nil {
		*input.Token = strings.TrimSpace(*input.Token)
		if *input.Token != "" && !tokenPattern.MatchString(*input.Token) {
			problem(w, 400, "invalid Cloudflare API token format")
			return
		}
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	token, _, err := s.repo.settings(r.Context())
	if err != nil {
		internal(w, err)
		return
	}
	if input.Token != nil {
		token = *input.Token
	}
	if !*input.DryRun && token == "" {
		problem(w, 400, "configure a Cloudflare API token before turning off Dry run")
		return
	}
	if err := s.repo.setSettings(r.Context(), input.Token, *input.DryRun); err != nil {
		internal(w, err)
		return
	}
	s.settingsEpoch++
	s.Wake()
	reply(w, 200, map[string]bool{"tokenConfigured": token != "", "dryRun": *input.DryRun})
}

func (s *Service) save(w http.ResponseWriter, r *http.Request) {
	var g Group
	if !body(w, r, &g) {
		return
	}
	g.ID = parseID(r.PathValue("id"))
	if r.Method == http.MethodPut && (g.ID <= 0 || g.Revision <= 0) {
		problem(w, 400, "group ID and revision are required")
		return
	}
	if err := g.normalize(); err != nil {
		problem(w, 400, err.Error())
		return
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	ctx := r.Context()
	if g.ID != 0 {
		old, err := s.repo.group(ctx, g.ID)
		if errors.Is(err, sql.ErrNoRows) {
			problem(w, 404, "group not found")
			return
		}
		if err != nil {
			internal(w, err)
			return
		}
		if old.Revision != g.Revision {
			problem(w, 409, errConflict.Error())
			return
		}
		if recordKey(old) != recordKey(g) || old.TTL != g.TTL || old.Proxied != g.Proxied {
			pending, err := s.repo.pending(ctx, old)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				internal(w, err)
				return
			}
			if pending.ID != 0 {
				problem(w, 409, "wait until Cloudflare confirms the pending update")
				return
			}
		}
	}
	if s.src.Validate != nil {
		if err := s.src.Validate(ctx, g); err != nil {
			problem(w, 400, err.Error())
			return
		}
	}
	saved, err := s.repo.save(ctx, g)
	switch {
	case errors.Is(err, errConflict), errors.Is(err, errDuplicate):
		problem(w, 409, err.Error())
	case err != nil:
		internal(w, err)
	default:
		s.Wake()
		code := 200
		if r.Method == http.MethodPost {
			code = 201
		}
		reply(w, code, saved)
	}
}

func (s *Service) delete(w http.ResponseWriter, r *http.Request) {
	id, rev := parseID(r.PathValue("id")), parseID(r.URL.Query().Get("revision"))
	if id <= 0 || rev <= 0 {
		problem(w, 400, "group ID and revision are required")
		return
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	ctx := r.Context()
	pending, err := s.repo.pending(ctx, Group{ID: id})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		internal(w, err)
		return
	}
	if pending.ID != 0 {
		problem(w, 409, "wait until Cloudflare confirms the pending update")
		return
	}
	if err := s.repo.remove(ctx, id, rev); err != nil {
		if errors.Is(err, errConflict) {
			problem(w, 409, err.Error())
			return
		}
		internal(w, err)
		return
	}
	s.viewsMu.Lock()
	delete(s.views, id)
	s.viewsMu.Unlock()
	reply(w, 200, map[string]bool{"deleted": true})
}

func (s *Service) history(w http.ResponseWriter, r *http.Request) {
	id, before := parseID(r.PathValue("id")), parseID(r.URL.Query().Get("before"))
	if id <= 0 || before < 0 {
		problem(w, 400, "invalid history cursor")
		return
	}
	events, err := s.repo.history(r.Context(), id, before, 26)
	if err != nil {
		internal(w, err)
		return
	}
	var next int64
	if len(events) > 25 {
		events = events[:25]
		next = events[24].ID
	}
	reply(w, 200, map[string]any{"events": events, "next": next})
}
