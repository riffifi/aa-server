package cryptocompat

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

var ErrJWT = errors.New("invalid access token")

func KeyID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])[:16]
}

func Sign(seed []byte, claims map[string]any) (string, error) {
	if len(seed) != ed25519.SeedSize {
		return "", errors.New("invalid signing seed")
	}
	key := ed25519.NewKeyFromSeed(seed)
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": KeyID(key.Public().(ed25519.PublicKey))})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(unsigned))), nil
}

func decodeJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrJWT
	}
	return nil
}

func Verify(token string, keys map[string]ed25519.PublicKey, issuer, audience string, now time.Time, requireMirea bool) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrJWT
	}
	decode := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")) }
	headerRaw, err := decode(parts[0])
	if err != nil {
		return nil, ErrJWT
	}
	var header map[string]any
	if decodeJSON(headerRaw, &header) != nil || header["alg"] != "EdDSA" {
		return nil, ErrJWT
	}
	kid, ok := header["kid"].(string)
	if !ok {
		return nil, ErrJWT
	}
	key := keys[kid]
	if len(key) != ed25519.PublicKeySize {
		return nil, ErrJWT
	}
	signature, err := decode(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return nil, ErrJWT
	}
	payload, err := decode(parts[1])
	if err != nil {
		return nil, ErrJWT
	}
	var claims map[string]any
	if decodeJSON(payload, &claims) != nil || claims["iss"] != issuer || claims["aud"] != audience {
		return nil, ErrJWT
	}
	exp, ok := claims["exp"].(json.Number)
	if !ok {
		return nil, ErrJWT
	}
	expiry, err := exp.Int64()
	if err != nil || float64(expiry) <= float64(now.UnixNano())/1e9 {
		return nil, ErrJWT
	}
	if _, ok := claims["sub"].(string); !ok {
		return nil, ErrJWT
	}
	if requireMirea {
		if _, ok := claims["mirea"].(string); !ok {
			return nil, ErrJWT
		}
	}
	return claims, nil
}
