package browser

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	pw "github.com/mxschmitt/playwright-go"
)

func TestRenewalStorageKeepsSSOButCannotReusePulseTickets(t *testing.T) {
	state := map[string]any{"cookies": []any{}, "origins": []any{map[string]any{"origin": "https://pulse.mirea.ru", "localStorage": []any{}}}}
	for _, c := range []struct{ name, domain string }{
		{"Pulse.Auth.Cookie", "pulse.mirea.ru"}, {"Pulse.Auth.CookieC1", ".mirea.ru"},
		{".AspNetCore.Cookies", "pulse.mirea.ru"}, {".AspNetCore.CookiesC2", "pulse.mirea.ru"},
		{"KEYCLOAK_IDENTITY", "sso.mirea.ru"}, {"KEYCLOAK_SESSION", "sso.mirea.ru"},
		{"__ddg9_", ".mirea.ru"}, {"Pulse.Auth.Cookie", "sso.mirea.ru"},
	} {
		state["cookies"] = append(state["cookies"].([]any), map[string]any{"name": c.name, "domain": c.domain, "value": "synthetic", "path": "/"})
	}
	before, _ := json.Marshal(state)
	clean, err := renewalStorage(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean["cookies"].([]any)) != 4 {
		t.Fatal("SSO/challenge state lost or Pulse ticket retained")
	}
	for _, item := range clean["cookies"].([]any) {
		c := item.(map[string]any)
		if c["domain"] != "sso.mirea.ru" && strings.Contains(c["name"].(string), "Cookie") {
			t.Fatal("old Pulse auth survived")
		}
		c["value"] = "changed"
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("stored credentials mutated")
	}
}

func TestChromiumRenewalContextStartsWithoutOldPulseTicket(t *testing.T) {
	dir := os.Getenv("TEST_BROWSER_DRIVER")
	if dir == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	e := New(context.Background(), Config{DriverDirectory: dir})
	defer e.Close()
	state := map[string]any{"cookies": []any{}, "origins": []any{}}
	for _, c := range []struct{ name, domain string }{{"Pulse.Auth.Cookie", "pulse.mirea.ru"}, {"KEYCLOAK_IDENTITY", "sso.mirea.ru"}, {"__ddg9_", ".mirea.ru"}} {
		state["cookies"] = append(state["cookies"].([]any), map[string]any{"name": c.name, "domain": c.domain, "value": "synthetic", "path": "/", "expires": float64(time.Now().Add(time.Hour).Unix()), "secure": true, "httpOnly": true, "sameSite": "Lax"})
	}
	clean, err := renewalStorage(state)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.newContext(clean)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	page, err := c.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	if err := page.Route("**/*", func(route pw.Route) {
		seen = append(seen, route.Request().Headers()["cookie"])
		_ = route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), ContentType: pw.String("text/html"), Body: "<p>Offline renewal</p>"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = page.Goto("https://pulse.mirea.ru/api/auth/login"); err != nil {
		t.Fatal(err)
	}
	if _, err = page.Goto("https://sso.mirea.ru/realms/mirea/"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || strings.Contains(seen[0], "Pulse.Auth.Cookie") || !strings.Contains(seen[0], "__ddg9_") || !strings.Contains(seen[1], "KEYCLOAK_IDENTITY") {
		t.Fatal("wrong renewal cookies")
	}
	// A fresh provider-issued ticket can be captured normally after the handoff.
	if err = c.AddCookies([]pw.OptionalCookie{{Name: "Pulse.Auth.Cookie", Value: "fresh", Domain: pw.String("pulse.mirea.ru"), Path: pw.String("/"), Expires: pw.Float(float64(time.Now().Add(14 * 24 * time.Hour).Unix()))}}); err != nil {
		t.Fatal(err)
	}
	credentials, err := credentials(c, time.Now())
	if err != nil || !strings.Contains(credentials["cookie"].(string), "Pulse.Auth.Cookie=fresh") {
		t.Fatal("fresh ticket capture failed", err)
	}
}
