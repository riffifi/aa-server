// Package identityserver implements the native AA ID issuer and device APIs.
package identityserver

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/buildinfo"
	"aa-server/internal/cryptocompat"
	"aa-server/internal/httpserver"
	"aa-server/internal/postgres"
	"aa-server/internal/schedule"
	"aa-server/internal/security"
)

type Config struct {
	Store                                 *postgres.Identity
	Seed                                  []byte
	Issuer, Audience, AdminToken, SiteURL string
	SessionsURL, StaticDir                string
	HTTP                                  *http.Client
	AccessTTL, SessionTTL                 int64
	MaxSessions                           int
	TrustProxy                            bool

	Calendars *schedule.Cache
	Logins    *browser.Jobs
	PublicURL string
	WebTTL    int64
}
type code struct {
	Expires   time.Time
	UserID    string
	SessionID string
}
type bucket struct {
	Times  []time.Time
	Window time.Duration
}
type Server struct {
	admission security.Gate
	config    Config
	mux       *http.ServeMux
	public    ed25519.PublicKey
	kid       string
	mu        sync.Mutex
	attempts  map[string]bucket
	codes     map[string]code
	cancel    context.CancelFunc
	done      chan struct{}

	registered    []httpserver.Route
	ownedHTTP     bool
	ownedCalendar bool
	ownedLogins   bool
	ownedBrowser  *browser.Engine
}

