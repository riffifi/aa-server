package postgres

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

func TestUsageDefinerFunctionsDoNotExposeIdentityTables(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	role := pgx.Identifier{fmt.Sprintf("aa_test_usage_%d", time.Now().UnixNano())}.Sanitize()
	for _, q := range []string{"CREATE ROLE " + role + " NOLOGIN", "GRANT USAGE ON SCHEMA aa TO " + role, "GRANT EXECUTE ON FUNCTION aa.mirea_blocked(TEXT),aa.record_login(TEXT,TEXT) TO " + role, "SET LOCAL ROLE " + role} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var blocked bool
	if err := tx.QueryRow(ctx, "SELECT aa.mirea_blocked('test')").Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT aa.record_login('missing','app')"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT denied"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT * FROM aaid.users"); err == nil {
		t.Fatal("identity rows exposed to function-only role")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT denied"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT aa.set_mirea_ban('test',true,'')"); err == nil {
		t.Fatal("AA ID runtime can invoke panel ban")
	}
}
