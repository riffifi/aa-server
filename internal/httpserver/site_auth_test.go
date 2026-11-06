package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rawRequest(s *Server, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://testserver"+path, nil)
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.serveHTTP(w, r)
	return w
}
func TestSiteStateCookieExchangeGroupAndLogout(t *testing.T) {
	uid := fmt.Sprintf("site-%d", time.Now().UnixNano())
	calls := 0
	s, v := setup(t, func(c *Config) {
		c.PublicURL = "https://aa.example"
		c.AAIDURL = "https://id.example"
		c.Exchange = func(context.Context, string) (map[string]any, error) {
			calls++
			return map[string]any{"id": "synthetic-aaid", "mirea": uid, "group_id": json.Number("5330"), "group_name": "Группа"}, nil
		}
	})
	row := account(t, v, uid)
	start := rawRequest(s, "GET", "/auth/aaid?next=/today&fresh=1", nil)
	if start.Code != 303 {
		t.Fatal(start.Body.String())
	}
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := location.Query().Get("state")
	if location.Host != "id.example" || location.Query().Get("fresh") != "1" || len(state) != 32 {
		t.Fatal("AA ID redirect changed")
	}
	for _, cookie := range start.Result().Cookies() {
		if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/auth" {
			t.Fatal("unsafe state cookie")
		}
	}
	code := "synthetic-code-at-least-20-chars"
	failed := rawRequest(s, "GET", "/auth/callback?code="+code+"&state=wrong", map[string]string{"Cookie": "aa_state=" + state})
	if failed.Code != 303 || failed.Header().Get("Location") != "/?signin=failed" || calls != 0 {
		t.Fatal("state mismatch reached code exchange")
	}
	callback := rawRequest(s, "GET", "/auth/callback?code="+code+"&state="+state, map[string]string{"Cookie": "aa_state=" + state + "; aa_next=/today"})
	if callback.Code != 303 || callback.Header().Get("Location") != "/today" || calls != 1 {
		t.Fatalf("callback failed: %d %s", callback.Code, callback.Body.String())
	}
	var site *http.Cookie
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == "aa_site" {
			site = cookie
		}
	}
	if site == nil || !site.HttpOnly || !site.Secure || site.SameSite != http.SameSiteStrictMode {
		t.Fatal("site cookie changed")
	}
	headers := map[string]string{"Cookie": "aa_site=" + site.Value}
	response, _ := request(t, s, "GET", "/api/accounts/"+row["id"].(string), "", headers)
	if response.Code != 200 {
		t.Fatal("site owner was not authenticated")
	}
	fresh, err := v.MetadataAccount(context.Background(), row["id"].(string))
	if err != nil || fresh["group_id"] != int64(5330) || fresh["page_seen_at"] == nil {
		t.Fatal("callback did not preserve group/page-seen behavior")
	}
	response, _ = request(t, s, "POST", "/site/logout", "", headers)
	if response.Code != 200 {
		t.Fatal("site logout failed")
	}
	response, _ = request(t, s, "GET", "/api/accounts/"+row["id"].(string), "", headers)
	if response.Code != 401 {
		t.Fatal("site session survived logout")
	}
}

func TestStaticETagRevalidationAndDirectoryBoundaries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "site.js"), []byte("synthetic script"), 0600); err != nil {
		t.Fatal(err)
	}
	s, _ := setup(t, func(c *Config) { c.StaticDir = root })
	first := rawRequest(s, "GET", "/static/site.js", nil)
	if first.Code != 200 || first.Header().Get("ETag") == "" || first.Header().Get("Cache-Control") != "public, max-age=0, must-revalidate" {
		t.Fatal("static cache headers missing")
	}
	second := rawRequest(s, "GET", "/static/site.js", map[string]string{"If-None-Match": first.Header().Get("ETag")})
	if second.Code != 304 || second.Body.Len() != 0 {
		t.Fatal("unchanged asset was downloaded again")
	}
	if directory := rawRequest(s, "GET", "/static/", nil); directory.Code != 404 {
		t.Fatal("static directory listing exposed")
	}
	if health := rawRequest(s, "HEAD", "/health", nil); health.Code != 405 {
		t.Fatal("GET-only API accepted HEAD")
	}
	if method := rawRequest(s, "POST", "/health", nil); method.Code != 405 {
		t.Fatal("method mismatch did not return 405")
	}
}
