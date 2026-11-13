package httpserver

import (
	"aa-server/internal/browser"
	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
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

// ACS history is provider state, not evidence of an action through AA.
func TestUsageExcludesExternalActions(t *testing.T) {
	upstream := &fakePulse{}
	s, v := setup(t, func(c *Config) { c.Pulse = upstream })
	ctx := context.Background()
	uid := fmt.Sprintf("usage-scope-%d", time.Now().UnixNano())
	row := account(t, v, uid)
	id := row["id"].(string)
	defer v.Pool.Exec(ctx, "DELETE FROM aa.usage_events WHERE target_mirea=$1", uid)
	event := pulse.Field(2, pulse.VField(1, uint64(time.Now().Unix())))
	event = append(event, pulse.Field(3, pulse.Field(2, []byte("Campus")))...)
	event = append(event, pulse.VField(5, 1)...)
	upstream.reply = grpcReply(pulse.Field(1, event))
	if events, _ := pulse.ACSEvents(upstream.reply.Body, upstream.reply.Headers); len(events) != 1 {
		t.Fatal("expected a provider passage in test response")
	}
	if _, err := s.pulseRPC(ctx, row, "acs-timeline", nil, ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := v.Pool.QueryRow(ctx, "SELECT count(*) FROM aa.usage_events WHERE target_mirea=$1", uid).Scan(&count); err != nil || count != 0 {
		t.Fatal("ACS read imported external activity", count, err)
	}
	// Simulate records left by 2.5.0, and a future unknown external source.
	for _, kind := range []string{"presence_transition", "external_attendance", "attendance"} {
		if err := v.Usage(ctx, kind, uid, uid, id, map[string]any{"result": "approved"}); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := secureRequest(t, s, "GET", "/panel/api/usage", "", nil)
	if w.Code != 401 {
		t.Fatal("public statistics")
	}
	token, _, err := v.AdminLogin(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "panel_session=" + token}
	for _, filter := range []string{"", "&kind=attendance", "&kind=presence_transition", "&kind=external_attendance"} {
		w, data := secureRequest(t, s, "GET", "/panel/api/usage?mirea_id="+uid+filter, "", headers)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		events := data["events"].([]any)
		summary := data["summary_24h"].([]any)
		expected := 1
		if filter == "&kind=presence_transition" || filter == "&kind=external_attendance" {
			expected = 0
		}
		if len(events) != expected || len(summary) != expected {
			t.Fatalf("external activity visible: %s %s", filter, w.Body.String())
		}
		if expected == 1 && (events[0].(map[string]any)["kind"] != "attendance" || summary[0].(map[string]any)["count"] != float64(1)) {
			t.Fatal("AA action or totals lost", w.Body.String())
		}
	}
}
