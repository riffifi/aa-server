package httpserver

import (
	"encoding/json"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pw "github.com/mxschmitt/playwright-go"
)

// Real Chromium, synthetic API responses, no MIREA or production connections.
func TestSiteAndMetricsUIInChromium(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
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
	page, err := browser.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	page.SetDefaultTimeout(5000)
	errors := make(chan string, 16)
	page.OnPageError(func(err error) {
		select {
		case errors <- err.Error():
		default:
		}
	})
	now := time.Now().Unix()
	lesson := func(name string, start, end int64) map[string]any {
		return map[string]any{"subject": name, "start": "09:00", "end": "10:30", "lesson_start": start, "starts_at": start, "ends_at": end}
	}
	previous, current := lesson("Previous class", now-6000, now-600), lesson("Current class", now-300, now+5100)
	if err := page.Route("**/*", func(route pw.Route) {
		u, _ := url.Parse(route.Request().URL())
		var data any
		switch u.Path {
		case "/site/state":
			chosen := current
			if u.Query().Get("lesson_start") != "" && u.Query().Get("lesson_start") != jsonNumber(current["lesson_start"]) {
				chosen = previous
			}
			data = map[string]any{"me": map[string]any{"group_id": 1, "group_name": "Own group", "label": "Owner", "connected": true}, "lesson": current, "next": nil, "selected_lesson": chosen, "available_lessons": []any{previous, current}, "active": "now", "own_group_size": 0, "groups": []any{}, "people": []any{}, "aaid_url": nil}
		case "/panel/api/state":
			data = map[string]any{"csrf": "synthetic", "accounts": []any{}, "cursor": 1}
		case "/panel/api/debug/system":
			// Older real Go responses omitted jobs; resource graphs must still load.
			data = map[string]any{"now": now, "started": now - 60, "process": map[string]any{"go": "go1.26", "pid": 1}, "files": map[string]any{"postgresql_bytes": 1024}, "background": map[string]any{"refresh_loop": "off"}, "browser": map[string]any{"running": false}, "presence": map[string]any{"cached": 0, "checking": 0, "ttl": 60}, "calendars": []any{}}
		case "/panel/api/debug/metrics":
			data = map[string]any{"available": true, "stale": false, "collected_at": now, "since": now - 60, "errors": 0, "filesystem": map[string]any{"available_bytes": 1024}, "total": map[string]any{"cpu_percent": 2, "memory_bytes": 1024, "rx_bytes": 100, "tx_bytes": 200}, "components": []any{map[string]any{"service": "sessions", "running": true, "cpu_percent": 2, "memory_bytes": 1024, "rx_bytes": 100, "tx_bytes": 200}}, "history": []any{map[string]any{"time": now - 60, "cpu_percent": 1, "memory_bytes": 500}, map[string]any{"time": now, "cpu_percent": 2, "memory_bytes": 1024}}}
		default:
			name := ""
			if u.Path == "/" {
				name = "index.html"
			} else if u.Path == "/panel" {
				name = "panel.html"
			} else if strings.HasPrefix(u.Path, "/static/") {
				name = strings.TrimPrefix(u.Path, "/static/")
			}
			if name == "" || strings.Contains(name, "..") {
				_ = route.Abort()
				return
			}
			body, err := os.ReadFile(filepath.Join("..", "..", "static", filepath.FromSlash(name)))
			if err != nil {
				t.Error(err)
				_ = route.Abort()
				return
			}
			_ = route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), ContentType: pw.String(mime.TypeByExtension(filepath.Ext(name))), Body: body})
			return
		}
		body, _ := json.Marshal(data)
		_ = route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), ContentType: pw.String("application/json"), Body: body})
	}); err != nil {
		t.Fatal(err)
	}
	wait := func(expression string) {
		t.Helper()
		handle, err := page.WaitForFunction(expression, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = handle.Dispose()
	}
	if _, err := page.Goto("https://aa.ytc-srv.org/"); err != nil {
		t.Fatal(err)
	}
	wait(`document.getElementById('lesson-title').textContent === 'Current class'`)
	if err := page.Locator("#lesson-prev").Click(); err != nil {
		t.Fatal(err)
	}
	wait(`document.getElementById('lesson-title').textContent === 'Previous class' && document.getElementById('lesson-prev').disabled`)
	if err := page.Locator("#lesson-next-button").Click(); err != nil {
		t.Fatal(err)
	}
	wait(`document.getElementById('lesson-title').textContent === 'Current class' && document.getElementById('lesson-next-button').disabled`)
	if _, err := page.Goto("https://aa.ytc-srv.org/panel#system"); err != nil {
		t.Fatal(err)
	}
	wait(`document.getElementById('metrics-table').textContent.includes('sessions') && document.getElementById('metrics-status').textContent.includes('Обновлено:')`)
	select {
	case err := <-errors:
		t.Fatal("UI JavaScript error:", err)
	default:
	}
}

func jsonNumber(value any) string { body, _ := json.Marshal(value); return string(body) }
