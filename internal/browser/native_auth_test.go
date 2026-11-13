package browser

import (
	"context"
	"net/url"
	"os"
	"testing"

	pw "github.com/mxschmitt/playwright-go"
)

func TestNativeAuthEntryPreservesSecurityAndRememberMe(t *testing.T) {
	for _, native := range []bool{false, true} {
		u, err := url.Parse(loginURL(native))
		if err != nil || !AllowedURL(u.String()) || u.Host != "pulse.mirea.ru" || u.Path != "/api/auth/login" {
			t.Fatal("invalid auth destination", err)
		}
		q := u.Query()
		if q.Get("rememberMe") != "True" || q.Get("redirectUri") != "https://pulse.mirea.ru/services" {
			t.Fatal("login parameters changed")
		}
		if native && q.Get("nativeApp") != "true" || !native && q.Has("nativeApp") {
			t.Fatal("native mode not applied")
		}
	}
}

func TestChromiumSendsNativeAppFlag(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	e := New(context.Background(), Config{DriverDirectory: directory, NativeAppAuth: true})
	defer e.Close()
	c, err := e.newContext(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	page, err := c.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 1)
	if err := page.Route("**/*", func(route pw.Route) {
		select {
		case seen <- route.Request().URL():
		default:
		}
		_ = route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), ContentType: pw.String("text/html"), Body: "<p>Local native-auth fixture</p>"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto(loginURL(e.config.NativeAppAuth)); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-seen:
		u, err := url.Parse(raw)
		if err != nil || u.Query().Get("nativeApp") != "true" || u.Query().Get("rememberMe") != "True" {
			t.Fatal("browser omitted native auth parameters", err)
		}
	default:
		t.Fatal("auth navigation was not observed")
	}
}
