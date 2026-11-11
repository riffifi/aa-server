package browser

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"aa-server/internal/pulse"
	pw "github.com/mxschmitt/playwright-go"
)

func TestChromiumRestoresRotatedCookieAndSSOState(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	e := New(context.Background(), Config{DriverDirectory: directory})
	defer e.Close()
	c, err := e.newContext(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddCookies([]pw.OptionalCookie{
		{Name: ".AspNetCore.Cookies", Value: "old", Domain: pw.String("pulse.mirea.ru"), Path: pw.String("/"), HttpOnly: pw.Bool(true), Secure: pw.Bool(true)},
		{Name: "SSO_TEST", Value: "sso-private", Domain: pw.String("sso.mirea.ru"), Path: pw.String("/"), HttpOnly: pw.Bool(true), Secure: pw.Bool(true)},
	}); err != nil {
		t.Fatal(err)
	}
	secret, err := credentials(c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	patch, _, err := pulse.MergeSessionCookies(secret, http.Header{"Set-Cookie": {".AspNetCore.Cookies=rotated; Path=/; Secure; HttpOnly; Max-Age=86400"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.newContext(patch["storage_state"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	cookies, err := restored.Cookies("https://pulse.mirea.ru/")
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 || cookies[0].Value != "rotated" || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatal("rotated browser cookie missing")
	}
	sso, err := restored.Cookies("https://sso.mirea.ru/")
	if err != nil || len(sso) != 1 || sso[0].Value != "sso-private" {
		t.Fatal("SSO storage was damaged", err)
	}
}
