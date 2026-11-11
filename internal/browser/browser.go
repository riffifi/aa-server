// Package browser implements MIREA's existing browser sign-in and silent renewal.
// Credentials are returned only to the caller; this package never logs them.
package browser

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"aa-server/internal/pulse"
	pw "github.com/mxschmitt/playwright-go"
)

//go:embed qr.js
var qrScript string

var ErrClosed = errors.New("browser is closed")
var ErrExpired = errors.New("login expired")
var ErrNotAuthenticated = errors.New("Pulse session is not authenticated")
var ErrReauthenticationRequired = errors.New("SSO requires a new interactive sign-in")
var authCookie = regexp.MustCompile(`^\.AspNetCore\.Cookies(?:C[1-9][0-9]*)?$`)

const loginURL = "https://pulse.mirea.ru/api/auth/login?redirectUri=https%3A%2F%2Fpulse.mirea.ru%2Fservices&rememberMe="

type Result struct {
	ProfileName string
	AccountID   string
	UID         string
	GroupID     *int64
	GroupName   *string
	Credentials map[string]any
}
type Update struct{ Status, QR string }
type Config struct {
	DriverDirectory        string
	LoginSlots, RenewSlots int
	Identity               bool
	RecycleAfter           int
}
type Engine struct {
	config           Config
	mu               sync.Mutex
	driver           *pw.Playwright
	browser          pw.Browser
	closed           bool
	root             context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	logins, renewals chan struct{}
	assets           *Assets
	traffic          *Traffic
	metricsWG        sync.WaitGroup
	completed        int
	launched         time.Time
}

func New(ctx context.Context, c Config) *Engine {
	if c.LoginSlots < 1 {
		c.LoginSlots = 6
	}
	if c.RenewSlots < 1 {
		c.RenewSlots = 2
	}
	if c.RecycleAfter < 1 {
		c.RecycleAfter = 50
	}
	root, cancel := context.WithCancel(ctx)
	return &Engine{config: c, root: root, cancel: cancel, logins: make(chan struct{}, c.LoginSlots), renewals: make(chan struct{}, c.RenewSlots), assets: NewAssets(), traffic: newTraffic()}
}
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.browser != nil {
		_ = e.browser.Close()
		e.browser = nil
	}
	e.metricsWG.Wait()
	e.assets.Clear()
	if e.driver != nil {
		_ = e.driver.Stop()
		e.driver = nil
	}
}

// AllowedURL also rejects explicit non-TLS ports and URL credentials.
func AllowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "pulse.mirea.ru" || host == "sso.mirea.ru" || host == "ddos-guard.net" || strings.HasSuffix(host, ".ddos-guard.net")
}
func (e *Engine) newContext(state map[string]any, lifetime ...context.Context) (pw.BrowserContext, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	if e.driver == nil {
		driver, err := pw.Run(&pw.RunOptions{DriverDirectory: e.config.DriverDirectory})
		if err != nil {
			return nil, err
		}
		e.driver = driver
	}
	if e.browser != nil && len(e.browser.Contexts()) == 0 && (e.completed >= e.config.RecycleAfter || time.Since(e.launched) > 30*time.Minute) {
		_ = e.browser.Close()
		e.browser = nil
	}
	if e.browser == nil || !e.browser.IsConnected() {
		browser, err := e.driver.Chromium.Launch(pw.BrowserTypeLaunchOptions{Headless: pw.Bool(true), Args: []string{"--disable-dev-shm-usage"}})
		if err != nil {
			return nil, err
		}
		e.browser = browser
		e.launched = time.Now()
		e.completed = 0
	}
	work := e.root
	if len(lifetime) > 0 {
		work = lifetime[0]
	}
	options := pw.BrowserNewContextOptions{Viewport: &pw.Size{Width: 900, Height: 900}}
	if diagnostic, _ := work.Value(diagnosticKey{}).(bool); !diagnostic {
		options.ServiceWorkers = pw.ServiceWorkerPolicyBlock
	}
	if state != nil {
		data, err := json.Marshal(state)
		if err != nil {
			return nil, err
		}
		var storage pw.OptionalStorageState
		if err = json.Unmarshal(data, &storage); err != nil {
			return nil, err
		}
		options.StorageState = &storage
	}
	browserContext, err := e.browser.NewContext(options)
	if err != nil {
		return nil, err
	}
	browserContext.SetDefaultTimeout(5000)
	browserContext.SetDefaultNavigationTimeout(30000)
	mark, stop := e.traffic.track(work, browserContext)
	e.metricsWG.Add(1)
	browserContext.OnClose(func(pw.BrowserContext) { stop(); e.metricsWG.Done() })
	if err = browserContext.Route("**/*", func(route pw.Route) {
		if blockedAuthResource(work, route.Request().URL(), route.Request().ResourceType()) {
			e.traffic.blocked()
			_ = route.Abort()
			return
		}
		if blockedDiagnostic(work, route.Request().URL()) {
			_ = route.Abort()
			return
		}
		if AllowedURL(route.Request().URL()) || diagnosticURL(work, route.Request().URL()) {
			_ = e.assets.Route(work, route, mark)
		} else {
			_ = route.Abort()
		}
	}); err != nil {
		_ = browserContext.Close()
		return nil, err
	}
	return browserContext, nil
}

