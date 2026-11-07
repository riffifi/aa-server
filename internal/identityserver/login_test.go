package identityserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/cryptocompat"
)

type loginRunner func(context.Context, func(browser.Update)) (*browser.Result, error)

func (f loginRunner) Login(ctx context.Context, update func(browser.Update)) (*browser.Result, error) {
	return f(ctx, update)
}
func TestQRLoginClaimHandsOffOnceAndCreatesPrivateSession(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "web"
		if native {
			name = "app"
		}
		t.Run(name, func(t *testing.T) { testQRLoginClaim(t, native) })
	}
}
func testQRLoginClaim(t *testing.T, native bool) {
	base := "/web/logins"
	if native {
		base = "/v1/auth/logins"
	}

	var uid string
	release := make(chan struct{})
	group := int64(5330)
	name := "Группа"
	var transfers atomic.Int64
	seed := make([]byte, 32)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transfers.Add(1)
		if r.URL.Path != "/internal/aaid/link" || r.Header.Get("Cookie") != "" {
			t.Error("invalid hand-off endpoint/headers")
		}
		claims, err := cryptocompat.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), map[string]ed25519.PublicKey{cryptocompat.KeyID(public): public}, "aa-id", "aa-link", time.Now(), true)
		if err != nil || claims["mirea"] != uid {
			t.Error("hand-off token invalid", err)
		}
		var data map[string]any
		if json.NewDecoder(r.Body).Decode(&data) != nil || data["cookie"] != "session=synthetic" || data["group_id"] != float64(5330) {
			t.Error("fresh session/group lost")
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	jobs := browser.NewJobs(context.Background(), loginRunner(func(ctx context.Context, update func(browser.Update)) (*browser.Result, error) {
		update(browser.Update{Status: "login", QR: "https://sso.mirea.ru/realms/mirea/qr-code-auth/scan?token=synthetic"})
		select {
		case <-release:
			return &browser.Result{UID: uid, GroupID: &group, GroupName: &name, Credentials: map[string]any{"cookie": "session=synthetic", "storage_state": map[string]any{"cookies": []any{}}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	defer jobs.Close()
	s, store, pulseID := setup(t, func(c *Config) {
		c.Logins = jobs
		c.PublicURL = "https://id.example"
		c.SessionsURL = upstream.URL
		c.HTTP = upstream.Client()
	})
	uid = pulseID
	w, started := request(t, s, "POST", base, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := started["id"].(string)
	token := started["token"].(string)
	headers := map[string]string{"X-Login-Token": token}
	w, _ = request(t, s, "GET", base+"/"+id, "", map[string]string{"X-Login-Token": "wrong"})
	if w.Code != 404 {
		t.Fatal("wrong capability accepted")
	}
	w, _ = request(t, s, "POST", base+"/"+id+"/claim", "", headers)
	if w.Code != 409 {
		t.Fatal("pending login was claimed")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, _ := jobs.State(id, token)
		if state.QR != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	w, state := request(t, s, "GET", base+"/"+id, "", headers)
	if w.Code != 200 || state["qr"] == nil || !strings.HasPrefix(state["qr_png"].(string), "data:image/png;base64,") {
		t.Fatal(w.Body.String())
	}
	close(release)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, _ := jobs.State(id, token)
		if state.Status == "complete" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	headers["User-Agent"] = "Mozilla Chrome/120 Windows"
	w, claimed := request(t, s, "POST", base+"/"+id+"/claim", "", headers)
	if w.Code != 200 || claimed["linked"] != true || transfers.Load() != 1 {
		t.Fatal(w.Body.String())
	}
	if native {
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("app login received a browser cookie")
		}
		if strings.Contains(w.Body.String(), "session=synthetic") || strings.Contains(w.Body.String(), "storage_state") {
			t.Fatal("browser credentials leaked to app")
		}
		access := claimed["access_token"].(string)
		auth := map[string]string{"Authorization": "Bearer " + access}
		check, me := request(t, s, "GET", "/web/me", "", auth)
		if check.Code != 200 || me["id"] != claimed["user"].(map[string]any)["id"] {
			t.Fatal("shared web API rejected app identity", check.Body.String())
		}
		check, _ = request(t, s, "POST", "/web/logout", "", auth)
		if check.Code != 200 {
			t.Fatal(check.Body.String())
		}
		check, _ = request(t, s, "GET", "/web/me", "", auth)
		if check.Code != 401 {
			t.Fatal("app session survived shared logout")
		}
	} else {
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != "aaid_web" || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
			t.Fatal("web cookie changed")
		}
		bad, _ := request(t, s, "GET", "/web/me", "", map[string]string{"Cookie": "aaid_web=" + cookies[0].Value, "Authorization": "Bearer invalid"})
		if bad.Code != 401 {
			t.Fatal("invalid bearer fell back to a valid browser cookie")
		}
		session, err := store.WebSession(context.Background(), cookies[0].Value)
		if err != nil || session == nil || session["device"] != "Chrome · Windows" {
			t.Fatal("web session missing", session, err)
		}
	}
	w, _ = request(t, s, "POST", base+"/"+id+"/claim", "", headers)
	if w.Code != 409 || transfers.Load() != 1 {
		t.Fatal("login handed off more than once")
	}
}
