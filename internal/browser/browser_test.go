package browser

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aa-server/internal/pulse"
	pw "github.com/mxschmitt/playwright-go"
)

type fakeResponse struct {
	body     []byte
	err      error
	disposed int
}

func (r *fakeResponse) Body() ([]byte, error)      { return r.body, r.err }
func (r *fakeResponse) Dispose() error             { r.disposed++; return nil }
func (r *fakeResponse) Headers() map[string]string { return map[string]string{"grpc-status": "0"} }
func (r *fakeResponse) Status() int                { return 200 }
func TestResponseDisposedOnEveryReadOutcome(t *testing.T) {
	for _, r := range []*fakeResponse{{body: []byte("ok")}, {err: errors.New("read failed")}, {body: make([]byte, pulse.MaxResponse+1)}} {
		_, err := consume(r)
		if r.disposed != 1 {
			t.Fatal("response retained")
		}
		if (r.err != nil || len(r.body) > pulse.MaxResponse) != (err != nil) {
			t.Fatal("read error lost")
		}
	}
}
func TestCookieCaptureUsesOnlyAuthenticationExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	cookies := []pw.Cookie{{Name: ".AspNetCore.Cookies", Value: "auth", Expires: 2000}, {Name: ".AspNetCore.CookiesC2", Value: "chunk", Expires: 1500}, {Name: ".AspNetCore.Correlation.flow", Value: "sensitive", Expires: 1100}, {Name: ".AspNetCore.OpenIdConnect.Nonce.flow", Value: "nonce"}, {Name: "theme", Value: "dark", Expires: 1001}}
	header, expiry, err := cookieCredentials(cookies, now)
	if err != nil || expiry != float64(1500) || strings.Contains(header, "sensitive") || strings.Contains(header, "nonce") || !strings.Contains(header, "theme=dark") {
		t.Fatal(header, expiry, err)
	}
	if _, _, err = cookieCredentials([]pw.Cookie{{Name: "bad", Value: "injected\r\nheader=x"}}, now); err == nil {
		t.Fatal("invalid browser cookie accepted")
	}
}
func TestBrowserURLGuard(t *testing.T) {
	for _, value := range []string{"https://pulse.mirea.ru/", "https://sso.mirea.ru/", "https://x.ddos-guard.net/"} {
		if !AllowedURL(value) {
			t.Fatal(value)
		}
	}
	for _, value := range []string{"http://pulse.mirea.ru/", "https://pulse.mirea.ru:8090/", "https://pulse.mirea.ru.evil/", "https://user@pulse.mirea.ru/", "https://127.0.0.1/", "file:///etc/passwd", "https://notddos-guard.net/"} {
		if AllowedURL(value) {
			t.Fatal(value)
		}
	}
}

type runner func(context.Context, func(Update)) (*Result, error)

func (f runner) Login(ctx context.Context, update func(Update)) (*Result, error) {
	return f(ctx, update)
}
func awaitState(t *testing.T, j *Jobs, id, token, status string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, err := j.State(id, token)
		if err != nil {
			t.Fatal(err)
		}
		if state.Status == status {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job did not reach %s", status)
}
func TestClaimAtomicCapabilityPrivateAndCleanupReleasesSecrets(t *testing.T) {
	jobs := NewJobs(context.Background(), runner(func(_ context.Context, update func(Update)) (*Result, error) {
		update(Update{Status: "finishing"})
		return &Result{UID: "synthetic", Credentials: map[string]any{"cookie": "secret"}}, nil
	}))
	defer jobs.Close()
	state, token, err := jobs.Start()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = jobs.State(state.ID, "wrong"); !errors.Is(err, ErrMissing) {
		t.Fatal("wrong capability accepted")
	}
	awaitState(t, jobs, state.ID, token, "complete")
	var success atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if result, err := jobs.Claim(state.ID, token); err == nil {
				if result.UID != "synthetic" {
					t.Error("identity changed")
				}
				success.Add(1)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatal("login claimed more than once")
	}
	jobs.mu.Lock()
	if jobs.entries[state.ID].result != nil {
		t.Fatal("credentials retained after claim")
	}
	jobs.entries[state.ID].created = time.Now().Add(-16 * time.Minute)
	jobs.mu.Unlock()
	jobs.Prune()
	if _, err = jobs.State(state.ID, token); !errors.Is(err, ErrMissing) {
		t.Fatal("expired job retained")
	}
}
func TestJobsBoundQueueExpireAndCloseWorkers(t *testing.T) {
	jobs := NewJobs(context.Background(), runner(func(ctx context.Context, _ func(Update)) (*Result, error) { <-ctx.Done(); return nil, ctx.Err() }))
	jobs.MaxActive = 1
	jobs.TTL = 30 * time.Millisecond
	state, token, err := jobs.Start()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = jobs.Start(); !errors.Is(err, ErrBusy) {
		t.Fatal("active queue unbounded")
	}
	awaitState(t, jobs, state.ID, token, "expired")
	jobs.Close()
	if _, _, err = jobs.Start(); !errors.Is(err, ErrClosed) {
		t.Fatal("closed manager accepted work")
	}
}
func TestJobsDoNotExposeDriverErrorContents(t *testing.T) {
	jobs := NewJobs(context.Background(), runner(func(context.Context, func(Update)) (*Result, error) {
		return nil, errors.New("https://sso.mirea.ru/?secret=private-cookie")
	}))
	defer jobs.Close()
	state, token, _ := jobs.Start()
	awaitState(t, jobs, state.ID, token, "failed")
	state, _ = jobs.State(state.ID, token)
	if state.Error == nil || strings.Contains(*state.Error, "secret") {
		t.Fatal("driver error leaked")
	}
}

func TestEngineCloseCancelsQueuedWorkerWithoutStartingDriver(t *testing.T) {
	engine := New(context.Background(), Config{LoginSlots: 1})
	engine.logins <- struct{}{}
	finished := make(chan error, 1)
	go func() { _, err := engine.Login(context.Background(), func(Update) {}); finished <- err }()
	engine.Close()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued browser worker survived Close")
	}
	if engine.driver != nil {
		t.Fatal("queued login started browser driver")
	}
}

