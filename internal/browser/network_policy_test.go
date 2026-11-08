package browser

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	pw "github.com/mxschmitt/playwright-go"
)

func TestAuthenticationDoesNotDownloadPulseApplication(t *testing.T) {
	ctx := context.Background()
	for _, item := range [][2]string{{"/_framework/dotnet.native.wasm", "fetch"}, {"/_framework/blazor.webassembly.js", "script"}, {"/content/app.js", "script"}, {"/assets/theme.css", "stylesheet"}, {"/runtime/module.dll", "fetch"}, {"/_content/bundle.dat", "fetch"}, {"/large-video.mp4", "media"}} {
		if !blockedAuthResource(ctx, "https://pulse.mirea.ru"+item[0], item[1]) {
			t.Fatal("application resource allowed", item)
		}
		if blockedAuthResource(context.WithValue(ctx, diagnosticKey{}, true), "https://pulse.mirea.ru"+item[0], item[1]) {
			t.Fatal("explicit admin recording broken")
		}
	}
	for _, item := range [][2]string{{"https://pulse.mirea.ru/api/auth/login", "document"}, {"https://pulse.mirea.ru/signin-oidc", "document"}, {"https://pulse.mirea.ru/services", "document"}, {"https://sso.mirea.ru/theme/login.js", "script"}, {"https://pulse.mirea.ru/.well-known/ddos-guard/challenge.js", "script"}, {"https://check.ddos-guard.net/check.js", "script"}, {"https://pulse.mirea.ru" + "/rtu_tc.user.api.UserService/GetUserInfo", "fetch"}} {
		if blockedAuthResource(ctx, item[0], item[1]) {
			t.Fatal("authentication resource blocked", item)
		}
	}
}

func TestChromiumBlocksPulseRuntimeBeforeTransfer(t *testing.T) {
	directory := os.Getenv("TEST_BROWSER_DRIVER")
	if directory == "" {
		t.Skip("TEST_BROWSER_DRIVER required")
	}
	engine := New(context.Background(), Config{DriverDirectory: directory})
	defer engine.Close()
	c, err := engine.newContext(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var resources atomic.Int64
	if err := c.Route("**/*", func(route pw.Route) {
		if route.Request().URL() == "https://pulse.mirea.ru/services" {
			_ = route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), ContentType: pw.String("text/html"), Body: `<script>window.done=false; Promise.allSettled([fetch('/_framework/dotnet.native.wasm'), fetch('/_content/runtime.bin')]).then(()=>window.done=true)</script>`})
			return
		}
		if strings.Contains(route.Request().URL(), "/_framework/") || strings.Contains(route.Request().URL(), "/_content/") {
			resources.Add(1)
			_ = route.Fallback()
		} else {
			_ = route.Abort()
		}
	}); err != nil {
		t.Fatal(err)
	}
	page, err := c.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto("https://pulse.mirea.ru/services"); err != nil {
		t.Fatal(err)
	}
	handle, err := page.WaitForFunction("window.done===true", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Dispose()
	if resources.Load() != 2 {
		t.Fatal("fixture did not request the runtime")
	}
	if count := engine.traffic.Snapshot()["blocked_auth_resources"]; count != uint64(2) {
		t.Fatal("runtime reached the network", count)
	}
	// The interactive login page must be identified without waiting for ten retries.
	if err := page.Route("https://sso.mirea.ru/**", func(route pw.Route) {
		_ = route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), ContentType: pw.String("text/html"), Body: `<script>window.kcContext={pageId:'login.ftl',QRauthToken:'synthetic'};</script>`})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto("https://sso.mirea.ru/realms/mirea/login"); err != nil {
		t.Fatal(err)
	}
	if !reauthenticationRequired(page) {
		t.Fatal("expired SSO was not detected")
	}
}
