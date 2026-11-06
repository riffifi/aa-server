package cryptocompat

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/scrypt"
)

func PasswordHash(password, salt string) (string, error) {
	if salt == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		salt = hex.EncodeToString(raw)
	}
	raw, err := hex.DecodeString(salt)
	if err != nil {
		return "", errors.New("invalid password salt")
	}
	key, err := scrypt.Key([]byte(password), raw, 16384, 8, 1, 64)
	if err != nil {
		return "", err
	}
	return salt + ":" + hex.EncodeToString(key), nil
}

func PasswordMatches(password, record string) bool {
	salt, _, ok := strings.Cut(record, ":")
	if !ok {
		return false
	}
	candidate, err := PasswordHash(password, salt)
	return err == nil && hmac.Equal([]byte(candidate), []byte(record))
}
