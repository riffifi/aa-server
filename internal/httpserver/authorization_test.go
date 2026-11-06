package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aa-server/internal/pulse"
)

func secureRequest(t *testing.T, s *Server, method, path, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, "http://testserver"+path, strings.NewReader(body))
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid secure response %d %s", w.Code, w.Body.String())
	}
	return w, result
}
func TestStrictBoundaryRejectsAnonymousEvenWithoutVerifier(t *testing.T) {
	s, _ := setup(t, nil)
	for _, path := range []string{"/api/accounts", "/api/groups", "/api/groups/search?q=test", "/api/events?wait=0", "/api/me/account", "/api/unknown", "/site/schedule", "/site/state", "/panel/api/state", "/panel/api/logins", "/panel/api/debug/tables"} {
		for _, method := range []string{"GET", "POST", "HEAD"} {
			w, _ := secureRequest(t, s, method, path, `{}`, map[string]string{"X-Pass-Test-Key": "not-an-authentication-credential", "X-Pass-Client": "not-a-login"})
			if w.Code != 401 {
				t.Fatalf("anonymous %s %s returned %d", method, path, w.Code)
			}
		}
	}
	w, health := secureRequest(t, s, "GET", "/health", "", nil)
	if w.Code != 200 || health["aaid"].(map[string]any)["required"] != true {
		t.Fatal(w.Body.String())
	}
}

