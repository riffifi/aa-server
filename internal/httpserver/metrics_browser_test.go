package httpserver

import (
	"aa-server/internal/postgres"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	pw "github.com/mxschmitt/playwright-go"
)

// Unlike the synthetic UI fixture, this renders the actual Go runtime response
// through the protected panel routes, with real PostgreSQL authentication.
func TestMetricsUIWithRuntimeServer(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	file := filepath.Join(t.TempDir(), "snapshot.json")
	now := time.Now().Unix()
	snapshot := map[string]any{"version": 1, "project": "test", "collected_at": now, "since": now, "errors": 0, "filesystem": map[string]any{"available_bytes": 10000}, "total": map[string]any{"cpu_percent": 2, "memory_bytes": 1024, "rx_bytes": 100, "tx_bytes": 20}, "components": []any{map[string]any{"service": "sessions", "running": true, "cpu_percent": 2, "memory_bytes": 1024}}}
	body, _ := json.Marshal(snapshot)
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	server, v := setup(t, func(c *Config) {
		c.MetricsFile = file
		c.StaticDir = filepath.Join("..", "..", "static")
		c.PanelLocalOnly = true
	})
	ctx := context.Background()
	uid := fmt.Sprintf("effect-ui-%d", time.Now().UnixNano())
	account, err := v.Upsert(ctx, postgres.Upsert{PulseID: uid, Label: "Measured auto-renewal", Credentials: map[string]any{"cookie": "private-ui-cookie", "expires_at": float64(now - 60)}})
	if err != nil {
		t.Fatal(err)
	}
	accountID := account["id"].(string)
	t.Cleanup(func() { _ = v.DeleteAccount(ctx, accountID) })
	version := account["session_version"].(int64)
	if err := v.NoteRenewal(ctx, accountID, version, "running", "expired"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Upsert(ctx, postgres.Upsert{PulseID: uid, AccountID: accountID, SessionExpected: &version, AutomaticRenewal: true, Credentials: map[string]any{"cookie": "private-ui-replacement", "expires_at": float64(now + 3600)}}); err != nil {
		t.Fatal(err)
	}
	token, _, err := v.AdminLogin(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	var metricRequests atomic.Int64
	var runtimeFailure atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-AA-Panel", "1")
		if r.URL.Path == "/panel/api/debug/metrics" {
			metricRequests.Add(1)
		}
		if runtimeFailure.Load() && r.URL.Path == "/panel/api/debug/system" {
			detail(w, 503, "Synthetic runtime diagnostic failure")
			return
		}
		server.ServeHTTP(w, r)
	}))
	defer host.Close()
	driver, err := pw.Run(&pw.RunOptions{DriverDirectory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Stop()
	browser, err := driver.Chromium.Launch(pw.BrowserTypeLaunchOptions{Headless: pw.Bool(true), Args: []string{"--no-sandbox"}})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	c, err := browser.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u, _ := url.Parse(host.URL)
	if err := c.AddCookies([]pw.OptionalCookie{{Name: "panel_session", Value: token, Domain: pw.String(u.Hostname()), Path: pw.String("/panel")}}); err != nil {
		t.Fatal(err)
	}
	page, err := c.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	page.SetDefaultTimeout(5000)
	if err := page.Route("**/*", func(route pw.Route) {
		request, _ := url.Parse(route.Request().URL())
		if request.Host != u.Host {
			_ = route.Abort()
			return
		}
		_ = route.Continue()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto(host.URL + "/panel#system"); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#metrics-table").Filter(pw.LocatorFilterOptions{HasText: "sessions"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#renewal-facts > div").Filter(pw.LocatorFilterOptions{HasText: regexp.MustCompile(`^Восстановлено истёкших\s*1$`)}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Locator("#renewal-account").SelectOption(pw.SelectOptionValues{Values: pw.StringSlice(accountID)}); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#renewal-facts > div").Filter(pw.LocatorFilterOptions{HasText: regexp.MustCompile(`^С подтверждённым эффектом\s*1$`)}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#renewal-history").Filter(pw.LocatorFilterOptions{HasText: "Успех"}).Filter(pw.LocatorFilterOptions{HasText: "+3660 с"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	// A new collector has no minute history yet. Its current point must be visible.
	painted, err := page.Evaluate(`() => {const a=document.getElementById('metrics-cpu').getContext('2d').getImageData(0,0,900,120).data;for(let i=0;i<a.length;i+=4) if(a[i]===71&&a[i+1]===120&&a[i+2]===223&&a[i+3]>0) return true;return false}`)
	if err != nil || painted != true {
		t.Fatal("current CPU point not painted", painted, err)
	}
	before := metricRequests.Load()
	runtimeFailure.Store(true)
	if _, err := page.Evaluate(`() => loadSystem()`); err != nil {
		t.Fatal(err)
	}
	if metricRequests.Load() <= before {
		t.Fatal("runtime failure prevented resource refresh")
	}
}
