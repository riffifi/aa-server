package httpserver

import (
	"context"
	"fmt"
	pw "github.com/mxschmitt/playwright-go"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageAndBanPanelInChromium(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	s, v := setup(t, func(c *Config) { c.StaticDir = filepath.Join("..", "..", "static") })
	uid := fmt.Sprintf("ban-ui-%d", time.Now().UnixNano())
	row := account(t, v, uid)
	defer v.Pool.Exec(context.Background(), "DELETE FROM aa.mirea_bans WHERE pulse_uid=$1", uid)
	if err := v.Usage(context.Background(), "attendance", uid, uid, row["id"].(string), map[string]any{"result": "approved", "subject": "Local lesson", "lesson_start": time.Now().Unix(), "lesson_end": time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	token, _, err := v.AdminLogin(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(s)
	defer host.Close()
	driver, err := pw.Run(&pw.RunOptions{DriverDirectory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Stop()
	b, err := driver.Chromium.Launch(pw.BrowserTypeLaunchOptions{Headless: pw.Bool(true), Args: []string{"--no-sandbox"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := b.NewContext()
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
		target, _ := url.Parse(route.Request().URL())
		if target.Host != u.Host {
			_ = route.Abort()
			return
		}
		_ = route.Continue()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto(host.URL + "/panel#usage"); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#usage-person").Fill(uid); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#usage-reload").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#usage-table").Filter(pw.LocatorFilterOptions{HasText: "Local lesson"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("[data-tab=bans]").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#ban-mirea").Fill(uid); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#ban-form button[type=submit]").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#ban-table").Filter(pw.LocatorFilterOptions{HasText: uid}).Filter(pw.LocatorFilterOptions{HasText: "Заблокирован"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#ban-unblock").Click(); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#ban-table").Filter(pw.LocatorFilterOptions{HasText: uid}).Filter(pw.LocatorFilterOptions{HasText: "Разблокирован"}).WaitFor(); err != nil {
		t.Fatal(err)
	}
}
