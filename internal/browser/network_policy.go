package browser

import (
	"context"
	"net/url"
	"path"
	"strings"
)

// Authentication uses server-rendered SSO pages and direct Pulse RPCs. Loading
// the Pulse Blazor application is unnecessary and can download hundreds of MB
// in every fresh browser context. Explicit admin recording still needs the UI.
func blockedAuthResource(ctx context.Context, raw, kind string) bool {
	if diagnostic, _ := ctx.Value(diagnosticKey{}).(bool); diagnostic {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "pulse.mirea.ru" {
		return false
	}
	p := strings.ToLower(u.Path)
	// Preserve anti-bot challenge assets; they are part of authentication.
	if strings.HasPrefix(p, "/.well-known/ddos-guard/") {
		return false
	}
	if strings.HasPrefix(p, "/_framework/") || strings.HasPrefix(p, "/_content/") {
		return true
	}
	switch path.Ext(p) {
	case ".wasm", ".dll", ".dat", ".bin":
		return true
	}
	switch kind {
	case "script", "stylesheet", "image", "media", "font":
		return true
	}
	return false
}