func TestTelegramEndpointsAreDisabled(t *testing.T) {
	s, _ := setup(t, func(c *Config) { c.StaticDir = t.TempDir() })
	for _, path := range []string{"/tg/connect", "/site/telegram-link", "/api/accounts/synthetic/pass-request", "/static/telegram.html", "/static/telegram.js"} {
		w, _ := secureRequest(t, s, "GET", path, "", nil)
		if w.Code != 404 {
			t.Fatalf("disabled Telegram route %s returned %d", path, w.Code)
		}
	}
	w, _ := secureRequest(t, s, "GET", "/panel/api/debug/browser", "", nil)
	if w.Code != 401 {
		t.Fatal("browser metrics exposed without panel login")
	}
}
func TestStrictSnapshotsEventsAndRPCEnforceOwnerAndExplicitPassShare(t *testing.T) {
	prefix := fmt.Sprintf("policy-%d-", time.Now().UnixNano())
	ownerUID, guestUID, otherUID := prefix+"owner", prefix+"guest", prefix+"other"
	upstream := &fakePulse{reply: grpcReply(nil)}
	s, v := setup(t, func(c *Config) {
		c.Pulse = upstream
		c.Verify = func(_ context.Context, token string) (map[string]any, error) {
			if token == "owner" {
				return map[string]any{"mirea": ownerUID, "sub": "owner"}, nil
			}
			if token == "guest" {
				return map[string]any{"mirea": guestUID, "sub": "guest"}, nil
			}
			return nil, nil
		}
	})
	owner := account(t, v, ownerUID)
	guest := account(t, v, guestUID)
	other := account(t, v, otherUID)
	ownerID, guestID, otherID := owner["id"].(string), guest["id"].(string), other["id"].(string)
	for _, id := range []string{ownerID, guestID, otherID} {
		if _, err := v.SetPassAllowed(context.Background(), id, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.SavePass(context.Background(), ownerID, "123456", nil); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer guest"}
	w, snapshot := secureRequest(t, s, "GET", "/api/accounts", "", headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(snapshot["accounts"].([]any)) != 1 || snapshot["accounts"].([]any)[0].(map[string]any)["id"] != guestID {
		t.Fatal("snapshot leaked unrelated accounts", w.Body.String())
	}
	if err := v.SharePass(context.Background(), ownerID, guestID); err != nil {
		t.Fatal(err)
	}
	w, filtered := secureRequest(t, s, "GET", "/api/accounts?group_id=9999", "", headers)
	if w.Code != 200 || len(filtered["accounts"].([]any)) != 0 {
		t.Fatal("group filter returned another group", w.Body.String())
	}
	w, snapshot = secureRequest(t, s, "GET", "/api/accounts", "", headers)
	if len(snapshot["accounts"].([]any)) != 2 {
		t.Fatal("explicit shared account disappeared", w.Body.String())
	}
	for _, item := range snapshot["accounts"].([]any) {
		data := item.(map[string]any)
		if data["id"] == otherID {
			t.Fatal("unrelated account exposed")
		}
		if data["id"] == ownerID {
			if _, exists := data["expires_at"]; exists {
				t.Fatal("shared card exposed authentication expiry")
			}
		}
	}
	found := false
	for _, item := range snapshot["passes"].([]any) {
		data := item.(map[string]any)
		if data["account_id"] == ownerID && data["number"] == "123456" {
			found = true
		}
	}
	if !found {
		t.Fatal("approved pass missing")
	}
	for _, suffix := range []string{"", "/rpc/profile", "/rpc/acs-timeline", "/rpc/attendance", "/group", "/end-session", "/pass-allowed"} {
		method := "POST"
		if suffix == "" {
			method = "GET"
		}
		if strings.HasSuffix(suffix, "group") || strings.HasSuffix(suffix, "pass-allowed") {
			method = "PUT"
		}
		w, _ = secureRequest(t, s, method, "/api/accounts/"+ownerID+suffix, `{}`, headers)
		if w.Code != 404 {
			t.Fatalf("share authorized private endpoint %s: %d", suffix, w.Code)
		}
	}
	if upstream.calls != 0 {
		t.Fatal("unauthorized RPC reached Pulse")
	}
	r := httptest.NewRequest("POST", "http://testserver/api/accounts/"+ownerID+"/rpc/pass-token", strings.NewReader(string(pulse.Frame(nil, 0))))
	r.Header.Set("Authorization", "Bearer guest")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, r)
	if response.Code != 200 || upstream.calls != 1 {
		t.Fatal("explicit pass permission failed", response.Body.String())
	}
	w, events := secureRequest(t, s, "GET", "/api/events?wait=0", "", headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, item := range events["events"].([]any) {
		if item.(map[string]any)["account_id"] == otherID {
			t.Fatal("foreign event exposed")
		}
	}
	beforeRevoke, err := v.Cursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.UnsharePass(context.Background(), ownerID, guestID); err != nil {
		t.Fatal(err)
	}
	w, revoked := secureRequest(t, s, "GET", fmt.Sprintf("/api/events?after=%d&wait=0&group_id=9999", beforeRevoke), "", headers)
	if w.Code != 200 || len(revoked["events"].([]any)) == 0 {
		t.Fatal("revocation did not wake the filtered client", w.Body.String())
	}
	for _, item := range revoked["events"].([]any) {
		event := item.(map[string]any)
		if event["account_id"] != guestID {
			t.Fatal("revocation exposed another account", event)
		}
		if _, exists := event["audience_mirea"]; exists {
			t.Fatal("internal event audience leaked")
		}
	}
	w, snapshot = secureRequest(t, s, "GET", "/api/accounts", "", headers)
	if len(snapshot["accounts"].([]any)) != 1 || strings.Contains(w.Body.String(), "123456") {
		t.Fatal("revoked pass still exposed", w.Body.String())
	}
}

func TestVerifierErrorDoesNotAuthenticateAndWriteQuotaIsEnforced(t *testing.T) {
	s, _ := setup(t, func(c *Config) {
		c.Verify = func(_ context.Context, token string) (map[string]any, error) {
			claims := map[string]any{"sub": "rate-test", "mirea": "rate-test"}
			if token == "error" {
				return claims, errors.New("invalid signature")
			}
			return claims, nil
		}
	})
	w, _ := secureRequest(t, s, "GET", "/api/accounts", "", map[string]string{"Authorization": "Bearer error"})
	if w.Code != 401 {
		t.Fatal("verifier error ignored")
	}
	headers := map[string]string{"Authorization": "Bearer valid"}
	for i := 0; i < 60; i++ {
		w, _ = secureRequest(t, s, "POST", "/api/not-implemented", "", headers)
		if w.Code == 429 {
			t.Fatal("quota reached too early")
		}
	}
	w, _ = secureRequest(t, s, "POST", "/api/not-implemented", "", headers)
	if w.Code != 429 || w.Result().Header.Get("Retry-After") != "60" {
		t.Fatal("write quota not enforced", w.Code)
	}
}
func TestStrictImportCannotPublishAnotherPersonsSessionAndCookiesDoNotMaskBadBearer(t *testing.T) {
	uid := fmt.Sprintf("strict-import-%d", time.Now().UnixNano())
	upstream := &fakePulse{identityUID: "someone-else"}
	s, v := setup(t, func(c *Config) {
		c.Pulse = upstream
		c.Verify = func(context.Context, string) (map[string]any, error) { return nil, nil }
	})
	account(t, v, uid)
	token, err := v.CreateSiteSession(context.Background(), "synthetic", uid, nil, nil, 3600)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "aa_site=" + token}
	w, _ := secureRequest(t, s, "POST", "/api/accounts/import", `{"cookie":"session=other","label":"Other"}`, headers)
	if w.Code != 403 {
		t.Fatal("another person's import accepted", w.Body.String())
	}
	headers["Authorization"] = "Bearer invalid"
	w, _ = secureRequest(t, s, "GET", "/api/accounts", "", headers)
	if w.Code != 401 {
		t.Fatal("bad bearer fell back to cookie")
	}
}

func TestOwnGroupCannotBeForgedForClassAccess(t *testing.T) {
	uid := fmt.Sprintf("group-policy-%d", time.Now().UnixNano())
	s, v := setup(t, func(c *Config) {
		c.Pulse = &fakePulse{identityUID: uid}
		c.Verify = func(context.Context, string) (map[string]any, error) {
			return map[string]any{"mirea": uid, "sub": "synthetic", "grp": int64(1)}, nil
		}
	})
	row := account(t, v, uid)
	headers := map[string]string{"Authorization": "Bearer valid"}
	w, _ := secureRequest(t, s, "PUT", "/api/accounts/"+row["id"].(string)+"/group", `{"group_id":2,"expected_version":1}`, headers)
	if w.Code != 403 {
		t.Fatal("owner forged group membership")
	}
	w, _ = secureRequest(t, s, "POST", "/api/accounts/import", `{"cookie":"session=valid","label":"Owner","group_id":2}`, headers)
	if w.Code != 403 {
		t.Fatal("import forged group membership")
	}
	body := pulse.ACSTimelineRequest("someone-else", 1, 2)
	request := httptest.NewRequest("POST", "http://testserver/api/accounts/"+row["id"].(string)+"/rpc/acs-timeline", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("foreign turnstile uid accepted")
	}
}
