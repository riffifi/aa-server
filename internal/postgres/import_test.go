package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"aa-server/internal/cryptocompat"
)

func syntheticSnapshot(t *testing.T, secret string, corrupt bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	emit := func(record any) {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		buffer.Write(data)
		buffer.WriteByte('\n')
	}
	emit(map[string]any{"type": "header", "version": 1})
	counts := map[string]int64{}
	table := func(schema, name string, columns []string, rows [][]any) {
		emit(map[string]any{"type": "table", "schema": schema, "name": name, "columns": columns})
		counts[schema+"."+name] = int64(len(rows))
		for _, row := range rows {
			emit(map[string]any{"type": "row", "values": row})
		}
	}
	table("aa", "config", []string{"key", "value"}, [][]any{{"admin_password", "synthetic-hash"}})
	table("aa", "accounts", []string{"id", "pulse_id", "label", "secret", "state", "checked_at", "version", "updated_at"}, [][]any{{"synthetic-import", "synthetic-pulse", "Imported", secret, "valid", 1000, 1, 1000}})
	table("aa", "events", []string{"id", "account_id", "version", "kind", "created"}, [][]any{{7, "synthetic-import", 1, "session", 1000}})
	table("aa_legacy", "devices", []string{"id", "name", "key_hash", "scopes", "publisher", "revoked", "last_seen"}, [][]any{{"synthetic-legacy-device", "Old phone", "synthetic-key-hash", `["*"]`, 1, 0, 1234.5}})
	table("aaid", "users", []string{"id", "status", "created"}, nil)
	table("aaid", "mirea_links", []string{"user_id", "pulse_uid", "linked_at", "verified_at"}, nil)
	table("aaid", "sessions", []string{"id", "user_id", "device", "created", "last_used", "expires"}, nil)
	table("aaid", "refresh_tokens", []string{"hash", "session_id", "created"}, nil)
	sum := sha256.Sum256(buffer.Bytes())
	checksum := hex.EncodeToString(sum[:])
	if corrupt {
		checksum = strings.Repeat("0", 64)
	}
	emit(map[string]any{"type": "summary", "counts": counts, "sha256": checksum})
	return buffer.Bytes()
}

func TestSnapshotImportIsAtomicPreservesSecretsAndResumesSequences(t *testing.T) {
	databaseURL := os.Getenv("TEST_IMPORT_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_IMPORT_DATABASE_URL is required")
	}
	ctx := context.Background()
	store, err := Open(ctx, databaseURL, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var name string
	if err = store.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "aa_test_") || !strings.HasSuffix(name, "_import") {
		t.Fatal("import tests require a dedicated aa_test_*_import database")
	}
	key := base64.URLEncoding.EncodeToString(make([]byte, 32))
	cipher, err := cryptocompat.NewFernet(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Seal([]byte(`{"cookie":"session=synthetic"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ImportSnapshot(ctx, bytes.NewReader(syntheticSnapshot(t, encrypted, true))); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
	var count int
	if err = store.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM aa.accounts").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed import left rows behind")
	}
	if err = store.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM aa_legacy.devices").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed import left archived device rows behind")
	}
	counts, err := store.ImportSnapshot(ctx, bytes.NewReader(syntheticSnapshot(t, encrypted, false)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Pool.Exec(ctx, "DELETE FROM aa.events WHERE account_id='synthetic-import'; DELETE FROM aa.accounts WHERE id='synthetic-import'; DELETE FROM aa.config WHERE key='admin_password'; DELETE FROM aa_legacy.devices WHERE id='synthetic-legacy-device'")
	}()
	if counts["aa.accounts"] != 1 || counts["aa.events"] != 1 {
		t.Fatal("row counts changed")
	}
	var keyHash, scopes string
	var revoked int64
	var lastSeen float64
	if err = store.Pool.QueryRow(ctx, "SELECT key_hash,scopes,revoked,last_seen FROM aa_legacy.devices WHERE id='synthetic-legacy-device'").Scan(&keyHash, &scopes, &revoked, &lastSeen); err != nil || keyHash != "synthetic-key-hash" || scopes != `["*"]` || revoked != 0 || lastSeen != 1234.5 {
		t.Fatalf("legacy device archive changed: %v", err)
	}
	vault, err := NewVault(store, key)
	if err != nil {
		t.Fatal(err)
	}
	row, err := vault.Account(ctx, "synthetic-import")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Secret(row)
	if err != nil || secret["cookie"] != "session=synthetic" {
		t.Fatal("encrypted session did not survive migration")
	}
	var next int64
	if err = store.Pool.QueryRow(ctx, "INSERT INTO aa.events(account_id,version,kind,created) VALUES('synthetic-import',2,'label',1001) RETURNING id").Scan(&next); err != nil || next != 8 {
		t.Fatalf("sequence did not resume after imported id: %d %v", next, err)
	}
	if _, err = store.ImportSnapshot(ctx, bytes.NewReader(syntheticSnapshot(t, encrypted, false))); err == nil {
		t.Fatal("nonempty target was overwritten")
	}
}
