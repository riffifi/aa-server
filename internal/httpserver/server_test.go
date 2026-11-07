package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aa-server/internal/postgres"
	"aa-server/internal/schedule"
)

const password = "synthetic-admin-password"

func TestCapturedPythonHTTPContracts(t *testing.T) {
	s, v := setup(t, nil)
	v.Now = func() time.Time { return time.Unix(1793692800, 0) }
	var fixtures []struct {
		Method, Path, Body string
		Status             int
		Expected           map[string]any
	}
	data, err := os.ReadFile("testdata/python_http.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	// Historic sync events from other tests are not part of the empty snapshot fixture.
	var cursor int64
	if err = v.Pool.QueryRow(context.Background(), "SELECT COALESCE(MAX(id),0) FROM aa.events").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	for i, f := range fixtures {
		t.Run(fmt.Sprintf("%02d_%s_%s", i, f.Method, f.Path), func(t *testing.T) {
			path := f.Path
			if strings.Split(path, "?")[0] == "/api/accounts" {
				f.Status = 401
				f.Expected = map[string]any{"detail": "AA ID sign-in required", "aaid_url": s.config.AAIDURL}
			}
			if RetiredClientPath(strings.Split(path, "?")[0]) {
				f.Status = 404
				f.Expected = map[string]any{"detail": "Not Found"}
			}
			// Mandatory sign-in intentionally replaces the old optional-auth health flag.
			if path == "/health" {
				f.Expected["aaid"] = map[string]any{"required": true, "url": s.config.AAIDURL}
			}
			if path == "/api/events?wait=0" {
				path += fmt.Sprintf("&after=%d", cursor)
			}
			if _, exists := f.Expected["cursor"]; exists {
				f.Expected["cursor"] = float64(cursor)
			}
			w, actual := request(t, s, f.Method, path, f.Body, nil)
			if w.Code != f.Status || !reflect.DeepEqual(actual, f.Expected) {
				t.Errorf("HTTP parity mismatch: status=%d want=%d actual=%s expected=%v", w.Code, f.Status, w.Body.String(), f.Expected)
			}
		})
	}
}

func setup(t *testing.T, configure func(*Config)) (*Server, *postgres.Vault) {
	t.Helper()
	databaseURL := os.Getenv("TEST_HTTP_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_HTTP_DATABASE_URL is required")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, databaseURL, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	var name string
	if err = store.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "aa_test_") {
		t.Fatal("HTTP integration tests require an isolated aa_test_* database")
	}
	if err = store.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	key := base64.URLEncoding.EncodeToString(make([]byte, 32))
	v, err := postgres.NewVault(store, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.InitializeAdmin(ctx, password); err != nil {
		t.Fatal(err)
	}
	calendar := schedule.NewCache(ctx, func(context.Context, string) (string, error) { return "", nil })
	t.Cleanup(calendar.Close)
	cfg := Config{Vault: v, MasterKey: key, Calendars: calendar}
	if configure != nil {
		configure(&cfg)
	}
	server, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server, v
}

func request(t *testing.T, s *Server, method, path, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	// Legacy wire-contract harness. Strict listener authorization is tested through
	// Server.ServeHTTP in authorization_test.go, including rejected anonymous calls.
	t.Helper()
	r := httptest.NewRequest(method, "http://testserver"+path, strings.NewReader(body))
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.serveHTTP(w, r)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("non-JSON HTTP response: %d %s", w.Code, w.Body.String())
	}
	return w, result
}
func account(t *testing.T, v *postgres.Vault, uid string) map[string]any {
	t.Helper()
	row, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: uid, Label: "Студент", Credentials: map[string]any{"cookie": "session=synthetic", "storage_state": map[string]any{"cookies": []any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.DeleteAccount(context.Background(), row["id"].(string)) })
	return row
}

