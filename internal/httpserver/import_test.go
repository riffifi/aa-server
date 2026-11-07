package httpserver

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"testing"
	"time"

	"aa-server/internal/cryptocompat"
)

func TestInternalLinkRequiresDedicatedAudienceAndMatchingPulseIdentity(t *testing.T) {
	uid := fmt.Sprintf("link-%d", time.Now().UnixNano())
	seed := make([]byte, 32)
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	verifier := cryptocompat.NewVerifier("", map[string]ed25519.PublicKey{cryptocompat.KeyID(public): public})
	defer verifier.Close()
	upstream := &fakePulse{identityUID: uid}
	s, v := setup(t, func(c *Config) { c.Verifier = verifier; c.Pulse = upstream })
	row := account(t, v, uid)
	body := `{"cookie":"session=fresh","storage_state":{"cookies":[]},"group_id":999999,"group_name":"Forged group"}`
	claims := map[string]any{"iss": "aa-id", "aud": "aa", "sub": "synthetic-aaid", "mirea": uid, "grp": int64(5330), "group_name": "Verified group", "exp": time.Now().Unix() + 600}
	token, err := cryptocompat.Sign(seed, claims)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := request(t, s, "POST", "/internal/aaid/link", body, map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusUnauthorized {
		t.Fatal("app audience was accepted as an internal link")
	}
	claims["aud"] = "aa-link"
	token, err = cryptocompat.Sign(seed, claims)
	if err != nil {
		t.Fatal(err)
	}
	w, result := request(t, s, "POST", "/internal/aaid/link", body, map[string]string{"Authorization": "Bearer " + token})
	if w.Code != 200 || result["account_id"] != row["id"] {
		t.Fatal(w.Body.String())
	}
	updated, err := v.Account(context.Background(), row["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(updated)
	if err != nil || secret["cookie"] != "session=fresh" || updated["group_id"] != int64(5330) || updated["group_name"] != "Verified group" {
		t.Fatal("signed handoff did not persist fresh credentials")
	}
	claims["mirea"] = "different-person"
	token, err = cryptocompat.Sign(seed, claims)
	if err != nil {
		t.Fatal(err)
	}
	w, _ = request(t, s, "POST", "/internal/aaid/link", body, map[string]string{"Authorization": "Bearer " + token})
	if w.Code != 422 {
		t.Fatal("another person's cookie was linked")
	}
}
