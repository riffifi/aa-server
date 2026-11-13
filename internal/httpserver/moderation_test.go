package httpserver

import (
	"aa-server/internal/browser"
	"aa-server/internal/postgres"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type heldRenewal struct{ started chan struct{} }

func (b *heldRenewal) Login(context.Context, func(browser.Update)) (*browser.Result, error) {
	return nil, nil
}
func (b *heldRenewal) Renew(ctx context.Context, _ map[string]any) (*browser.Result, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBanCancelsActiveRenewal(t *testing.T) {
	uid := fmt.Sprintf("ban-renew-%d", time.Now().UnixNano())
	runner := &heldRenewal{started: make(chan struct{})}
	s, v := setup(t, func(c *Config) { c.Browser = runner; c.Pulse = &fakePulse{identityUID: uid} })
	ctx := context.Background()
	row, err := v.Upsert(ctx, postgres.Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "session=old", "renewed_at": float64(1), "storage_state": map[string]any{"cookies": []any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	defer v.DeleteAccount(ctx, id)
	defer v.Pool.Exec(ctx, "DELETE FROM aa.mirea_bans WHERE pulse_uid=$1", uid)
	done := make(chan error, 1)
	go func() { _, err := s.refresh(ctx, id); done <- err }()
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal did not start")
	}
	if _, err := v.Pool.Exec(ctx, "SELECT aa.set_mirea_ban($1,true,'test')", uid); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, postgres.ErrBlocked) {
			t.Fatal("ban did not terminate renewal", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("banned renewal was not canceled")
	}
}

func TestBanFreezesExistingAndFutureMireaAccounts(t *testing.T) {
	ctx := context.Background()
	uid := fmt.Sprintf("ban-%d", time.Now().UnixNano())
	upstream := &fakePulse{identityUID: uid}
	s, v := setup(t, func(c *Config) {
		c.Pulse = upstream
		c.Verify = func(context.Context, string) (map[string]any, error) {
			return map[string]any{"mirea": uid, "sub": uid}, nil
		}
	})
	row := account(t, v, uid)
	id := row["id"].(string)
	identityStore := postgres.NewIdentity(v.Store)
	user, err := identityStore.BindMirea(ctx, uid, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID := user["id"].(string)
	defer func() {
		v.Pool.Exec(ctx, "DELETE FROM aa.mirea_bans WHERE pulse_uid=$1", uid)
		for _, q := range []string{"DELETE FROM aaid.refresh_tokens WHERE session_id IN(SELECT id FROM aaid.sessions WHERE user_id=$1)", "DELETE FROM aaid.sessions WHERE user_id=$1", "DELETE FROM aaid.mirea_links WHERE user_id=$1", "DELETE FROM aaid.users WHERE id=$1"} {
			v.Pool.Exec(ctx, q, userID)
		}
	}()
	session, _, err := identityStore.CreateSession(ctx, userID, "test", 3600, 2, "app")
	if err != nil {
		t.Fatal(err)
	}
	logins, err := v.UsageList(ctx, 0, uid, "login")
	if err != nil || len(logins) != 1 {
		t.Fatal("login was not recorded", err)
	}
	siteToken, err := v.CreateSiteSession(ctx, userID, uid, nil, nil, 3600)
	if err != nil {
		t.Fatal(err)
	}
	admin, csrf, err := v.AdminLogin(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "panel_session=" + admin, "X-CSRF-Token": csrf}
	w, _ := secureRequest(t, s, "PUT", "/panel/api/bans", fmt.Sprintf(`{"mirea_id":%q,"blocked":true,"reason":"test"}`, uid), nil)
	if w.Code != 401 {
		t.Fatal("anonymous ban accepted")
	}
	w, _ = secureRequest(t, s, "PUT", "/panel/api/bans", fmt.Sprintf(`{"mirea_id":%q,"blocked":true}`, uid), map[string]string{"Cookie": "panel_session=" + admin})
	if w.Code != 403 {
		t.Fatal("ban accepted without csrf", w.Code)
	}
	w, _ = secureRequest(t, s, "PUT", "/panel/api/bans", fmt.Sprintf(`{"mirea_id":%q,"blocked":true}`, uid), headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"/api/accounts", "/site/state", "/site/passes", "/site/passes/" + id} {
		w, _ = secureRequest(t, s, "GET", path, "", map[string]string{"Authorization": "Bearer valid"})
		if w.Code != 403 {
			t.Fatal("blocked bearer accepted", path, w.Code)
		}
	}
	w, _ = secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"123456"}`, map[string]string{"Authorization": "Bearer valid"})
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	if _, err := s.refresh(ctx, id); !errors.Is(err, postgres.ErrBlocked) {
		t.Fatal("blocked account renewed", err)
	}
	if upstream.calls != 0 {
		t.Fatal("blocked actor reached Pulse")
	}
	if active, err := identityStore.ActiveSession(ctx, session["id"].(string)); err != nil || active != nil {
		t.Fatal("AAID session survived", err)
	}
	if active, err := v.Session(ctx, "site", siteToken); err != nil || active != nil {
		t.Fatal("site cookie survived", err)
	}
	if _, err := identityStore.BindMirea(ctx, uid, nil, nil); !errors.Is(err, postgres.ErrBlocked) {
		t.Fatal("blocked person signed in", err)
	}
	if _, err := v.Upsert(ctx, postgres.Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "session=late"}}); !errors.Is(err, postgres.ErrBlocked) {
		t.Fatal("stale login resurrected account", err)
	}
	w, _ = secureRequest(t, s, "PUT", "/panel/api/bans", fmt.Sprintf(`{"mirea_id":%q,"blocked":false}`, uid), headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if active, _ := identityStore.ActiveSession(ctx, session["id"].(string)); active != nil {
		t.Fatal("unban resurrected revoked token")
	}
	if _, err := identityStore.BindMirea(ctx, uid, nil, nil); err != nil {
		t.Fatal("unban ineffective", err)
	}
	unknown := uid + "-future"
	defer v.Pool.Exec(ctx, "DELETE FROM aa.mirea_bans WHERE pulse_uid=$1", unknown)
	if _, err := v.Pool.Exec(ctx, "SELECT aa.set_mirea_ban($1,true,'test')", unknown); err != nil {
		t.Fatal(err)
	}
	if _, err := identityStore.BindMirea(ctx, unknown, nil, nil); !errors.Is(err, postgres.ErrBlocked) {
		t.Fatal("pre-registration ban bypassed", err)
	}
}

func TestUsagePresenceTransitionsArePrivateAndDeduplicated(t *testing.T) {
	s, v := setup(t, nil)
	ctx := context.Background()
	uid := fmt.Sprintf("presence-%d", time.Now().UnixNano())
	row := account(t, v, uid)
	id := row["id"].(string)
	defer v.Pool.Exec(ctx, "DELETE FROM aa.presence_observations WHERE pulse_uid=$1", uid)
	events := []map[string]any{{"time": uint64(100), "direction": "in"}, {"time": uint64(200), "direction": "out"}}
	for i := 0; i < 2; i++ {
		if err := v.ObservePresence(ctx, uid, id, events); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := v.UsageList(ctx, 0, uid, "presence_transition")
	if err != nil || len(rows) != 2 {
		t.Fatal("duplicate provider observations", len(rows), err)
	}
	details := rows[0]["details"].(map[string]any)
	if fmt.Sprint(details["previous_state"]) != "1" || fmt.Sprint(details["new_state"]) != "2" || rows[0]["actor_mirea"] != nil {
		t.Fatal("invented actor or incorrect transition", rows)
	}
	w, _ := secureRequest(t, s, "GET", "/panel/api/usage", "", nil)
	if w.Code != 401 {
		t.Fatal("public statistics")
	}
	token, _, err := v.AdminLogin(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	w, data := secureRequest(t, s, "GET", "/panel/api/usage?mirea_id="+uid, "", map[string]string{"Cookie": "panel_session=" + token})
	if w.Code != 200 || len(data["events"].([]any)) != 2 {
		t.Fatal(w.Body.String())
	}
}
