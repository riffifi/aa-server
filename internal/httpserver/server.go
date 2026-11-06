// Package httpserver contains native Go HTTP handlers under migration.
// It is not yet wired into the production Docker deployment.
package httpserver

import (
	"context"
	"crypto/hmac"
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
	"aa-server/internal/cryptocompat"
	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
	"aa-server/internal/schedule"
)

type Config struct {
	Vault                                    *postgres.Vault
	MasterKey                                string
	AAIDURL                                  string
	PublicURL                                string
	StaticDir                                string
	Verifier                                 *cryptocompat.Verifier
	Exchange                                 func(context.Context, string) (map[string]any, error)
	EnforceAAID, PanelLocalOnly              bool
	Verify                                   func(context.Context, string) (map[string]any, error)
	Pulse                                    Pulse
	Calendars                                *schedule.Cache
	SearchGroups                             func(context.Context, string) []map[string]any
	Browser                                  browser.LoginRunner
	Logins                                   *browser.Jobs
	RefreshEvery                             time.Duration
	RenewBefore, RenewInterval, RenewBackoff time.Duration
}
type Pulse interface {
	RPC(context.Context, string, string, []byte, string) (pulse.Reply, error)
	Identity(context.Context, string) (string, string)
	Logout(context.Context, string) error
}
type Server struct {
	site            *siteService
	diagnostics     diagnosticState
	config          Config
	mux             *http.ServeMux
	attemptsMu      sync.Mutex
	attempts        map[string][]time.Time
	signalMu        sync.Mutex
	signal          chan struct{}
	cancel          context.CancelFunc
	done            chan struct{}
	stateMu         sync.Mutex
	issued          map[string]map[string]int64
	ownedPulse      *pulse.Transport
	ownedCalendar   *schedule.Cache
	ownedBrowser    *browser.Engine
	ownedLogins     bool
	maintenanceDone chan struct{}
	registered      []Route
	attemptWindows  map[string]time.Duration
	refreshMu       sync.Mutex
	refreshing      map[string]*refreshFlight
	renewed         map[string]time.Time
	refreshWG       sync.WaitGroup
	refreshRoot     context.Context
}
type principalKey struct{}

