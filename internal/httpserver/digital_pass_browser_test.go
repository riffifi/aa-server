package httpserver

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pw "github.com/mxschmitt/playwright-go"
)

// Exercise the actual page, HTTP authorization, protobuf adapter and encrypted
// PostgreSQL writes in Chromium. Only the university transport is synthetic.
func TestDigitalPassPageInChromium(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	p := &enrollmentPulse{}
	s, v := setup(t, func(c *Config) {
		c.Pulse = p
		c.StaticDir = filepath.Join("..", "..", "static")
	})
	uid := fmt.Sprintf("pass-browser-%d", time.Now().UnixNano())
	row := account(t, v, uid)
	if _, err := v.SetPassAllowed(context.Background(), row["id"].(string), true, nil); err != nil {
		t.Fatal(err)
	}
	token, err := v.CreateSiteSession(context.Background(), uid, uid, nil, nil, 3600)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Logout(context.Background(), "site", token)
	host := httptest.NewServer(s)
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
	if err := c.AddCookies([]pw.OptionalCookie{{Name: "aa_site", Value: token, Domain: pw.String(u.Hostname()), Path: pw.String("/")}}); err != nil {
		t.Fatal(err)
	}
	page, err := c.NewPage()
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
	if _, err := page.Goto(host.URL + "/passes"); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#pass-code").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#pass-status").Filter(pw.LocatorFilterOptions{HasText: "Код отправлен"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#pass-input").Fill("012345"); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#pass-issue").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#pass-status").Filter(pw.LocatorFilterOptions{HasText: "Пропуск сохранён"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#pass-list button").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator(".pass-number").Filter(pw.LocatorFilterOptions{HasText: "9223372036854775807"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if text, err := page.Locator("body").TextContent(); err != nil || strings.Contains(text, "server.only.token") {
		t.Fatal("token exposed", err)
	}
	if value, err := page.Evaluate(`() => ({local: localStorage.length, session: sessionStorage.length})`); err != nil {
		t.Fatal(err)
	} else {
		values := value.(map[string]any)
		if fmt.Sprint(values["local"]) != "0" || fmt.Sprint(values["session"]) != "0" {
			t.Fatal("pass page persisted browser data", values)
		}
	}
	if err := page.Locator("#pass-refresh").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#pass-list button").WaitFor(); err != nil {
		t.Fatal(err)
	}
	if text, err := page.Locator(".pass-number").TextContent(); err != nil || text != "" {
		t.Fatal("refresh retained credential", text, err)
	}
	select {
	case message := <-errors:
		t.Fatal("page error", message)
	default:
	}
}
