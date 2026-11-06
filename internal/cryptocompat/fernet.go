// Package cryptocompat preserves the existing Python service's on-disk formats.
package cryptocompat

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
)

var ErrToken = errors.New("invalid encrypted token")

type Fernet struct{ signing, encryption []byte }

func NewFernet(encoded string) (*Fernet, error) {
	key, err := base64.URLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid Fernet key")
	}
	return &Fernet{key[:16], key[16:]}, nil
}

// Open authenticates before decrypting. Existing Vault.secret does not enforce TTL.
func (f *Fernet) Open(token string) ([]byte, error) {
	raw, err := base64.URLEncoding.DecodeString(token)
	if err != nil || len(raw) < 73 || raw[0] != 0x80 {
		return nil, ErrToken
	}
	message, tag := raw[:len(raw)-32], raw[len(raw)-32:]
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(message)
	if !hmac.Equal(mac.Sum(nil), tag) {
		return nil, ErrToken
	}
	encrypted := message[25:]
	if len(encrypted) == 0 || len(encrypted)%aes.BlockSize != 0 {
		return nil, ErrToken
	}
	block, err := aes.NewCipher(f.encryption)
	if err != nil {
		return nil, ErrToken
	}
	plain := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, message[9:25]).CryptBlocks(plain, encrypted)
	padding := int(plain[len(plain)-1])
	if padding < 1 || padding > aes.BlockSize || padding > len(plain) {
		return nil, ErrToken
	}
	if !bytes.Equal(plain[len(plain)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return nil, ErrToken
	}
	return plain[:len(plain)-padding], nil
}

func (f *Fernet) Seal(plain []byte, now time.Time) (string, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	block, err := aes.NewCipher(f.encryption)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 25+len(padded))
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(now.Unix()))
	copy(raw[9:25], iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(raw[25:], padded)
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(raw)
	return base64.URLEncoding.EncodeToString(append(raw, mac.Sum(nil)...)), nil
}

func PassTestingKey(masterKey []byte) []byte {
	data := append([]byte("pass-testing-key\x00"), bytes.TrimSpace(masterKey)...)
	sum := sha256.Sum256(data)
	return sum[:]
}

type DailyAccess struct {
	Password  string  `json:"password"`
	ExpiresAt float64 `json:"expires_at"`
	Day       string  `json:"day"`
	Timezone  string  `json:"timezone"`
}

func Daily(key []byte, now time.Time) DailyAccess {
	zone := time.FixedZone("Europe/Moscow", 3*3600)
	current := now.In(zone)
	day := current.Format("2006-01-02")
	midnight := time.Date(current.Year(), current.Month(), current.Day()+1, 0, 0, 0, 0, zone)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("pass-testing:" + day))
	code := base32.StdEncoding.EncodeToString(mac.Sum(nil))[:20]
	return DailyAccess{code[:5] + "-" + code[5:10] + "-" + code[10:15] + "-" + code[15:], float64(midnight.Unix()), day, "Europe/Moscow"}
}

func MatchDaily(candidate string, access DailyAccess) bool {
	candidate = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(candidate), "-", ""))
	expected := strings.ReplaceAll(access.Password, "-", "")
	return len(candidate) <= 128 && hmac.Equal([]byte(candidate), []byte(expected))
}