func New(ctx context.Context, config Config) (*Server, error) {
	if config.RenewBefore <= 0 {
		config.RenewBefore = 15 * time.Minute
	}
	if config.RenewInterval <= 0 {
		config.RenewInterval = 15 * time.Minute
	}
	if config.RenewBackoff <= 0 {
		config.RenewBackoff = 5 * time.Minute
	}
	if config.Verifier != nil {
		if config.Verify == nil {
			config.Verify = func(ctx context.Context, token string) (map[string]any, error) {
				return config.Verifier.Verify(ctx, token, "")
			}
		}
		if config.Exchange == nil {
			config.Exchange = config.Verifier.Exchange
		}
	}
	if config.Vault == nil {
		return nil, errors.New("vault is required")
	}
	if config.Vault.Store == nil || config.Vault.Pool == nil || config.Vault.Cipher == nil || config.Vault.Now == nil {
		return nil, errors.New("initialized vault is required")
	}
	cipher, err := cryptocompat.NewFernet(config.MasterKey)
	if err != nil {
		return nil, errors.New("matching master key is required")
	}
	probe, err := config.Vault.Cipher.Seal([]byte("aa-key-check"), config.Vault.Now())
	if err != nil {
		return nil, err
	}
	if _, err = cipher.Open(probe); err != nil {
		return nil, errors.New("master key does not match the vault")
	}
	if config.Vault.Pool.Config().MaxConns < 2 {
		return nil, errors.New("HTTP server requires at least two PostgreSQL connections")
	}
	if config.EnforceAAID && config.Verify == nil {
		return nil, errors.New("AA ID verifier is required when enforcement is enabled")
	}
	s := &Server{config: config, mux: http.NewServeMux(), attempts: map[string][]time.Time{}, signal: make(chan struct{}), done: make(chan struct{})}
	s.issued = map[string]map[string]int64{}
	s.attemptWindows = map[string]time.Duration{}
	s.maintenanceDone = make(chan struct{})
	if s.config.Pulse == nil {
		s.ownedPulse = pulse.NewTransport(nil)
		s.config.Pulse = s.ownedPulse
	}
	if s.config.SearchGroups == nil {
		if search, ok := s.config.Pulse.(interface {
			SearchGroups(context.Context, string) []map[string]any
		}); ok {
			s.config.SearchGroups = search.SearchGroups
		} else {
			s.config.SearchGroups = func(context.Context, string) []map[string]any { return []map[string]any{} }
		}
	}
	listenCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	connection, err := config.Vault.Pool.Acquire(listenCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	if _, err = connection.Exec(listenCtx, "LISTEN aa_events"); err != nil {
		connection.Release()
		cancel()
		return nil, err
	}
	if s.config.Calendars == nil {
		s.ownedCalendar = schedule.NewCache(ctx, nil)
		s.ownedCalendar.Now = config.Vault.Now
		s.config.Calendars = s.ownedCalendar
	}
	s.initBrowser(listenCtx)
	s.refreshRoot = listenCtx
	s.initSite(listenCtx)
	s.refreshing = map[string]*refreshFlight{}
	s.renewed = map[string]time.Time{}
	if config.RefreshEvery > 0 {
		s.refreshWG.Add(1)
		go s.refreshLoop()
	}
	go func() {
		defer close(s.done)
		for {
			_, err := connection.Conn().WaitForNotification(listenCtx)
			if err == nil {
				s.changed()
				continue
			}
			connection.Release()
			if listenCtx.Err() != nil {
				return
			}
			for {
				select {
				case <-listenCtx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				connection, err = config.Vault.Pool.Acquire(listenCtx)
				if err != nil {
					continue
				}
				if _, err = connection.Exec(listenCtx, "LISTEN aa_events"); err != nil {
					connection.Release()
					continue
				}
				s.changed()
				break
			}
		}
	}()
	go func() {
		defer close(s.maintenanceDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-listenCtx.Done():
				return
			case <-ticker.C:
				s.prune()
			}
		}
	}()
	s.routes()
	return s, nil
}

func (s *Server) Close() {
	s.cancel()
	s.refreshMu.Lock()
	s.refreshMu.Unlock()
	<-s.done
	<-s.maintenanceDone
	s.refreshWG.Wait()
	s.site.journals.close()
	s.site.presence.close()
	if s.ownedLogins {
		s.config.Logins.Close()
	}
	if s.ownedBrowser != nil {
		s.ownedBrowser.Close()
	}
	if s.ownedPulse != nil {
		s.ownedPulse.Close()
	}
	if s.ownedCalendar != nil {
		s.ownedCalendar.Close()
	}
}
func (s *Server) prune() {
	now := s.config.Vault.Now()
	s.refreshMu.Lock()
	for id, at := range s.renewed {
		if now.Sub(at) > time.Hour {
			delete(s.renewed, id)
		}
	}
	s.refreshMu.Unlock()
	s.attemptsMu.Lock()
	for key, queue := range s.attempts {
		window := s.attemptWindows[key]
		if window < 10*time.Minute {
			window = 10 * time.Minute
		}
		if len(queue) == 0 || queue[len(queue)-1].Before(now.Add(-window)) {
			delete(s.attempts, key)
			delete(s.attemptWindows, key)
		}
	}
	s.attemptsMu.Unlock()
	s.stateMu.Lock()
	for id, tokens := range s.issued {
		for key, expiry := range tokens {
			if expiry <= now.Unix() {
				delete(tokens, key)
			}
		}
		if len(tokens) == 0 {
			delete(s.issued, id)
		}
	}
	s.stateMu.Unlock()
}
func (s *Server) changed() {
	s.signalMu.Lock()
	close(s.signal)
	s.signal = make(chan struct{})
	s.signalMu.Unlock()
}
func (s *Server) subscription() <-chan struct{} {
	s.signalMu.Lock()
	defer s.signalMu.Unlock()
	return s.signal
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func detail(w http.ResponseWriter, status int, message string) {
	write(w, status, map[string]any{"detail": message})
}
func cookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *Server) failure(w http.ResponseWriter, err error) {
	var conflict *postgres.Conflict
	switch {
	case errors.As(err, &conflict):
		detail(w, 409, conflict.Error())
	case errors.Is(err, postgres.ErrNotFound):
		detail(w, 404, "Account not found")
	default:
		detail(w, 500, "Internal Server Error")
	}
}

func (s *Server) owner(r *http.Request, id string) (bool, error) {
	if scoped(r) {
		var allowed bool
		err := s.config.Vault.Pool.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM aa.accounts WHERE id=$1 AND pulse_id=$2)", id, identity(r)).Scan(&allowed)
		return allowed, err
	}
	return s.config.Vault.OwnsSiteAccount(r.Context(), cookie(r, "aa_site"), id)
}
func (s *Server) panel(w http.ResponseWriter, r *http.Request, writing bool) (map[string]any, bool) {
	session, err := s.config.Vault.Session(r.Context(), "panel", cookie(r, "panel_session"))
	if err != nil {
		s.failure(w, err)
		return nil, false
	}
	if session == nil {
		detail(w, 401, "Sign in to the panel")
		return nil, false
	}
	if writing && !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(session["csrf"].(string))) {
		detail(w, 403, "Invalid CSRF token")
		return nil, false
	}
	return session, true
}
func (s *Server) allowOwner(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/panel/") {
		_, ok := s.panel(w, r, true)
		return ok
	}
	allowed, err := s.owner(r, r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return false
	}
	if !allowed {
		detail(w, 401, "Sign in to this account first")
	}
	return allowed
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s.config.PanelLocalOnly && strings.HasPrefix(r.URL.Path, "/panel") && r.Header.Get("X-AA-Panel") != "1" {
		detail(w, 404, "Not Found")
		return
	}
	limit := int64(200000)
	if r.URL.Path == "/internal/aaid/link" {
		limit = 4000000
	}
	if r.ContentLength > limit {
		detail(w, 413, "Request too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		detail(w, 413, "Request too large")
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if (strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/site/")) && r.Method != "GET" && r.Method != "HEAD" {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.User != nil || u.Host != r.Host {
				detail(w, 403, "Invalid origin")
				return
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
	if !scoped(r) && strings.HasPrefix(r.URL.Path, "/api/") && s.config.Verify != nil {
		browserAllowed := r.URL.Path == "/api/groups/search"
		if r.Header.Get("Authorization") == "" && strings.HasPrefix(r.URL.Path, "/api/accounts/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
			if len(parts) == 1 || (len(parts) == 2 && (parts[1] == "attendance" || parts[1] == "label" || parts[1] == "end-session" || parts[1] == "pass-allowed")) {
				browserAllowed, err = s.owner(r, parts[0])
				if err != nil {
					s.failure(w, err)
					return
				}
			}
		}
		if !browserAllowed {
			var claims map[string]any
			scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			if ok && strings.EqualFold(scheme, "bearer") && token != "" {
				claims, _ = s.config.Verify(r.Context(), token)
			}
			if claims == nil && (s.config.EnforceAAID || strings.HasPrefix(r.URL.Path, "/api/me/")) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.Header().Set("X-AAID-Required", "1")
				write(w, 401, map[string]any{"detail": "AA ID sign-in required", "aaid_url": s.config.AAIDURL})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), principalKey{}, claims))
		}
	}
	_, pattern := s.mux.Handler(r)
	if pattern == "GET /" && r.URL.Path != "/" {
		pattern = ""
	}
	if r.Method == "HEAD" && !strings.HasPrefix(r.URL.Path, "/static/") && pattern != "" && (pattern != "GET /" || r.URL.Path == "/") {
		w.Header().Set("Allow", "GET")
		detail(w, 405, "Method Not Allowed")
		return
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
	s.mux.ServeHTTP(w, r)
}

