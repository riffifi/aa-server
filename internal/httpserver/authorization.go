package httpserver

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type authorizedKey struct{}

func scoped(r *http.Request) bool {
	value, _ := r.Context().Value(authorizedKey{}).(bool)
	return value
}
func identity(r *http.Request) string {
	claims, _ := r.Context().Value(principalKey{}).(map[string]any)
	value, _ := claims["mirea"].(string)
	return value
}

// ServeHTTP is the production boundary. Contract tests may exercise individual
// handlers through serveHTTP; network listeners always use this strict policy.
func (s *Server) serveAuthorized(w http.ResponseWriter, r *http.Request) {
	if RetiredClientPath(r.URL.Path) {
		detail(w, 404, "Not Found")
		return
	}
	if r.URL.Path == "/tg/connect" || r.URL.Path == "/site/telegram-link" || (strings.HasPrefix(r.URL.Path, "/api/accounts/") && strings.HasSuffix(r.URL.Path, "/pass-request")) {
		detail(w, 404, "Not Found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(r.URL.Path, "/panel/api/") && r.URL.Path != "/panel/api/login" {
		if s.config.PanelLocalOnly && r.Header.Get("X-AA-Panel") != "1" {
			detail(w, 404, "Not Found")
			return
		}
		if _, ok := s.panel(w, r, r.Method != "GET" && r.Method != "HEAD"); !ok {
			return
		}
	}
	// Release metadata is public, like /health; no account or configuration data.
	if r.URL.Path != "/api/version" && (strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/site/")) {
		var claims map[string]any
		if header := r.Header.Get("Authorization"); header != "" {
			scheme, token, ok := strings.Cut(header, " ")
			if ok && strings.EqualFold(scheme, "bearer") && token != "" && s.config.Verify != nil {
				verified, err := s.config.Verify(r.Context(), token)
				if err == nil {
					claims = verified
				}
			}
			// An explicitly invalid bearer must not fall back to a browser cookie.
		} else {
			session, err := s.config.Vault.Session(r.Context(), "site", cookie(r, "aa_site"))
			if err != nil {
				s.failure(w, err)
				return
			}
			if session != nil {
				claims = map[string]any{"sub": session["aaid_id"], "mirea": session["mirea"], "grp": session["group_id"], "group_name": session["group_name"], "sid": session["aaid_session_id"]}
			}
		}
		mirea, _ := claims["mirea"].(string)
		if mirea != "" && s.config.RequireIdentityState {
			sub, _ := claims["sub"].(string)
			sid, _ := claims["sid"].(string)
			current, err := s.config.Vault.VerifiedIdentity(r.Context(), sub, mirea, sid)
			if err != nil {
				s.failure(w, err)
				return
			}
			if current == nil {
				mirea = ""
			} else {
				claims["grp"] = current["group_id"]
				claims["group_name"] = current["group_name"]
			}
		}
		if mirea == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.Header().Set("X-AAID-Required", "1")
			write(w, 401, map[string]any{"detail": "AA ID sign-in required", "aaid_url": s.config.AAIDURL})
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, claims))
		r = r.WithContext(context.WithValue(r.Context(), authorizedKey{}, true))
		count := 300
		window := time.Minute
		bucket := "api:" + mirea
		if r.Method != "GET" && r.Method != "HEAD" {
			count = 60
			bucket = "api-write:" + mirea
		}
		if s.limited(w, bucket, count, window) {
			w.Header().Set("Retry-After", "60")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
			if !s.authorizeAccount(w, r, parts[0]) {
				return
			}
		}
	}
	s.serveHTTP(w, r)
}
func (s *Server) authorizeAccount(w http.ResponseWriter, r *http.Request, id string) bool {
	var allowed bool
	query := `SELECT EXISTS(SELECT 1 FROM aa.accounts a WHERE a.id=$1 AND a.pulse_id=$2)`
	if err := s.config.Vault.Pool.QueryRow(r.Context(), query, id, identity(r)).Scan(&allowed); err != nil {
		s.failure(w, err)
		return false
	}
	if !allowed {
		detail(w, 404, "Account not found")
	}
	return allowed
}
