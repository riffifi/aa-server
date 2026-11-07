package httpserver

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aa-server/internal/postgres"
)

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if !scoped(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.Header().Set("X-AAID-Required", "1")
		write(w, 401, map[string]any{"detail": "AA ID sign-in required", "aaid_url": s.config.AAIDURL})
		return
	}
	s.visibleAccounts(w, r)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if scoped(r) {
		key := identity(r)
		s.attemptsMu.Lock()
		if s.activeEvents == nil {
			s.activeEvents = map[string]int{}
		}
		if s.activeEvents[key] >= 2 {
			s.attemptsMu.Unlock()
			w.Header().Set("Retry-After", "1")
			detail(w, 429, "Too many concurrent event polls")
			return
		}
		s.activeEvents[key]++
		s.attemptsMu.Unlock()
		defer func() {
			s.attemptsMu.Lock()
			s.activeEvents[key]--
			if s.activeEvents[key] == 0 {
				delete(s.activeEvents, key)
			}
			s.attemptsMu.Unlock()
		}()
	}
	after, ok := integer(w, r, "after", 0)
	if !ok {
		return
	}
	wait, ok := integer(w, r, "wait", 25)
	if !ok {
		return
	}
	if after < 0 || wait < 0 || wait > 25 {
		detail(w, 400, "Invalid event cursor")
		return
	}
	gid, ok := group(w, r)
	if !ok {
		return
	}
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	for {
		signal := s.subscription()
		var values []map[string]any
		var cursor int64
		var err error
		if scoped(r) {
			values, cursor, err = s.config.Vault.VisibleEvents(r.Context(), identity(r), after, gid)
		} else {
			values, cursor, err = s.config.Vault.Events(r.Context(), after, gid)
		}
		if err != nil {
			s.failure(w, err)
			return
		}
		if len(values) > 0 || cursor != after || wait == 0 {
			write(w, 200, map[string]any{"events": values, "cursor": cursor})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			detail(w, 503, "Service unavailable")
			return
		case <-deadline.C:
			write(w, 200, map[string]any{"events": []any{}, "cursor": cursor})
			return
		case <-signal:
		}
	}
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	row, err := s.config.Vault.MetadataAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return
	}
	if row == nil {
		detail(w, 404, "Account not found")
		return
	}
	allowed, err := s.owner(r, r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return
	}
	if !allowed {
		detail(w, 401, "Sign in to this account first")
		return
	}
	write(w, 200, postgres.Public(row))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	password, ok := stringField(w, body, "password", 1, 256)
	if !ok {
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || u.Host != r.Host {
			detail(w, 403, "Invalid origin")
			return
		}
	}
	now := s.config.Vault.Now()
	host := remoteHost(r)
	s.attemptsMu.Lock()
	if _, exists := s.attempts[host]; !exists && len(s.attempts) >= 8192 {
		s.attemptsMu.Unlock()
		detail(w, 429, "Server is busy; retry later")
		return
	}
	queue := s.attempts[host]
	for len(queue) > 0 && queue[0].Before(now.Add(-time.Minute)) {
		queue = queue[1:]
	}
	if len(queue) >= 5 {
		s.attempts[host] = queue
		s.attemptsMu.Unlock()
		detail(w, 429, "Try again in a minute")
		return
	}
	s.attempts[host] = append(queue, now)
	s.attemptsMu.Unlock()
	token, csrf, err := s.config.Vault.AdminLogin(r.Context(), password)
	if err != nil {
		s.failure(w, err)
		return
	}
	if token == "" {
		detail(w, 401, "Wrong panel password")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "panel_session", Value: token, Path: "/panel", MaxAge: 43200, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
	write(w, 200, map[string]any{"csrf": csrf})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, true); !ok {
		return
	}
	if err := s.config.Vault.Logout(r.Context(), "panel", cookie(r, "panel_session")); err != nil {
		s.failure(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "panel_session", Value: "", Path: "/panel", MaxAge: -1})
	write(w, 200, map[string]any{"ok": true})
}

func (s *Server) panelState(w http.ResponseWriter, r *http.Request) {
	session, ok := s.panel(w, r, false)
	if !ok {
		return
	}
	v := s.config.Vault
	metadata := make([]map[string]any, 0)
	err := v.EachAccount(r.Context(), func(row map[string]any) error {
		data := postgres.Public(row)
		secret, err := v.Secret(row)
		if err != nil {
			return err
		}
		state := secret["storage_state"]
		hasState := state != nil
		if value, ok := state.(map[string]any); ok {
			hasState = len(value) > 0
		}
		if value, ok := state.([]any); ok {
			hasState = len(value) > 0
		}
		data["automatic_renewal"] = hasState
		data["renewed_at"] = secret["renewed_at"]
		data["page_seen_at"] = row["page_seen_at"]
		metadata = append(metadata, data)
		return nil
	})
	if err != nil {
		s.failure(w, err)
		return
	}
	cursor, err := v.Cursor(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"csrf": session["csrf"], "accounts": metadata, "cursor": cursor})
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	label, ok := stringField(w, body, "label", 1, 60)
	if !ok {
		return
	}
	version, ok := expected(w, body)
	if !ok {
		return
	}
	if !s.allowOwner(w, r) {
		return
	}
	row, err := s.config.Vault.Rename(r.Context(), r.PathValue("id"), label, version)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			s.failure(w, err)
		} else if _, ok := err.(*postgres.Conflict); ok {
			s.failure(w, err)
		} else if strings.TrimSpace(label) == "" {
			detail(w, 400, "Invalid name")
		} else {
			s.failure(w, err)
		}
		return
	}
	write(w, 200, row)
}

func (s *Server) passAllowed(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	allowed, ok := body["allowed"].(bool)
	if !ok {
		detail(w, 422, "Invalid allowed")
		return
	}
	version, ok := expected(w, body)
	if !ok {
		return
	}
	if !s.allowOwner(w, r) {
		return
	}
	row, err := s.config.Vault.SetPassAllowed(r.Context(), r.PathValue("id"), allowed, version)
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, row)
}
