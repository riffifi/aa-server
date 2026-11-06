package httpserver

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/postgres"
)

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if scoped(r) {
		s.visibleAccounts(w, r)
		return
	}
	gid, ok := group(w, r)
	if !ok {
		return
	}
	v := s.config.Vault
	ctx := r.Context()
	rows, err := v.Metadata(ctx, gid)
	if err != nil {
		s.failure(w, err)
		return
	}
	claims, _ := ctx.Value(principalKey{}).(map[string]any)
	var own map[string]any
	shared := map[string]bool{}
	if claims != nil {
		mirea, _ := claims["mirea"].(string)
		own, err = v.MetadataByPulse(ctx, mirea)
		if err != nil {
			s.failure(w, err)
			return
		}
		if own != nil {
			shared[own["id"].(string)] = true
			extra, err := v.SharedMetadata(ctx, own["id"].(string))
			if err != nil {
				s.failure(w, err)
				return
			}
			for _, row := range extra {
				shared[row["id"].(string)] = true
			}
			if gid != nil {
				known := map[string]bool{}
				for _, row := range rows {
					known[row["id"].(string)] = true
				}
				for _, row := range append([]map[string]any{own}, extra...) {
					id := row["id"].(string)
					if !known[id] {
						rows = append(rows, row)
						known[id] = true
					}
				}
			}
		}
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row["id"].(string))
	}
	statuses, err := v.PassStatuses(ctx, ids, r.Header.Get("X-Pass-Client"))
	if err != nil {
		s.failure(w, err)
		return
	}
	site, err := v.Session(ctx, "site", cookie(r, "aa_site"))
	if err != nil {
		s.failure(w, err)
		return
	}
	daily := cryptocompat.Daily(cryptocompat.PassTestingKey([]byte(s.config.MasterKey)), v.Now())
	testing := r.Header.Get("X-Pass-Test-Key") != "" && cryptocompat.MatchDaily(r.Header.Get("X-Pass-Test-Key"), daily)
	metadata := make([]map[string]any, 0, len(rows))
	passes := make([]map[string]any, 0, len(rows))
	eligible := []string{}
	for _, row := range rows {
		id := row["id"].(string)
		state := statuses[id]
		data := postgres.Public(row)
		if state != "disabled" && state != "unavailable" && shared[id] {
			data["pass_access"] = "approved"
		} else {
			data["pass_access"] = state
		}
		data["own"] = claims != nil && row["pulse_id"] == claims["mirea"]
		metadata = append(metadata, data)
		siteOwn := site != nil && site["mirea"] == row["pulse_id"]
		if data["pass_allowed"] == true && (testing || siteOwn || shared[id] || state == "approved") {
			eligible = append(eligible, id)
		}
	}
	numbers, err := v.PassNumbers(ctx, eligible)
	if err != nil {
		s.failure(w, err)
		return
	}
	for _, row := range rows {
		id := row["id"].(string)
		passes = append(passes, map[string]any{"account_id": id, "number": numbers[id], "version": row["pass_version"]})
	}
	cursor, err := v.Cursor(ctx)
	if err != nil {
		s.failure(w, err)
		return
	}
	var testingExpires any
	if testing {
		testingExpires = daily.ExpiresAt
	}
	write(w, 200, map[string]any{"accounts": metadata, "passes": passes, "cursor": cursor, "protocol": 2, "sync": "automatic", "group_filter": gid,
		"pass_agreements_required": true, "pass_access_expires_at": float64(v.Now().UnixMicro())/1e6 + 300, "pass_testing_expires_at": testingExpires})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
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
	write(w, 200, map[string]any{"csrf": session["csrf"], "accounts": metadata, "cursor": cursor, "pass_testing": cryptocompat.Daily(cryptocompat.PassTestingKey([]byte(s.config.MasterKey)), v.Now())})
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
