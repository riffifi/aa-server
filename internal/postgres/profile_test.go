package postgres

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestCookieRotationAndNameFillNeverResurrectOrReplaceManualName(t *testing.T) {
	v := testVault(t)
	ctx := context.Background()
	uid := fmt.Sprintf("native-cookie-%d", time.Now().UnixNano())
	row, err := v.Upsert(ctx, Upsert{PulseID: uid, Label: "Без имени", Credentials: map[string]any{"cookie": ".AspNetCore.Cookies=old", "pass_number": "123456"}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	defer v.DeleteAccount(ctx, id)
	version := row["session_version"].(int64)
	name := "Иванов Иван"
	named, err := v.FillProfileName(ctx, id, name, version)
	if err != nil || named["label"] != name {
		t.Fatal(named, err)
	}
	if _, err := v.Rename(ctx, id, "Выбранное имя", nil); err != nil {
		t.Fatal(err)
	}
	if named, err := v.FillProfileName(ctx, id, "Не заменять", version); err != nil || named != nil {
		t.Fatal(named, err)
	}
	h := http.Header{"Set-Cookie": {".AspNetCore.Cookies=fresh; Path=/; Max-Age=86400; Secure; HttpOnly"}}
	fresh, err := v.SavePulseCookies(ctx, id, ".AspNetCore.Cookies=old", version, h)
	if err != nil || fresh == nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(fresh)
	if err != nil || secret["cookie"] != ".AspNetCore.Cookies=fresh" || secret["pass_number"] != "123456" {
		t.Fatal("rotation lost credentials", err)
	}
	stats := renewalStatistics(secret)
	if stats.CookieRefreshes != 1 || stats.LastCookieRefresh == 0 || stats.Since == 0 {
		t.Fatal(stats)
	}
	if _, err := v.SavePulseCookies(ctx, id, ".AspNetCore.Cookies=old", version, h); err != nil {
		t.Fatal(err)
	}
	if _, err := v.EndSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if stale, err := v.SavePulseCookies(ctx, id, ".AspNetCore.Cookies=fresh", fresh["session_version"].(int64), h); err != nil || stale != nil {
		t.Fatal("logout resurrected", stale, err)
	}
	current, err := v.Account(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	secret, err = v.Secret(current)
	if err != nil || secret["cookie"] != nil || current["state"] != "expired" {
		t.Fatal("ended account changed", err)
	}
}

func TestCurrentPulseRotationClearsRenewalPause(t *testing.T) {
	v := testVault(t)
	ctx := context.Background()
	uid := fmt.Sprintf("pulse-cookie-%d", time.Now().UnixNano())
	row, err := v.Upsert(ctx, Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "Pulse.Auth.Cookie=old"}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	defer v.DeleteAccount(ctx, id)
	version := row["session_version"].(int64)
	if err := v.NoteRenewal(ctx, id, version, "requires_login"); err != nil {
		t.Fatal(err)
	}
	fresh, err := v.SavePulseCookies(ctx, id, "Pulse.Auth.Cookie=old", version, http.Header{"Set-Cookie": {"Pulse.Auth.Cookie=fresh; Path=/; Max-Age=1209600; Secure; HttpOnly"}})
	if err != nil || fresh == nil {
		t.Fatal("rotation not persisted", err)
	}
	secret, err := v.Secret(fresh)
	if err != nil {
		t.Fatal(err)
	}
	stats := renewalStatistics(secret)
	if secret["renewal_status"] != nil || stats.CookieRefreshes != 1 || fresh["expires_at"] == nil {
		t.Fatal("current ticket did not clear stale pause or update expiry")
	}
	if _, err := v.EndSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if stale, err := v.SavePulseCookies(ctx, id, "Pulse.Auth.Cookie=fresh", fresh["session_version"].(int64), http.Header{"Set-Cookie": {"Pulse.Auth.Cookie=stale; Path=/; Max-Age=1209600"}}); err != nil || stale != nil {
		t.Fatal("logout resurrected", err)
	}
}
