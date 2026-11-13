package pulse

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProfileNameUsesVerifiedUserAndPreservesPrivacy(t *testing.T) {
	user := append(Field(1, []byte("owner")), Field(2, []byte("Иван"))...)
	user = append(user, Field(3, []byte("Иванов"))...)
	user = append(user, Field(4, Field(1, []byte("Иванович")))...)
	body, h := passReply(Field(1, Field(1, user)))
	if name := ProfileName(body, h, "owner"); name != "Иванов Иван Иванович" {
		t.Fatal(name)
	}
	if name := ProfileName(body, h, "other"); name != "" {
		t.Fatal("used another person's name", name)
	}
	for _, bad := range [][]byte{
		append(user, Field(2, []byte("Duplicate"))...),
		append(user, Field(3, []byte("Bad\nname"))...),
		append(Field(1, []byte("owner")), Field(2, []byte{255})...),
	} {
		b, h := passReply(Field(1, Field(1, bad)))
		if name := ProfileName(b, h, "owner"); name != "" {
			t.Fatal("malformed name accepted", name)
		}
	}
	b, h := passReply(Field(2, nil))
	if name := ProfileName(b, h, "owner"); name != "" {
		t.Fatal(name)
	}
}

func TestMergeSessionCookiesHandlesChunksExpiryAndStorage(t *testing.T) {
	now := time.Unix(1794400000, 0)
	state := map[string]any{"cookies": []any{
		map[string]any{"name": "SSO", "value": "private-sso", "domain": "sso.mirea.ru", "path": "/", "expires": -1},
		map[string]any{"name": ".AspNetCore.CookiesC3", "value": "obsolete", "domain": "pulse.mirea.ru", "path": "/"},
	}, "origins": []any{map[string]any{"origin": Host, "localStorage": []any{}}}}
	secret := map[string]any{"cookie": ".AspNetCore.Cookies=chunks-3; .AspNetCore.CookiesC1=old1; .AspNetCore.CookiesC2=old2; .AspNetCore.CookiesC3=old3; theme=dark", "expires_at": float64(now.Unix() + 60), "storage_state": state}
	before, _ := json.Marshal(secret)
	h := http.Header{"Set-Cookie": {
		".AspNetCore.Cookies=chunks-2; Path=/; Secure; HttpOnly; Max-Age=86400",
		".AspNetCore.CookiesC1=new1; Path=/; Secure; HttpOnly; Max-Age=86400",
		".AspNetCore.CookiesC2=new2; Path=/; Secure; HttpOnly; Max-Age=86400",
		"evil=x; Domain=example.org; Path=/",
		"irrelevant=x; Path=/private",
	}}
	patch, auth, err := MergeSessionCookies(secret, h, now)
	if err != nil || !auth || strings.Contains(patch["cookie"].(string), "old") || strings.Contains(patch["cookie"].(string), "evil") || strings.Contains(patch["cookie"].(string), "irrelevant") {
		t.Fatal(patch, auth, err)
	}
	if patch["expires_at"] != float64(now.Unix()+86400) {
		t.Fatal(patch["expires_at"])
	}
	after, _ := json.Marshal(secret)
	if string(before) != string(after) {
		t.Fatal("mutated input state")
	}
	cookies := patch["storage_state"].(map[string]any)["cookies"].([]any)
	foundSSO := false
	for _, raw := range cookies {
		c := raw.(map[string]any)
		if c["name"] == "SSO" && c["value"] == "private-sso" {
			foundSSO = true
		}
		if c["name"] == ".AspNetCore.CookiesC3" {
			t.Fatal("stale chunk retained")
		}
	}
	if !foundSSO {
		t.Fatal("SSO state lost")
	}
	precedence, _, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {".AspNetCore.Cookies=live; Path=/; Max-Age=3600; Expires=Thu, 01 Jan 1970 00:00:00 GMT"}}, now)
	if err != nil || !strings.Contains(precedence["cookie"].(string), "Cookies=live") {
		t.Fatal("Max-Age did not override Expires", err)
	}
	for k, v := range patch {
		secret[k] = v
	}
	plain, _, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {".AspNetCore.Cookies=single; Path=/; HttpOnly; Secure"}}, now)
	if err != nil || strings.Contains(plain["cookie"].(string), "CookiesC") || plain["expires_at"] != nil {
		t.Fatal(plain, err)
	}
	if _, _, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {".AspNetCore.Cookies=chunks-4; Path=/"}}, now); err == nil {
		t.Fatal("incomplete cookie chunks accepted")
	}
	deleted, _, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {".AspNetCore.Cookies=; Path=/; Max-Age=0"}}, now)
	if err != nil || strings.Contains(deleted["cookie"].(string), "Cookies") {
		t.Fatal(deleted, err)
	}
	ignored, _, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {"session=other; Domain=sso.mirea.ru; Path=/"}}, now)
	if err != nil || ignored != nil {
		t.Fatal("foreign cookie accepted", err)
	}
}

func TestCurrentPulseCookieRotationAndChunks(t *testing.T) {
	now := time.Unix(1794604268, 0)
	secret := map[string]any{"cookie": "Pulse.Auth.Cookie=old; theme=dark", "expires_at": float64(now.Unix() + 3600)}
	patch, auth, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {
		"Pulse.Auth.Cookie=fresh; Path=/; Secure; HttpOnly; Max-Age=1209600",
		"__ddg9_=challenge; Domain=.mirea.ru; Path=/; Max-Age=1200",
	}}, now)
	if err != nil || !auth || patch["expires_at"] != float64(now.Add(14*24*time.Hour).Unix()) || !HasSessionCookie(patch["cookie"].(string)) {
		t.Fatal("current ticket rotation/expiry lost", err)
	}
	chunked := map[string]any{"cookie": "Pulse.Auth.Cookie=chunks-2; Pulse.Auth.CookieC1=old1; Pulse.Auth.CookieC2=old2"}
	single, auth, err := MergeSessionCookies(chunked, http.Header{"Set-Cookie": {"Pulse.Auth.Cookie=single; Path=/; Max-Age=1209600"}}, now)
	if err != nil || !auth || strings.Contains(single["cookie"].(string), "CookieC") {
		t.Fatal("obsolete chunks survived", err)
	}
	if _, _, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {"Pulse.Auth.Cookie=chunks-2; Path=/"}}, now); err == nil {
		t.Fatal("incomplete current chunks accepted")
	}
	deleted, auth, err := MergeSessionCookies(secret, http.Header{"Set-Cookie": {"Pulse.Auth.Cookie=; Path=/; Max-Age=0"}}, now)
	if err != nil || !auth || HasSessionCookie(deleted["cookie"].(string)) {
		t.Fatal("deleted current ticket retained", err)
	}
	for _, name := range []string{"__ddg9_", "Pulse.Auth.CookieEvil", "Pulse.Auth.CookieC0", "xPulse.Auth.Cookie"} {
		if IsSessionCookie(name) {
			t.Fatal("non-auth cookie accepted", name)
		}
	}
}