type response interface {
	Body() ([]byte, error)
	Dispose() error
	Status() int
	Headers() map[string]string
}

// Consume always releases Playwright's retained response, including body errors.
func consume(r response) (pulse.Reply, error) {
	defer r.Dispose()
	headers := http.Header{}
	for key, value := range r.Headers() {
		headers.Set(key, value)
	}
	body, err := r.Body()
	if err != nil {
		return pulse.Reply{}, err
	}
	if len(body) > pulse.MaxResponse {
		return pulse.Reply{}, pulse.ErrResponseLimit
	}
	return pulse.Reply{Status: r.Status(), Headers: headers, Body: body}, nil
}
func call(c pw.BrowserContext, method string) (pulse.Reply, error) {
	body := pulse.Frame(nil, 0)
	if method == "profile" {
		body = pulse.ProfileRequest()
	}
	r, err := c.Request().Post(pulse.Host+pulse.Methods[method], pw.APIRequestContextPostOptions{Data: body, Headers: map[string]string{"content-type": "application/grpc-web+proto", "x-grpc-web": "1", "x-requested-with": "XMLHttpRequest", "origin": pulse.Host}, Timeout: pw.Float(15000), MaxRedirects: pw.Int(0)})
	if err != nil {
		return pulse.Reply{}, err
	}
	return consume(r)
}
func credentials(c pw.BrowserContext, now time.Time) (map[string]any, error) {
	cookies, err := c.Cookies(pulse.Host + pulse.Methods["profile"])
	if err != nil {
		return nil, err
	}
	header, expiry, err := cookieCredentials(cookies, now)
	if err != nil {
		return nil, err
	}
	state, err := c.StorageState()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	var storage map[string]any
	if err = json.Unmarshal(raw, &storage); err != nil {
		return nil, err
	}
	return map[string]any{"cookie": header, "expires_at": expiry, "renewed_at": float64(now.UnixMilli()) / 1000, "storage_state": storage}, nil
}
func cookieCredentials(cookies []pw.Cookie, now time.Time) (string, any, error) {
	items := []string{}
	var expiry any
	for _, cookie := range cookies {
		if cookie.Value == "" || strings.HasPrefix(cookie.Name, ".AspNetCore.Correlation.") || strings.HasPrefix(cookie.Name, ".AspNetCore.OpenIdConnect.Nonce.") {
			continue
		}
		items = append(items, cookie.Name+"="+cookie.Value)
		if authCookie.MatchString(cookie.Name) && cookie.Expires > float64(now.UnixMilli())/1000 {
			if expiry == nil || cookie.Expires < expiry.(float64) {
				expiry = cookie.Expires
			}
		}
	}
	header := strings.Join(items, "; ")
	if err := pulse.ValidateCookie(header); err != nil {
		return "", nil, err
	}
	return header, expiry, nil
}
func (e *Engine) capture(c pw.BrowserContext) (*Result, error) {
	reply, err := call(c, "profile")
	if err == nil {
		e.traffic.api(len(reply.Body))
	}
	if err != nil {
		return nil, err
	}
	var uid string
	if e.config.Identity {
		uid, err = pulse.DecodeIdentity(reply.Status, reply.Headers, reply.Body)
	} else {
		state, value := pulse.AuthenticationState(reply.Status, reply.Headers, reply.Body)
		if state == "valid" {
			uid = value
		}
	}
	if err != nil {
		return nil, err
	}
	if uid == "" {
		return nil, ErrNotAuthenticated
	}
	result := &Result{UID: uid}
	result.ProfileName = pulse.ProfileName(reply.Body, reply.Headers, uid)
	if e.config.Identity {
		group, err := call(c, "schedules")
		if err == nil {
			e.traffic.api(len(group.Body))
		}
		if err == nil && group.Status == 200 {
			result.GroupID, result.GroupName = pulse.DecodeGroup(group.Headers, group.Body)
		}
	}
	result.Credentials, err = credentials(c, time.Now())
	if err != nil && !e.config.Identity {
		return nil, err
	}
	return result, nil
}
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Login runs one QR approval; Renew replays saved SSO state without exposing a QR.
func (e *Engine) Login(ctx context.Context, update func(Update)) (*Result, error) {
	return e.run(ctx, nil, false, update)
}
func (e *Engine) Renew(ctx context.Context, state map[string]any) (*Result, error) {
	if len(state) == 0 {
		return nil, ErrNotAuthenticated
	}
	return e.run(ctx, state, true, func(Update) {})
}
func (e *Engine) run(parent context.Context, state map[string]any, renew bool, update func(Update)) (*Result, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, ErrClosed
	}
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stopRoot := context.AfterFunc(e.root, cancel)
	defer stopRoot()
	slots := e.logins
	if renew {
		slots = e.renewals
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c, err := e.newContext(state, ctx)
	if err != nil {
		return nil, err
	}
	defer func() { e.mu.Lock(); e.completed++; e.mu.Unlock() }()
	// The client has no context-aware actions: closing the context interrupts them.
	// Wait for the cancellation hook before returning, so no close goroutine leaks.
	var once sync.Once
	closed := make(chan struct{})
	closeContext := func() { once.Do(func() { _ = c.Close(); close(closed) }) }
	stop := context.AfterFunc(ctx, closeContext)
	defer func() { stop(); closeContext(); <-closed }()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	page, err := c.NewPage()
	if err != nil {
		return nil, err
	}
	if !renew {
		if err = page.Route("**/qr-code-auth/check-status?*", func(route pw.Route) { _ = route.Abort() }); err != nil {
			return nil, err
		}
	}
	update(Update{Status: "login"})
	remember := "True"
	timeout := 45000.
	if renew {
		timeout = 30000
	}
	if _, err = page.Goto(loginURL+remember, pw.PageGotoOptions{WaitUntil: pw.WaitUntilStateDomcontentloaded, Timeout: &timeout}); err != nil {
		return nil, err
	}
	approved := time.Time{}
	submitted := false
	for attempt := 0; !renew || attempt < 10; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !approved.IsZero() && time.Since(approved) > 90*time.Second {
			return nil, &UserError{"Пульс не подтвердил вход. Начните заново."}
		}
		where, _ := url.Parse(page.URL())
		if where != nil {
			switch where.Hostname() {
			case "pulse.mirea.ru":
				if result, err := e.capture(c); err == nil {
					return result, nil
				}
				advance(page, e.config.Identity)
			case "sso.mirea.ru":
				if renew && reauthenticationRequired(page) {
					return nil, ErrReauthenticationRequired
				}
				if renew || submitted {
					advance(page, e.config.Identity)
				} else {
					yes, err := qrStep(c, page, update, e.config.Identity, e.traffic)
					if err != nil {
						return nil, err
					}
					if yes {
						submitted = true
						approved = time.Now()
					}
				}
			}
		}
		wait := 2 * time.Second
		if renew {
			wait = time.Second
		}
		if !sleep(ctx, wait) {
			return nil, ctx.Err()
		}
	}
	return nil, ErrNotAuthenticated
}
