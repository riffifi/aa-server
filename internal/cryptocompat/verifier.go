package cryptocompat

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Verifier struct {
	JWKSURL, Issuer, Audience string
	HTTP                      *http.Client
	Now                       func() time.Time
	mu                        sync.RWMutex
	keys                      map[string]ed25519.PublicKey
	fetched                   time.Time
	refreshMu                 sync.Mutex
}

func NewVerifier(jwksURL string, keys map[string]ed25519.PublicKey) *Verifier {
	copied := map[string]ed25519.PublicKey{}
	for id, key := range keys {
		copied[id] = append(ed25519.PublicKey(nil), key...)
	}
	return &Verifier{JWKSURL: jwksURL, Issuer: "aa-id", Audience: "aa", HTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Now: time.Now, keys: copied}
}
func (v *Verifier) Close() { v.HTTP.CloseIdleConnections() }
func (v *Verifier) refresh(ctx context.Context) {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	if v.JWKSURL == "" || !v.fetched.IsZero() && v.Now().Sub(v.fetched) < time.Minute {
		return
	}
	v.fetched = v.Now()
	request, err := http.NewRequestWithContext(ctx, "GET", v.JWKSURL, nil)
	if err != nil {
		return
	}
	response, err := v.HTTP.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return
	}
	var document struct {
		Keys []struct {
			KID   string `json:"kid"`
			KTY   string `json:"kty"`
			Curve string `json:"crv"`
			X     string `json:"x"`
		}
	}
	if json.Unmarshal(data, &document) != nil {
		return
	}
	keys := map[string]ed25519.PublicKey{}
	for _, key := range document.Keys {
		if key.KTY == "OKP" && key.Curve == "Ed25519" {
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(key.X, "="))
			if err == nil {
				keys[key.KID] = raw
			}
		}
	}
	if len(keys) > 0 {
		v.mu.Lock()
		v.keys = keys
		v.mu.Unlock()
	}
}
func (v *Verifier) Verify(ctx context.Context, token, audience string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[0], "="))
	if err != nil {
		return nil, nil
	}
	var header struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
	}
	if json.Unmarshal(raw, &header) != nil || header.Alg != "EdDSA" {
		return nil, nil
	}
	v.mu.RLock()
	_, known := v.keys[header.KID]
	v.mu.RUnlock()
	if !known {
		v.refresh(ctx)
	}
	if audience == "" {
		audience = v.Audience
	}
	v.mu.RLock()
	claims, err := Verify(token, v.keys, v.Issuer, audience, v.Now(), true)
	v.mu.RUnlock()
	if err != nil {
		return nil, nil
	}
	return claims, nil
}
func (v *Verifier) Exchange(ctx context.Context, code string) (map[string]any, error) {
	if v.JWKSURL == "" {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	endpoint := strings.TrimSuffix(v.JWKSURL, "/.well-known/jwks.json") + "/v1/site/exchange"
	request, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := v.HTTP.Do(request)
	if err != nil {
		return nil, nil
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, nil
	}
	var result map[string]any
	if decodeJSON(data, &result) != nil {
		return nil, nil
	}
	if _, ok := result["id"].(string); !ok {
		return nil, nil
	}
	if _, ok := result["mirea"].(string); !ok {
		return nil, nil
	}
	return result, nil
}
