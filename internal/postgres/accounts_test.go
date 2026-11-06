package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

func testVault(t *testing.T) *Vault {
	t.Helper()
	store := testStore(t)
	v, err := NewVault(store, base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func ptr[T any](value T) *T { return &value }

func TestAccountLifecyclePreservesCredentialsAndVersionContracts(t *testing.T) {
	v := testVault(t)
	ctx := context.Background()
	uid := fmt.Sprintf("lifecycle-%d", time.Now().UnixNano())
	row, err := v.Upsert(ctx, Upsert{PulseID: uid, Label: "Owner", GroupID: ptr(int64(5330)), Credentials: map[string]any{"cookie": "session=old", "storage_state": map[string]any{"cookies": []any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	t.Cleanup(func() { _ = v.DeleteAccount(ctx, id) })
	if row["pass_allowed"] != int64(0) || row["version"] != int64(1) || row["session_version"] != int64(1) {
		t.Fatalf("new-account contract changed: %#v", Public(row))
	}
	if _, err = v.SavePass(ctx, id, "9223372036854775807", nil); err != nil {
		t.Fatal(err)
	}
	before, err := v.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row, err = v.Upsert(ctx, Upsert{PulseID: uid, AccountID: id, Credentials: map[string]any{"cookie": "session=new"}, SessionExpected: ptr(int64(1))})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(row)
	if err != nil {
		t.Fatal(err)
	}
	if secret["pass_number"] != "9223372036854775807" || secret["cookie"] != "session=new" || row["label"] != "Owner" || row["group_id"] != int64(5330) {
		t.Fatal("renewal lost account data")
	}
	if row["session_version"] != int64(2) || row["pass_version"] != int64(1) {
		t.Fatal("wrong renewal versions")
	}
	events, _, err := v.Events(ctx, before, nil)
	if err != nil || len(events) != 1 || events[0]["kind"] != "session" {
		t.Fatalf("wrong renewal event: %v", err)
	}
	version := row["version"].(int64)
	renamed, err := v.Rename(ctx, id, "  Новое имя  ", &version)
	if err != nil {
		t.Fatal(err)
	}
	if renamed["label"] != "Новое имя" {
		t.Fatal("name normalization changed")
	}
	_, err = v.Rename(ctx, id, "stale", &version)
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatal("stale update accepted")
	}
	before, _ = v.Cursor(ctx)
	if _, err = v.Rename(ctx, id, "Новое имя", nil); err != nil {
		t.Fatal(err)
	}
	after, _ := v.Cursor(ctx)
	if before != after {
		t.Fatal("no-op rename emitted an event")
	}
	if _, err = v.SetPassAllowed(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}
	ended, err := v.EndSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ended["state"] != "expired" || ended["pass_allowed"] != true || ended["session_version"] != int64(3) {
		t.Fatal("end-session changed metadata")
	}
	row, _ = v.Account(ctx, id)
	secret, err = v.Secret(row)
	if err != nil {
		t.Fatal(err)
	}
	if secret["cookie"] != nil || secret["storage_state"] != nil || secret["pass_number"] != "9223372036854775807" {
		t.Fatal("end-session credential cleanup failed")
	}
}

func TestOptimisticWritersHaveExactlyOneWinner(t *testing.T) {
	v := testVault(t)
	ctx := context.Background()
	row, err := v.Upsert(ctx, Upsert{PulseID: fmt.Sprintf("race-%d", time.Now().UnixNano()), Credentials: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	t.Cleanup(func() { _ = v.DeleteAccount(ctx, id) })
	version := row["version"].(int64)
	done := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) { _, err := v.Rename(ctx, id, name, &version); done <- err }(name)
	}
	won, conflicted := 0, 0
	for range 2 {
		err := <-done
		if err == nil {
			won++
		} else {
			var conflict *Conflict
			if errors.As(err, &conflict) {
				conflicted++
			} else {
				t.Fatal(err)
			}
		}
	}
	if won != 1 || conflicted != 1 {
		t.Fatalf("optimistic update results: %d winners, %d conflicts", won, conflicted)
	}
}

func TestGroupEventsCarryPreviousGroupAndInvalidPassDoesNotWrite(t *testing.T) {
	v := testVault(t)
	ctx := context.Background()
	row, err := v.Upsert(ctx, Upsert{PulseID: fmt.Sprintf("group-%d", time.Now().UnixNano()), GroupID: ptr(int64(1)), Credentials: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	t.Cleanup(func() { _ = v.DeleteAccount(ctx, id) })
	before, _ := v.Cursor(ctx)
	for _, number := range []string{"0", "01", "-1", "9223372036854775808", "1.0", "１２"} {
		if _, err = v.SavePass(ctx, id, number, nil); err == nil {
			t.Fatalf("invalid pass accepted: %q", number)
		}
	}
	after, _ := v.Cursor(ctx)
	if before != after {
		t.Fatal("invalid pass changed events")
	}
	if _, err = v.UpdateGroup(ctx, id, ptr(int64(2)), ptr(" Group 2 "), row["version"].(int64)); err != nil {
		t.Fatal(err)
	}
	oldGroup := int64(1)
	events, _, err := v.Events(ctx, before, &oldGroup)
	if err != nil || len(events) != 1 || events[0]["previous_group_id"] != int64(1) || events[0]["group_id"] != int64(2) {
		t.Fatal("previous group cannot see reassignment")
	}
	groups, err := v.Groups(ctx)
	if err != nil || len(groups) == 0 {
		t.Fatalf("group listing failed: %v", err)
	}
}
