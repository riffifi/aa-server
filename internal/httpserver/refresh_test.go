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
	if _, err = v.EndSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.refresh(context.Background(), id)
	if err != nil || fresh["state"] != "expired" || b.calls != 1 {
		t.Fatal("ended session renewed", err)
	}
}
