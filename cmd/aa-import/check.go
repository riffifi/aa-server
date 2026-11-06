package main

import (
	"aa-server/internal/cryptocompat"
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

func verifyCredentials(file io.ReadSeeker, cipher *cryptocompat.Fernet) error {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	account := false
	secretColumn := -1
	for scanner.Scan() {
		var record struct {
			Type, Schema, Name string
			Columns            []string
			Values             []any
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return errors.New("invalid snapshot JSON")
		}
		if record.Type == "table" {
			account = record.Schema == "aa" && record.Name == "accounts"
			secretColumn = -1
			if account {
				for i, column := range record.Columns {
					if column == "secret" {
						secretColumn = i
					}
				}
			}
		}
		if account && record.Type == "row" {
			if secretColumn < 0 || secretColumn >= len(record.Values) {
				return errors.New("snapshot account credentials are missing")
			}
			encrypted, ok := record.Values[secretColumn].(string)
			if !ok {
				return errors.New("invalid snapshot credential type")
			}
			plain, err := cipher.Open(encrypted)
			if err != nil {
				return errors.New("preserved master key does not decrypt the snapshot; nothing was imported")
			}
			var object map[string]any
			if json.Unmarshal(plain, &object) != nil || object == nil {
				return errors.New("invalid decrypted snapshot credentials")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := file.Seek(0, io.SeekStart)
	return err
}
