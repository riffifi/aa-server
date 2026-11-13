package pulse

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var sessionCookie = regexp.MustCompile(`^(?:\.AspNetCore\.Cookies|Pulse\.Auth\.Cookie)(?:C[1-9][0-9]*)?$`)

// IsSessionCookie recognizes current Pulse tickets and legacy ASP.NET tickets,
// including chunks. Challenge/analytics cookies never determine auth expiry.
func IsSessionCookie(name string) bool { return sessionCookie.MatchString(name) }

func HasSessionCookie(header string) bool {
	for _, c := range (&http.Request{Header: http.Header{"Cookie": {header}}}).Cookies() {
		if (c.Name == "Pulse.Auth.Cookie" || c.Name == ".AspNetCore.Cookies") && c.Value != "" {
			return true
		}
	}
	return false
}

// MergeSessionCookies mirrors the native WebView's cookie persistence without
// inventing an upstream session lifetime. Only root cookies issued by Pulse
// are accepted; cookies for SSO, foreign domains or other paths are untouched.
// The result is a deep copy, so a concurrent reader's browser state is safe.
func MergeSessionCookies(secret map[string]any, headers http.Header, now time.Time) (map[string]any, bool, error) {
	patch := []*http.Cookie{}
	auth := false
	for _, c := range (&http.Response{Header: headers}).Cookies() {
		domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
		if domain != "" && domain != "pulse.mirea.ru" && domain != "mirea.ru" {
			continue
		}
		if c.Path != "/" {
			continue
		}
		if strings.HasPrefix(c.Name, ".AspNetCore.Correlation.") || strings.HasPrefix(c.Name, ".AspNetCore.OpenIdConnect.Nonce.") {
			continue
		}
		if c.Value != "" && ValidateCookie(c.Name+"="+c.Value) != nil {
			continue
		}
		patch = append(patch, c)
		auth = auth || sessionCookie.MatchString(c.Name)
	}
	if len(patch) == 0 {
		return nil, false, nil
	}
	old, _ := secret["cookie"].(string)
	values := map[string]string{}
	for _, c := range (&http.Request{Header: http.Header{"Cookie": {old}}}).Cookies() {
		values[c.Name] = c.Value
	}
	// A new main ticket replaces its complete chunk family; expired old chunks
	// must not shadow a newly issued unchunked cookie.
	for _, c := range patch {
		if c.Name == ".AspNetCore.Cookies" || c.Name == "Pulse.Auth.Cookie" {
			for name := range values {
				if name == c.Name || strings.HasPrefix(name, c.Name+"C") && sessionCookie.MatchString(name) {
					delete(values, name)
				}
			}
		}
	}
	var expiry any = secret["expires_at"]
	if auth {
		expiry = nil
	}
	for _, c := range patch {
		if c.MaxAge < 0 || c.Value == "" || c.MaxAge == 0 && !c.Expires.IsZero() && !c.Expires.After(now) {
			delete(values, c.Name)
			continue
		}
		values[c.Name] = c.Value
		if sessionCookie.MatchString(c.Name) {
			deadline := c.Expires
			if c.MaxAge > 0 {
				deadline = now.Add(time.Duration(min(c.MaxAge, 2147483647)) * time.Second)
			}
			if !deadline.IsZero() {
				seconds := float64(deadline.Unix())
				if expiry == nil || seconds < cookieSeconds(expiry) {
					expiry = seconds
				}
			}
		}
	}
	for _, family := range []string{".AspNetCore.Cookies", "Pulse.Auth.Cookie"} {
		base := values[family]
		if !strings.HasPrefix(base, "chunks-") {
			continue
		}
		count, err := strconv.Atoi(strings.TrimPrefix(base, "chunks-"))
		if err != nil || count < 1 || count > 32 {
			return nil, false, ErrCookie
		}
		for i := 1; i <= count; i++ {
			if values[family+"C"+strconv.Itoa(i)] == "" {
				return nil, false, ErrCookie
			}
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := []string{}
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	header := strings.Join(parts, "; ")
	if header == old && cookieSeconds(expiry) == cookieSeconds(secret["expires_at"]) {
		return nil, false, nil
	}
	if header != "" && ValidateCookie(header) != nil {
		return nil, false, ErrCookie
	}
	data, err := json.Marshal(secret["storage_state"])
	if err != nil {
		return nil, false, err
	}
	storage := map[string]any{}
	if string(data) != "null" {
		if err := json.Unmarshal(data, &storage); err != nil {
			return nil, false, err
		}
	}
	if storage == nil {
		storage = map[string]any{}
	}
	cookies, _ := storage["cookies"].([]any)
	kept := []any{}
	previous := map[string]map[string]any{}
	for _, item := range cookies {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		domain, _ := c["domain"].(string)
		domain = strings.TrimPrefix(strings.ToLower(domain), ".")
		if (domain == "pulse.mirea.ru" || domain == "mirea.ru") && c["path"] == "/" {
			if name, ok := c["name"].(string); ok {
				previous[name] = c
			}
			continue // Rebuild Pulse root cookies from the merged header below.
		}
		kept = append(kept, c)
	}
	for _, name := range names {
		c := map[string]any{"name": name, "value": values[name], "domain": "pulse.mirea.ru", "path": "/", "expires": float64(-1), "httpOnly": true, "secure": true, "sameSite": "Lax"}
		if old := previous[name]; old != nil && old["value"] == values[name] {
			c = old
		}
		// Keep session cookies as session cookies, just as CookieManager.setCookie
		// does when restoring its saved Cookie header. This does not extend the
		// server's encrypted ticket lifetime or bypass upstream revocation.
		for _, changed := range patch {
			if changed.Name == name {
				c["httpOnly"], c["secure"] = changed.HttpOnly, changed.Secure
				c["domain"] = "pulse.mirea.ru"
				if changed.Domain != "" {
					c["domain"] = "." + strings.TrimPrefix(strings.ToLower(changed.Domain), ".")
				}
				c["expires"] = float64(-1)
				c["sameSite"] = "Lax"
				if changed.SameSite == http.SameSiteStrictMode {
					c["sameSite"] = "Strict"
				}
				if changed.SameSite == http.SameSiteNoneMode && changed.Secure {
					c["sameSite"] = "None"
				}
				if changed.MaxAge > 0 {
					c["expires"] = float64(now.Add(time.Duration(min(changed.MaxAge, 2147483647)) * time.Second).Unix())
				} else if !changed.Expires.IsZero() {
					c["expires"] = float64(changed.Expires.Unix())
				}
			}
		}
		kept = append(kept, c)
	}
	storage["cookies"] = kept
	if _, ok := storage["origins"]; !ok {
		storage["origins"] = []any{}
	}
	return map[string]any{"cookie": header, "expires_at": expiry, "storage_state": storage}, auth, nil
}

func cookieSeconds(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case json.Number:
		n, _ := v.Float64()
		return n
	case int64:
		return float64(v)
	}
	return 0
}
