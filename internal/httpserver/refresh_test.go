package httpserver

import (
	"aa-server/internal/browser"
	"aa-server/internal/postgres"
	"context"
	"fmt"
	"testing"
	"time"
)

type renewingBrowser struct {
	calls int
	uid   string
}

func (b *renewingBrowser) Login(context.Context, func(browser.Update)) (*browser.Result, error) {
	return nil, nil
}
func (b *renewingBrowser) Renew(context.Context, map[string]any) (*browser.Result, error) {
	b.calls++
	return &browser.Result{UID: b.uid, Credentials: map[string]any{"cookie": "session=renewed", "storage_state": map[string]any{"cookies": []any{}}, "renewed_at": float64(time.Now().Unix())}}, nil
}
func TestRefreshRenewsAndNeverResurrectsEndedSession(t *testing.T) {
	uid := fmt.Sprintf("renew-%d", time.Now().UnixNano())
	b := &renewingBrowser{uid: uid}
	s, v := setup(t, func(c *Config) { c.Browser = b; c.Pulse = &fakePulse{identityUID: uid} })
	row, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "session=old", "storage_state": map[string]any{"cookies": []any{}}, "renewed_at": float64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	t.Cleanup(func() { _ = v.DeleteAccount(context.Background(), id) })
	fresh, err := s.refresh(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(fresh)
	if err != nil || secret["cookie"] != "session=renewed" || b.calls != 1 {
		t.Fatal("renewal failed", err)
	}
	statistics := postgres.RenewalView(secret)["statistics"].(postgres.RenewalStatistics)
	if statistics.Attempts != 1 || statistics.Succeeded != 1 || statistics.ExpiryUnknown != 1 {
		t.Fatal("refresh did not record automatic outcome", statistics)
	}
	if _, err = v.EndSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.refresh(context.Background(), id)
	if err != nil || fresh["state"] != "expired" || b.calls != 1 {
		t.Fatal("ended session renewed", err)
	}
}

func TestRenewalDeadlineDoesNotOpenBrowserForHealthySessions(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name, state      string
		expires, renewed any
		want             bool
	}{
		{"long lived recently renewed cookie", "valid", float64(now.Add(8 * time.Hour).Unix()), float64(now.Add(-time.Minute).Unix()), false},
		{"SSO heartbeat for long lived cookie", "valid", float64(now.Add(8 * time.Hour).Unix()), float64(now.Add(-time.Hour).Unix()), true},
		{"fresh ten minute cookie", "valid", float64(now.Add(10 * time.Minute).Unix()), float64(now.Unix()), false},
		{"ten minute cookie near expiry", "valid", float64(now.Add(time.Minute).Unix()), float64(now.Add(-9 * time.Minute).Unix()), true},
		{"unknown deadline heartbeat", "valid", nil, float64(now.Add(-time.Hour).Unix()), true},
		{"recent unknown deadline", "valid", nil, float64(now.Unix()), false},
		{"expired cookie", "expired", nil, nil, true},
		{"unavailable upstream", "unavailable", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renewalDue(tc.state, now, tc.expires, tc.renewed, 15*time.Minute, 15*time.Minute); got != tc.want {
				t.Fatalf("renew=%v want %v", got, tc.want)
			}
		})
	}
}

type expiredSSOBrowser struct{ calls int }

func (b *expiredSSOBrowser) Login(context.Context, func(browser.Update)) (*browser.Result, error) {
	return nil, nil
}
func (b *expiredSSOBrowser) Renew(context.Context, map[string]any) (*browser.Result, error) {
	b.calls++
	return nil, browser.ErrReauthenticationRequired
}

func TestExpiredSSOStopsRetryLoopAndFreshLoginResetsIt(t *testing.T) {
	uid := fmt.Sprintf("expired-sso-%d", time.Now().UnixNano())
	b := &expiredSSOBrowser{}
	s, v := setup(t, func(c *Config) { c.Browser = b; c.Pulse = &fakePulse{identityUID: uid} })
	row, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "session=old", "storage_state": map[string]any{"cookies": []any{}}, "renewed_at": float64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	t.Cleanup(func() { _ = v.DeleteAccount(context.Background(), id) })
	first, err := s.refresh(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(first)
	if err != nil || secret["renewal_status"] != "requires_login" || b.calls != 1 {
		t.Fatal("failed SSO result missing", err, secret["renewal_status"])
	}
	s.refreshMu.Lock()
	clear(s.renewed)
	s.refreshMu.Unlock() // simulate process restart/cooldown expiration
	if _, err := s.refresh(context.Background(), id); err != nil || b.calls != 1 {
		t.Fatal("expired SSO retried", err, b.calls)
	}
	newer, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "session=new", "renewed_at": float64(time.Now().Unix())}})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.NoteRenewal(context.Background(), id, row["session_version"].(int64), "requires_login"); err != nil {
		t.Fatal(err)
	}
	newer, err = v.Account(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	secret, err = v.Secret(newer)
	if err != nil || secret["renewal_status"] != nil {
		t.Fatal("fresh login paused by stale renewal", err)
	}
}
