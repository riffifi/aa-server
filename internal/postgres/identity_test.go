package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func identityUser(t *testing.T, s *Identity, pulse string) map[string]any {
	t.Helper()
	user, err := s.BindMirea(context.Background(), pulse, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := user["id"].(string)
	t.Cleanup(func() {
		ctx := context.Background()
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(ctx)
		for _, sql := range []string{"DELETE FROM aaid.web_tokens WHERE session_id IN(SELECT id FROM aaid.sessions WHERE user_id=$1)", "DELETE FROM aaid.refresh_tokens WHERE session_id IN(SELECT id FROM aaid.sessions WHERE user_id=$1)", "DELETE FROM aaid.sessions WHERE user_id=$1", "DELETE FROM aaid.mirea_links WHERE user_id=$1", "DELETE FROM aaid.users WHERE id=$1", "DELETE FROM aaid.audit WHERE user_id=$1"} {
			if _, err = tx.Exec(ctx, sql, id); err != nil {
				t.Error(err)
				return
			}
		}
		if err = tx.Commit(ctx); err != nil {
			t.Error(err)
		}
	})
	return user
}

func TestIdentitySessionKindsEvictionAndRefreshReuse(t *testing.T) {
	s := NewIdentity(testStore(t))
	ctx := context.Background()
	user := identityUser(t, s, fmt.Sprintf("identity-%d", time.Now().UnixNano()))
	id := user["id"].(string)
	first, _, err := s.CreateSession(ctx, id, "first", 3600, 2, "app")
	if err != nil {
		t.Fatal(err)
	}
	second, refresh, err := s.CreateSession(ctx, id, "second", 3600, 2, "app")
	if err != nil {
		t.Fatal(err)
	}
	_, web, err := s.CreateSession(ctx, id, "browser", 3600, 1, "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateSession(ctx, id, "third", 3600, 2, "app"); err != nil {
		t.Fatal(err)
	}
	session, err := s.ActiveSession(ctx, first["id"].(string))
	if err != nil || session != nil {
		t.Fatal("oldest app session was not evicted")
	}
	if session, err = s.WebSession(ctx, web); err != nil || session == nil {
		t.Fatal("app eviction affected web session")
	}
	renewed, fresh, err := s.Rotate(ctx, refresh, 3600)
	if err != nil || renewed == nil || fresh == "" {
		t.Fatal("refresh rotation failed")
	}
	reused, _, err := s.Rotate(ctx, refresh, 3600)
	if err != nil || reused != nil {
		t.Fatal("refresh reuse was accepted")
	}
	if session, err = s.ActiveSession(ctx, second["id"].(string)); err != nil || session != nil {
		t.Fatal("refresh reuse did not revoke session")
	}
	if session, _, err = s.Rotate(ctx, fresh, 3600); err != nil || session != nil {
		t.Fatal("fresh refresh survived session revocation")
	}
	if changed, err := s.SetStatus(ctx, id, "blocked"); err != nil || !changed {
		t.Fatal("blocking failed")
	}
	if session, err = s.WebSession(ctx, web); err != nil || session != nil {
		t.Fatal("web session survived blocking")
	}
	if _, err = s.BindMirea(ctx, user["pulse_uid"].(string), nil, nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("blocked user signed in again")
	}
}

func TestConcurrentIdentityBindingHasOneUserAndUsernameUniqueness(t *testing.T) {
	s := NewIdentity(testStore(t))
	ctx := context.Background()
	pulse := fmt.Sprintf("bind-%d", time.Now().UnixNano())
	user := identityUser(t, s, pulse)
	id := user["id"].(string)
	results := make(chan error, 8)
	for range 8 {
		go func() {
			user, err := s.BindMirea(ctx, pulse, nil, nil)
			if err == nil && user["id"] != id {
				err = errors.New("duplicate identity")
			}
			results <- err
		}()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	second := identityUser(t, s, pulse+"-other")
	username := "synthetic_" + id
	if _, err := s.SetUsername(ctx, id, &username); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUsername(ctx, second["id"].(string), &username); !errors.Is(err, ErrUsernameTaken) {
		t.Fatal("duplicate username accepted")
	}
}