func TestAnonymousAndOwnerSnapshotsDoNotLeakCredentials(t *testing.T) {
	uid := fmt.Sprintf("snapshot-%d", time.Now().UnixNano())
	s, v := setup(t, func(c *Config) {
		c.AAIDURL = "https://id.example"
		c.Verify = func(ctx context.Context, token string) (map[string]any, error) {
			if token == "valid" {
				return map[string]any{"mirea": uid, "sub": "synthetic-aaid"}, nil
			}
			return nil, nil
		}
	})
	row := account(t, v, uid)
	id := row["id"].(string)
	if _, err := v.SetPassAllowed(context.Background(), id, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := v.SavePass(context.Background(), id, "987654321", nil); err != nil {
		t.Fatal(err)
	}
	w, _ := secureRequest(t, s, "GET", "/api/accounts", "", nil)
	if w.Code != 401 {
		t.Fatal("anonymous snapshot accepted")
	}
	headers := map[string]string{"Authorization": "Bearer valid"}
	w, result := secureRequest(t, s, "GET", "/api/accounts", "", headers)
	if w.Code != 200 || result["accounts"].([]any)[0].(map[string]any)["own"] != true {
		t.Fatal(w.Body.String())
	}
	for _, value := range []string{"987654321", "session=synthetic", "storage_state"} {
		if strings.Contains(w.Body.String(), value) {
			t.Fatal("credential or digital pass leaked", value)
		}
	}
	if _, exists := result["passes"]; exists {
		t.Fatal("legacy pass payload remains")
	}
	if _, err := v.SetPassAllowed(context.Background(), id, false, nil); err != nil {
		t.Fatal(err)
	}
	w, result = secureRequest(t, s, "GET", "/api/accounts", "", headers)
	if w.Code != 200 || result["accounts"].([]any)[0].(map[string]any)["pass_access"] != "disabled" {
		t.Fatal("owner permission ignored")
	}

}

func TestPanelLoginCSRFAndOptimisticRename(t *testing.T) {
	s, v := setup(t, nil)
	row := account(t, v, fmt.Sprintf("panel-%d", time.Now().UnixNano()))
	id := row["id"].(string)
	w, _ := request(t, s, "PUT", "/panel/api/accounts/"+id+"/label", `{"label":"New"}`, nil)
	if w.Code != 401 {
		t.Fatal("unauthorized write accepted")
	}
	w, login := request(t, s, "POST", "/panel/api/login", `{"password":"`+password+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login failed: %s", w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Path != "/panel" || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("panel cookie changed")
	}
	headers := map[string]string{"Cookie": cookies[0].Name + "=" + cookies[0].Value}
	w, _ = request(t, s, "PUT", "/panel/api/accounts/"+id+"/label", `{"label":"New"}`, headers)
	if w.Code != 403 {
		t.Fatal("CSRF protection missing")
	}
	headers["X-CSRF-Token"] = login["csrf"].(string)
	w, renamed := request(t, s, "PUT", "/panel/api/accounts/"+id+"/label", `{"label":" Новое имя ","expected_version":1}`, headers)
	if w.Code != 200 || renamed["label"] != "Новое имя" || renamed["version"] != float64(2) {
		t.Fatalf("rename failed: %s", w.Body.String())
	}
	w, _ = request(t, s, "PUT", "/panel/api/accounts/"+id+"/label", `{"label":"Stale","expected_version":1}`, headers)
	if w.Code != 409 {
		t.Fatal("stale version accepted")
	}
	w, _ = request(t, s, "POST", "/panel/api/logout", "", headers)
	if w.Code != 200 {
		t.Fatal("logout failed")
	}
	w, _ = request(t, s, "GET", "/panel/api/state", "", headers)
	if w.Code != 401 {
		t.Fatal("revoked panel session accepted")
	}
}

func TestEventsWakeOnPostgresCommitAndKeepGroupCursor(t *testing.T) {
	s, v := setup(t, nil)
	row := account(t, v, fmt.Sprintf("events-%d", time.Now().UnixNano()))
	id := row["id"].(string)
	cursor, err := v.Cursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("GET", fmt.Sprintf("http://testserver/api/events?after=%d&wait=5", cursor), nil)
		w := httptest.NewRecorder()
		s.serveHTTP(w, r)
		done <- w
	}()
	if _, err = v.Rename(context.Background(), id, "Changed", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var result map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		events := result["events"].([]any)
		if len(events) != 1 || events[0].(map[string]any)["kind"] != "label" {
			t.Fatalf("event lost: %s", w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification did not wake long polling")
	}
	w, result := request(t, s, "GET", fmt.Sprintf("/api/events?after=%d&wait=0&group_id=12345", cursor), "", nil)
	if w.Code != 200 || len(result["events"].([]any)) != 0 || result["cursor"].(float64) <= float64(cursor) {
		t.Fatal("filtered cursor stopped advancing")
	}
}

func TestHealthAuthenticationAndPanelNetworkBoundary(t *testing.T) {
	s, _ := setup(t, func(c *Config) {
		c.EnforceAAID = true
		c.PanelLocalOnly = true
		c.AAIDURL = "https://id.example"
		c.Verify = func(context.Context, string) (map[string]any, error) { return nil, nil }
	})
	w, result := request(t, s, "GET", "/health", "", nil)
	if w.Code != 200 || result["protocol"] != float64(2) || result["aaid"].(map[string]any)["required"] != true {
		t.Fatal("health contract changed")
	}
	w, _ = request(t, s, "GET", "/api/accounts", "", nil)
	if w.Code != 401 || w.Header().Get("X-AAID-Required") != "1" {
		t.Fatal("AA ID enforcement missing")
	}
	w, _ = request(t, s, "POST", "/panel/api/login", `{"password":"`+password+`"}`, nil)
	if w.Code != 404 {
		t.Fatal("panel network boundary missing")
	}
	w, _ = request(t, s, "POST", "/panel/api/login", `{"password":"`+password+`"}`, map[string]string{"X-AA-Panel": "1"})
	if w.Code != 200 {
		t.Fatal("local panel denied")
	}
}