func New(ctx context.Context, c Config) (*Server, error) {
	if c.Store == nil || c.Store.Pool == nil || c.Store.Now == nil {
		return nil, errors.New("initialized AA ID store is required")
	}
	if len(c.Seed) != ed25519.SeedSize {
		return nil, errors.New("preserved 32-byte signing seed is required")
	}
	c.Seed = append([]byte(nil), c.Seed...)
	if c.Issuer == "" {
		c.Issuer = "aa-id"
	}
	if c.Audience == "" {
		c.Audience = "aa"
	}
	if c.AccessTTL == 0 {
		c.AccessTTL = 600
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 60 * 86400
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = 2
	}
	if c.WebTTL == 0 {
		c.WebTTL = 30 * 86400
	}
	if c.WebTTL < 600 {
		return nil, errors.New("web session TTL must be at least 600 seconds")
	}
	if c.AccessTTL < 60 || c.SessionTTL < 3600 || c.MaxSessions < 1 {
		return nil, errors.New("invalid AA ID session limits")
	}
	c.SiteURL = strings.TrimRight(strings.TrimSpace(c.SiteURL), "/")
	public := ed25519.NewKeyFromSeed(c.Seed).Public().(ed25519.PublicKey)
	server := &Server{config: c, mux: http.NewServeMux(), public: public, kid: cryptocompat.KeyID(public), attempts: map[string]bucket{}, codes: map[string]code{}, done: make(chan struct{})}
	if server.config.SessionsURL == "" {
		server.config.SessionsURL = "http://sessions:8080"
	}
	server.config.SessionsURL = strings.TrimRight(server.config.SessionsURL, "/")
	if server.config.HTTP == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = 32
		transport.MaxConnsPerHost = 64
		server.config.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		server.ownedHTTP = true
	}
	maintenance, cancel := context.WithCancel(ctx)
	if server.config.Logins == nil {
		server.ownedBrowser = browser.New(maintenance, browser.Config{Identity: true, LoginSlots: 4})
		server.config.Logins = browser.NewJobs(maintenance, server.ownedBrowser)
		server.ownedLogins = true
	}
	if server.config.Calendars == nil {
		server.config.Calendars = schedule.NewCache(maintenance, nil)
		server.config.Calendars.Now = c.Store.Now
		server.config.Calendars.Retry = 0
		server.ownedCalendar = true
	}
	server.cancel = cancel
	go func() {
		defer close(server.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-maintenance.Done():
				return
			case <-ticker.C:
				server.prune()
			}
		}
	}()
	server.routes()
	return server, nil
}
func (s *Server) Close() {
	s.cancel()
	<-s.done
	if s.ownedLogins {
		s.config.Logins.Close()
	}
	if s.ownedBrowser != nil {
		s.ownedBrowser.Close()
	}
	if s.ownedCalendar {
		s.config.Calendars.Close()
	}
	if s.ownedHTTP {
		s.config.HTTP.CloseIdleConnections()
	}
}
func (s *Server) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.config.Store.Now()
	for key, b := range s.attempts {
		if len(b.Times) == 0 || b.Times[len(b.Times)-1].Before(now.Add(-b.Window)) {
			delete(s.attempts, key)
		}
	}
	for key, c := range s.codes {
		if c.Expires.Before(now) {
			delete(s.codes, key)
		}
	}
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func detail(w http.ResponseWriter, status int, message string) {
	write(w, status, map[string]any{"detail": message})
}
func failure(w http.ResponseWriter, err error) {
	var api *apiError
	if errors.As(err, &api) {
		detail(w, api.Status, api.Detail)
	} else if errors.Is(err, postgres.ErrBlocked) {
		detail(w, 403, "This AA ID is blocked")
	} else {
		detail(w, 500, "Internal Server Error")
	}
}
func (s *Server) limit(w http.ResponseWriter, r *http.Request, key string, count int, window time.Duration) bool {
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	if s.config.TrustProxy {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
			address = ip.String()
		}
	}
	key += "|" + address
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.attempts[key]
	b.Window = window
	now := s.config.Store.Now()
	if _, exists := s.attempts[key]; !exists && len(s.attempts) >= 8192 {
		for candidate, value := range s.attempts {
			if len(value.Times) == 0 || value.Times[len(value.Times)-1].Before(now.Add(-value.Window)) {
				delete(s.attempts, candidate)
			}
		}
		if len(s.attempts) >= 8192 {
			detail(w, 429, "Server is busy; retry later")
			return true
		}
	}
	for len(b.Times) > 0 && b.Times[0].Before(now.Add(-window)) {
		b.Times = b.Times[1:]
	}
	if len(b.Times) >= count {
		s.attempts[key] = b
		detail(w, 429, "Too many attempts; try again later")
		return true
	}
	b.Times = append(b.Times, now)
	s.attempts[key] = b
	return false
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.admission.Try() {
		w.Header().Set("Retry-After", "1")
		detail(w, 429, "Server is busy; retry shortly")
		return
	}
	defer s.admission.Leave()
	if r.ContentLength > 100000 {
		detail(w, 413, "Request too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 100000))
	if err != nil {
		detail(w, 413, "Request too large")
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if strings.HasPrefix(r.URL.Path, "/web/") && r.Method != "GET" && r.Method != "HEAD" {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.User != nil || u.Host != r.Host {
				detail(w, 403, "Invalid origin")
				return
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, pattern := s.mux.Handler(r)
	if pattern == "GET /" && r.URL.Path != "/" {
		pattern = ""
	}
	if pattern == "" {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
			candidate := r.Clone(r.Context())
			candidate.Method = method
			_, match := s.mux.Handler(candidate)
			if match != "" && (match != "GET /" || r.URL.Path == "/") {
				w.Header().Set("Allow", method)
				detail(w, 405, "Method Not Allowed")
				return
			}
		}
		detail(w, 404, "Not Found")
		return
	}
	if r.Method == "HEAD" && !strings.HasPrefix(r.URL.Path, "/static/") {
		w.Header().Set("Allow", "GET")
		detail(w, 405, "Method Not Allowed")
		return
	}
	s.mux.ServeHTTP(w, r)
}
func public(user map[string]any) map[string]any {
	out := map[string]any{}
	for _, field := range []string{"id", "username", "group_id", "group_name", "created", "verified_at"} {
		out[field] = user[field]
	}
	return out
}
func (s *Server) grant(user, session map[string]any, refresh string) (map[string]any, error) {
	now := s.config.Store.Now().Unix()
	claims := map[string]any{"iss": s.config.Issuer, "aud": s.config.Audience, "sub": user["id"], "sid": session["id"], "mirea": user["pulse_uid"], "grp": user["group_id"], "group_name": user["group_name"], "usr": user["username"], "iat": now, "exp": now + s.config.AccessTTL}
	token, err := cryptocompat.Sign(s.config.Seed, claims)
	if err != nil {
		return nil, err
	}
	return map[string]any{"access_token": token, "access_expires_at": now + s.config.AccessTTL, "refresh_token": refresh, "session_id": session["id"], "user": public(user)}, nil
}
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (map[string]any, map[string]any, bool) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	var claims map[string]any
	if strings.EqualFold(scheme, "bearer") {
		claims, _ = cryptocompat.Verify(token, map[string]ed25519.PublicKey{s.kid: s.public}, s.config.Issuer, s.config.Audience, s.config.Store.Now(), false)
	}
	if claims != nil {
		sid, _ := claims["sid"].(string)
		session, err := s.config.Store.ActiveSession(r.Context(), sid)
		if err != nil {
			failure(w, err)
			return nil, nil, false
		}
		if session == nil || session["user_id"] != claims["sub"] {
			claims = nil
		}
	}
	if claims == nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		detail(w, 401, "Sign in to AA ID")
		return nil, nil, false
	}
	user, err := s.config.Store.User(r.Context(), claims["sub"].(string))
	if err != nil {
		failure(w, err)
		return nil, nil, false
	}
	if user == nil || user["status"] != "active" {
		detail(w, 403, "This AA ID is blocked")
		return nil, nil, false
	}
	return claims, user, true
}
func (s *Server) handle(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
	method, path, _ := strings.Cut(pattern, " ")
	s.registered = append(s.registered, httpserver.Route{Method: method, Path: path})
}
func NativeRoutes() []httpserver.Route {
	server := &Server{mux: http.NewServeMux(), config: Config{StaticDir: "."}}
	server.routes()
	return server.registered
}
func (s *Server) routes() {
	s.handle("POST /v1/auth/logins", s.webLogin)
	s.handle("GET /v1/auth/logins/{identifier}", s.webLoginState)
	s.handle("POST /v1/auth/logins/{identifier}/claim", s.webLoginClaim)
	s.handle("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, buildinfo.Current("aa-id"))
	})
	s.handle("GET /health", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"ok": true, "service": "aa-id"})
	})
	s.handle("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"issuer": s.config.Issuer, "audience": s.config.Audience, "keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(s.public), "kid": s.kid, "alg": "EdDSA", "use": "sig"}}})
	})
	s.handle("POST /v1/auth/refresh", s.refresh)
	s.handle("POST /v1/auth/logout", s.logout)
	s.handle("GET /v1/me", s.me)
	s.handle("GET /web/schedule", s.webSchedule)
	s.handle("GET /v1/me/sessions", s.sessions)
	s.handle("DELETE /v1/me/sessions/{session}", s.revoke)
	s.handle("PUT /v1/admin/users/{user}/status", s.status)
	s.webRoutes()
}
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	body, ok := httpserver.DecodeBody(w, r, []httpserver.Field{{Name: "refresh_token", Kind: "string", Required: true, Min: 20, Max: 200}})
	if !ok {
		return
	}
	if s.limit(w, r, "refresh", 60, time.Minute) {
		return
	}
	session, token, err := s.config.Store.Rotate(r.Context(), body["refresh_token"].(string), float64(s.config.SessionTTL))
	if err != nil {
		failure(w, err)
		return
	}
	if session == nil {
		detail(w, 401, "Sign in to AA ID again")
		return
	}
	user, err := s.config.Store.User(r.Context(), session["user_id"].(string))
	if err != nil {
		failure(w, err)
		return
	}
	if user == nil || user["status"] != "active" {
		detail(w, 403, "This AA ID is blocked")
		return
	}
	grant, err := s.grant(user, session, token)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, grant)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	claims, user, ok := s.caller(w, r)
	if !ok {
		return
	}
	if _, err := s.config.Store.Revoke(r.Context(), user["id"].(string), claims["sid"].(string), "logout"); err != nil {
		failure(w, err)
		return
	}
	write(w, 200, map[string]any{"ok": true})
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	claims, user, ok := s.caller(w, r)
	if !ok {
		return
	}
	out := public(user)
	out["session_id"] = claims["sid"]
	out["max_sessions"] = s.config.MaxSessions
	write(w, 200, out)
}
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	claims, user, ok := s.caller(w, r)
	if !ok {
		return
	}
	rows, err := s.config.Store.Sessions(r.Context(), user["id"].(string))
	if err != nil {
		failure(w, err)
		return
	}
	for _, row := range rows {
		row["current"] = row["id"] == claims["sid"]
	}
	write(w, 200, map[string]any{"sessions": rows, "max_sessions": s.config.MaxSessions})
}
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	_, user, ok := s.caller(w, r)
	if !ok {
		return
	}
	changed, err := s.config.Store.Revoke(r.Context(), user["id"].(string), r.PathValue("session"), "owner")
	if err != nil {
		failure(w, err)
		return
	}
	if !changed {
		detail(w, 404, "Session not found")
		return
	}
	write(w, 200, map[string]any{"ok": true})
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	body, ok := httpserver.DecodeBody(w, r, []httpserver.Field{{Name: "status", Kind: "string", Required: true, Pattern: "^(active|blocked)$"}})
	if !ok {
		return
	}
	if len(s.config.AdminToken) < 24 || !hmac.Equal([]byte(r.Header.Get("X-Admin-Token")), []byte(s.config.AdminToken)) {
		detail(w, 403, "Forbidden")
		return
	}
	changed, err := s.config.Store.SetStatus(r.Context(), r.PathValue("user"), body["status"].(string))
	if err != nil {
		failure(w, err)
		return
	}
	if !changed {
		detail(w, 404, "User not found")
		return
	}
	write(w, 200, map[string]any{"ok": true})
}
