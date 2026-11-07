package identityserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aa-server/internal/cryptocompat"
)

func TestWebCodeIsSingleUseAndSessionLogoutRevokesCookie(t *testing.T) {
	s, store, pulseID := setup(t, func(c *Config) { c.SiteURL = "https://aa.example" })
	user, err := store.BindMirea(context.Background(), pulseID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := store.CreateSession(context.Background(), user["id"].(string), "browser", 3600, 1, "web")
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "aaid_web=" + token}
	w, me := request(t, s, "GET", "/web/me", "", headers)
	if w.Code != 200 || me["site_url"] != "https://aa.example" {
		t.Fatal(w.Body.String())
	}
	w, result := request(t, s, "POST", "/web/authorize", `{"state":"synthetic_state_1234567890"}`, headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	redirect, err := url.Parse(result["redirect"].(string))
	if err != nil || redirect.Host != "aa.example" || redirect.Query().Get("state") != "synthetic_state_1234567890" {
		t.Fatal("site callback redirect changed")
	}
	body := `{"code":"` + redirect.Query().Get("code") + `"}`
	w, person := request(t, s, "POST", "/v1/site/exchange", body, nil)
	if w.Code != 200 || person["id"] != user["id"] || person["mirea"] != pulseID {
		t.Fatal(w.Body.String())
	}
	w, _ = request(t, s, "POST", "/v1/site/exchange", body, nil)
	if w.Code != 400 {
		t.Fatal("site code was reused")
	}
	w, _ = request(t, s, "POST", "/web/logout", "", headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w, _ = request(t, s, "GET", "/web/me", "", headers)
	if w.Code != 401 {
		t.Fatal("web cookie survived logout")
	}
}

func TestCallbackCodeCannotOutliveItsOriginatingSession(t *testing.T) {
	s, store, pulseID := setup(t, func(c *Config) { c.SiteURL = "https://aa.ytc-srv.org" })
	user, err := store.BindMirea(context.Background(), pulseID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, token, err := store.CreateSession(context.Background(), user["id"].(string), "browser", 3600, 3, "web")
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "aaid_web=" + token}
	w, data := request(t, s, "POST", "/web/authorize", `{"state":"synthetic_state_1234567890"}`, headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	location, err := url.Parse(data["redirect"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(context.Background(), user["id"].(string), session["id"].(string), "test"); err != nil {
		t.Fatal(err)
	}
	w, _ = request(t, s, "POST", "/v1/site/exchange", `{"code":"`+location.Query().Get("code")+`"}`, nil)
	if w.Code != 401 {
		t.Fatal("code from revoked session redeemed", w.Body.String())
	}
}

func TestWebProxyForwardsOnlyShortLivedJWTAndPublicFields(t *testing.T) {
	seed := make([]byte, 32)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" {
			t.Error("AA ID cookie leaked to session server")
		}
		claims, err := cryptocompat.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), map[string]ed25519.PublicKey{cryptocompat.KeyID(public): public}, "aa-id", "aa", time.Now(), true)
		if err != nil {
			t.Error("short JWT missing or invalid")
		} else {
			expiry, _ := claims["exp"].(json.Number).Int64()
			if expiry > time.Now().Unix()+60 {
				t.Error("proxy token exceeded one minute")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/me/account" {
			_ = json.NewEncoder(w).Encode(map[string]any{"account": true, "connected": true, "label": "Owner", "pass_allowed": false, "group_name": "Group", "secret": "must-not-leak"})
			return
		}
		if r.URL.Path == "/api/me/pass/people" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || body["account_id"] != "target" {
				t.Error("unvalidated body fields were forwarded")
			}
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "Человек не найден"})
			return
		}
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	client := upstream.Client()
	defer client.CloseIdleConnections()
	s, store, pulseID := setup(t, func(c *Config) { c.SessionsURL = upstream.URL; c.HTTP = client })
	user, err := store.BindMirea(context.Background(), pulseID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := store.CreateSession(context.Background(), user["id"].(string), "browser", 3600, 1, "web")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := request(t, s, "GET", "/web/account", "", nil)
	if w.Code != 401 || calls.Load() != 0 {
		t.Fatal("unauthenticated web proxy reached upstream")
	}
	headers := map[string]string{"Cookie": "aaid_web=" + token}
	w, account := request(t, s, "GET", "/web/account", "", headers)
	if w.Code != 200 || len(account) != 5 || strings.Contains(w.Body.String(), "must-not-leak") {
		t.Fatal("private upstream fields were exposed")
	}
	w, problem := request(t, s, "POST", "/web/pass/people", `{"account_id":"target","secret":"do-not-forward"}`, headers)
	if w.Code != 404 || problem["detail"] != "Человек не найден" {
		t.Fatal("human-readable upstream error changed")
	}
	before := calls.Load()
	headers["Origin"] = "https://evil.example"
	w, _ = request(t, s, "PUT", "/web/account", `{"label":"Changed"}`, headers)
	if w.Code != 403 || calls.Load() != before {
		t.Fatal("cross-origin write reached upstream")
	}
}
