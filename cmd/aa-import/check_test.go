package main

import (
	"aa-server/internal/cryptocompat"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestWrongMasterKeyStopsImportPreflight(t *testing.T) {
	good, _ := cryptocompat.NewFernet(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	wrong, _ := cryptocompat.NewFernet(base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	secret, err := good.Seal([]byte(`{"cookie":"session=synthetic"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var content bytes.Buffer
	encoder := json.NewEncoder(&content)
	_ = encoder.Encode(map[string]any{"type": "table", "schema": "aa", "name": "accounts", "columns": []string{"id", "secret"}})
	_ = encoder.Encode(map[string]any{"type": "row", "values": []string{"synthetic", secret}})
	if err = verifyCredentials(bytes.NewReader(content.Bytes()), wrong); err == nil {
		t.Fatal("wrong key accepted")
	}
	reader := bytes.NewReader(content.Bytes())
	if err = verifyCredentials(reader, good); err != nil {
		t.Fatal(err)
	}
	offset, _ := reader.Seek(0, 1)
	if offset != 0 {
		t.Fatal("reader was not rewound")
	}
}