// This uses real Chromium, but all documents are fulfilled in-process. It never
// contacts MIREA and does not require an account or production credentials.
func TestChromiumLocalQRConsentCookiesAndContextCleanup(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER is required for the local Chromium check")
	}
	engine := New(context.Background(), Config{DriverDirectory: directory, Identity: true})
	defer engine.Close()
	c, err := engine.newContext(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	page, err := c.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	if err = page.Route("**/*", func(route pw.Route) {
		_ = route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), ContentType: pw.String("text/html; charset=utf-8"), Body: `<script>window.kcContext={pageId:'login.ftl',QRauthToken:'https://sso.mirea.ru/realms/mirea/qr-code-auth/scan?token=synthetic',sessionId:'session',tabId:'tab',url:{loginAction:'https://sso.mirea.ru/realms/mirea/login-actions/authenticate'},QRauthExecId:'execution'};</script><button onclick="window.clicked='reject'">Отмена</button><button onclick="window.clicked='accept'">Продолжить</button>`})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = page.Goto("https://sso.mirea.ru/local-synthetic"); err != nil {
		t.Fatal(err)
	}
	raw, err := page.Evaluate(qrScript)
	if err != nil {
		t.Fatal(err)
	}
	if raw.(map[string]any)["sessionId"] != "session" {
		t.Fatal("QR context extraction changed")
	}
	advance(page, true)
	clicked, _ := page.Evaluate("window.clicked")
	if clicked != "accept" {
		t.Fatal("consent clicked the wrong control", clicked)
	}
	if err = page.SetContent(`<button onclick="window.clicked='linked'">Подтвердить MAX</button><button name="cancel-aia" onclick="window.clicked='skipped'">Пропустить</button>`); err != nil {
		t.Fatal(err)
	}
	if _, err = page.Evaluate(`history.replaceState({}, '', '/realms/mirea/login-actions/required-action')`); err != nil {
		t.Fatal(err)
	}
	advance(page, true)
	clicked, _ = page.Evaluate("window.clicked")
	if clicked != "skipped" {
		t.Fatal("optional action linked an account", clicked)
	}
	if err = c.AddCookies([]pw.OptionalCookie{{Name: ".AspNetCore.Cookies", Value: "synthetic", Domain: pw.String("pulse.mirea.ru"), Path: pw.String("/"), Secure: pw.Bool(true), Expires: pw.Float(float64(time.Now().Add(time.Hour).Unix()))}}); err != nil {
		t.Fatal(err)
	}
	secret, err := credentials(c, time.Now())
	if err != nil || secret["cookie"] != ".AspNetCore.Cookies=synthetic" {
		t.Fatal("browser cookies/state capture failed", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if len(engine.browser.Contexts()) != 0 {
		t.Fatal("browser context retained")
	}
}

func TestChromiumRepeatedContextsAndRecycle(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	engine := New(context.Background(), Config{DriverDirectory: directory, RecycleAfter: 4})
	defer engine.Close()
	for i := 0; i < 12; i++ {
		c, err := engine.newContext(nil)
		if err != nil {
			t.Fatal(err)
		}
		page, err := c.NewPage()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = page.Evaluate(`window.transient = new ArrayBuffer(4 * 1024 * 1024)`); err != nil {
			t.Fatal(err)
		}
		if err = c.Close(); err != nil {
			t.Fatal(err)
		}
		engine.mu.Lock()
		engine.completed++
		engine.mu.Unlock()
		if len(engine.browser.Contexts()) != 0 {
			t.Fatal("context retained after close")
		}
	}
	engine.metricsWG.Wait()
	stats := engine.Stats()
	if stats["contexts"] != 0 {
		t.Fatal("contexts survived repeated logins")
	}
	if engine.completed > 4 {
		t.Fatal("idle browser was not recycled")
	}
}
