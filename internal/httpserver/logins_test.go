package httpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/postgres"
)

type loginRunner func(context.Context, func(browser.Update)) (*browser.Result, error)

func (f loginRunner) Login(ctx context.Context, update func(browser.Update)) (*browser.Result, error) {
	return f(ctx, update)
}
func waitLogin(t *testing.T, s *Server, id, status string) browser.JobState {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, err := s.config.Logins.AdminState(id)
		if err != nil {
			t.Fatal(err)
		}
		if state.Status == status {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("login did not finish")
	return browser.JobState{}
}
func TestPanelEnrollmentPersistsCredentialsAndProtectsCapability(t *testing.T) {
	uid := fmt.Sprintf("native-login-%d", time.Now().UnixNano())
	release := make(chan struct{})
	runner := loginRunner(func(ctx context.Context, update func(browser.Update)) (*browser.Result, error) {
		update(browser.Update{Status: "login", QR: "https://sso.mirea.ru/realms/mirea/qr-code-auth/scan?token=synthetic"})
		select {
		case <-release:
			return &browser.Result{UID: uid, Credentials: map[string]any{"cookie": "session=fresh"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	s, v := setup(t, func(c *Config) { c.Browser = runner })
	token, csrf, err := v.AdminLogin(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "panel_session=" + token, "X-CSRF-Token": csrf}
	w, _ := request(t, s, "POST", "/panel/api/logins", `{}`, nil)
	if w.Code != 401 {
		t.Fatal("anonymous login started")
	}
	w, started := request(t, s, "POST", "/panel/api/logins", `{}`, headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := started["id"].(string)
	path := started["enrollment_path"].(string)
	if started["expires_in"] != float64(600) {
		t.Fatal(w.Body.String())
	}
	state := waitLogin(t, s, id, "login")
	if state.QR == nil {
		t.Fatal("QR missing")
	}
	w, _ = request(t, s, "GET", "/enroll/"+id+"/wrong/state", "", nil)
	if w.Code != 404 {
		t.Fatal("wrong enrollment capability accepted")
	}
	w, result := request(t, s, "GET", path+"/state", "", nil)
	if w.Code != 200 || result["label"] != nil || result["account_id"] != nil {
		t.Fatal(w.Body.String())
	}
	w, _ = request(t, s, "POST", path+"/claim", "", nil)
	if w.Code != 409 {
		t.Fatal("pending enrollment claimed")
	}
	qr := rawRequest(s, "GET", path+"/qr", nil)
	if qr.Code != 200 || qr.Header().Get("Content-Type") != "image/png" {
		t.Fatal("QR rendering failed")
	}
	close(release)
	state = waitLogin(t, s, id, "complete")
	if state.AccountID == "" {
		t.Fatal("account not stored")
	}
	t.Cleanup(func() { _ = v.DeleteAccount(context.Background(), state.AccountID) })
	w, result = request(t, s, "POST", path+"/claim", "", nil)
	if w.Code != 200 || result["account_id"] != state.AccountID {
		t.Fatal(w.Body.String())
	}
	row, err := v.Account(context.Background(), state.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(row)
	if err != nil || secret["cookie"] != "session=fresh" {
		t.Fatal("captured credentials lost", err)
	}
	if strings.Contains(w.Body.String(), "fresh") {
		t.Fatal("credentials leaked in enrollment response")
	}
}
func TestBrowserCompletionCannotOverwriteNewerSession(t *testing.T) {
	s, v := setup(t, nil)
	row := account(t, v, fmt.Sprintf("login-conflict-%d", time.Now().UnixNano()))
	id := row["id"].(string)
	version := row["session_version"].(int64)
	if _, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: row["pulse_id"].(string), AccountID: id, Credentials: map[string]any{"cookie": "session=newer"}}); err != nil {
		t.Fatal(err)
	}
	runner := accountLogin{runner: loginRunner(func(context.Context, func(browser.Update)) (*browser.Result, error) {
		return &browser.Result{UID: row["pulse_id"].(string), Credentials: map[string]any{"cookie": "session=obsolete"}}, nil
	}), vault: v, accountID: id, sessionExpected: &version}
	state, token, err := s.config.Logins.StartWith(runner, nil, "failure")
	if err != nil {
		t.Fatal(err)
	}
	state = waitLogin(t, s, state.ID, "failed")
	if state.Error == nil || !strings.Contains(*state.Error, "newer version") {
		t.Fatal(state)
	}
	if _, err = s.config.Logins.Claim(state.ID, token); err == nil {
		t.Fatal("conflicting session claimed")
	}
	fresh, err := v.Account(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(fresh)
	if err != nil || secret["cookie"] != "session=newer" {
		t.Fatal("newer session overwritten")
	}
}
