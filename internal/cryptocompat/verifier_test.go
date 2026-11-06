package cryptocompat

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifierCoalescesUnknownKeysAndDoesNotFetchOnEveryRequest(t *testing.T) {
	seed := make([]byte, 32)
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	kid := KeyID(public)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kid": kid, "kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(public)}}})
	}))
	defer server.Close()
	v := NewVerifier(server.URL+"/.well-known/jwks.json", nil)
	defer v.Close()
	token, err := Sign(seed, map[string]any{"iss": "aa-id", "aud": "aa", "sub": "synthetic", "mirea": "owner", "exp": time.Now().Unix() + 600})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			claims, err := v.Verify(context.Background(), token, "")
			if err != nil || claims == nil {
				t.Error("valid token rejected")
			}
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("JWKS fetched %d times", calls.Load())
	}
	if claims, _ := v.Verify(context.Background(), token, "aa-link"); claims != nil {
		t.Fatal("wrong audience accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("known key caused another fetch")
	}
}