func integer(w http.ResponseWriter, r *http.Request, key string, defaultValue int64) (int64, bool) {
	values, exists := r.URL.Query()[key]
	if !exists {
		return defaultValue, true
	}
	raw := values[len(values)-1]
	value, overflow, valid := parseIntegerText(raw)
	if !valid {
		write(w, 422, map[string]any{"detail": []any{map[string]any{"type": "int_parsing", "loc": []string{"query", key}, "msg": "Input should be a valid integer, unable to parse string as an integer", "input": raw}}})
		return 0, false
	}
	if overflow {
		message := "Invalid event cursor"
		if key == "group_id" {
			message = "Invalid group"
		}
		detail(w, 400, message)
		return 0, false
	}
	return value, true
}
func group(w http.ResponseWriter, r *http.Request) (*int64, bool) {
	if !r.URL.Query().Has("group_id") {
		return nil, true
	}
	value, ok := integer(w, r, "group_id", 0)
	if !ok {
		return nil, false
	}
	if value <= 0 {
		detail(w, 400, "Invalid group")
		return nil, false
	}
	return &value, true
}
func (s *Server) routes() {
	s.handle("GET /health", func(w http.ResponseWriter, r *http.Request) {
		aaid := map[string]any{"required": true, "url": s.config.AAIDURL}
		if s.config.Verify != nil {
			aaid = map[string]any{"required": true, "url": s.config.AAIDURL}
		}
		write(w, 200, map[string]any{"ok": true, "protocol": 2, "sync": "automatic", "aaid": aaid})
	})
	s.handle("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		if scoped(r) {
			s.visibleGroups(w, r)
			return
		}
		rows, err := s.config.Vault.Groups(r.Context())
		if err != nil {
			s.failure(w, err)
			return
		}
		write(w, 200, map[string]any{"groups": rows})
	})
	s.handle("GET /api/accounts", s.accounts)
	s.handle("GET /api/events", s.events)
	s.handle("GET /api/accounts/{id}", s.account)
	s.handle("POST /panel/api/login", s.login)
	s.handle("POST /panel/api/logout", s.logout)
	s.handle("GET /panel/api/state", s.panelState)
	s.handle("GET /auth/aaid", s.signIn)
	s.handle("GET /auth/callback", s.callback)
	s.handle("POST /site/logout", s.siteLogout)
	s.handle("POST /api/accounts/import", s.importAccount)
	s.handle("POST /internal/aaid/link", s.linkIdentity)
	s.handle("GET /api/groups/search", s.searchGroups)
	s.handle("GET /site/schedule", s.weekSchedule)
	s.handle("GET /site/schedule/search", s.searchSchedule)
	s.loginRoutes()
	s.handle("POST /panel/api/debug/pulse-record", s.pulseRecord)
	s.handle("GET /site/today", s.siteToday)
	s.handle("GET /api/me/today", s.siteToday)
	s.handle("GET /site/state", s.siteState)
	s.handle("POST /site/mark", s.siteMark)
	s.handle("GET /panel/api/debug/requests", s.debugRequests)
	s.handle("GET /panel/api/debug/logs", s.debugLogs)
	s.handle("GET /panel/api/debug/system", s.debugSystem)
	s.handle("GET /panel/api/debug/tables", s.debugTables)
	s.handle("GET /panel/api/debug/tables/{name}", s.debugTables)
	s.handle("GET /panel/api/debug/audit", s.debugAudit)
	s.handle("POST /api/debug/pass-testing", s.unlockPassTesting)
	s.handle("GET /api/me/attendance", s.attendance)
	s.handle("GET /api/accounts/{id}/attendance", s.attendance)
	s.handle("GET /panel/api/accounts/{id}/attendance", s.attendance)
	s.handle("GET /panel/api/accounts/{id}/profile-fields", s.profileFields)
	s.handle("POST /panel/api/accounts/{id}/refresh", s.refreshHandler)
	s.handle("GET /panel/api/debug/browser", s.browserDebug)
	s.handle("GET /panel/api/pass-snapshot", s.accounts)
	s.handle("GET /api/me/account", s.myAccount)
	s.handle("PUT /api/me/account", s.changeMyAccount)
	s.handle("GET /api/me/people", s.people)
	s.handle("GET /api/me/pass", func(w http.ResponseWriter, r *http.Request) {
		own, ok := s.ownAccount(w, r, true)
		if ok {
			s.passList(w, r, own)
		}
	})
	s.handle("GET /api/me/passes", func(w http.ResponseWriter, r *http.Request) {
		own, ok := s.ownAccount(w, r, true)
		if ok {
			s.myPasses(w, r, own)
		}
	})
	s.handle("POST /api/me/pass/people", s.share)
	s.handle("DELETE /api/me/pass/people/{kind}/{grant}", s.unshare)
	s.handle("POST /api/me/pass/requests", s.askPass)
	s.handle("POST /api/me/pass/requests/{request}/{decision}", s.answerPass)
	if s.config.StaticDir != "" {
		s.staticRoutes()
	}
	s.handle("DELETE /panel/api/accounts/{id}", s.deleteAccount)
	s.handle("POST /api/accounts/{id}/rpc/{method}", s.rpc)
	s.handle("PUT /api/accounts/{id}/pass", s.publishPass)
	for _, prefix := range []string{"/api/accounts/", "/panel/api/accounts/"} {
		s.handle("PUT "+prefix+"{id}/label", s.rename)
		s.handle("PUT "+prefix+"{id}/pass-allowed", s.passAllowed)
		s.handle("PUT "+prefix+"{id}/group", s.assignGroup)
		s.handle("POST "+prefix+"{id}/end-session", s.endSession)
	}
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
