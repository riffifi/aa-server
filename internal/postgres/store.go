// Package postgres is the Go service's PostgreSQL storage layer.
// It never opens SQLite files and never generates replacement encryption keys.
package postgres

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string, maxConnections int32) (*Store, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	if maxConnections < 1 {
		return nil, errors.New("database pool must have at least one connection")
	}
	config.MaxConns = maxConnections
	config.MinConns = 0
	config.MaxConnIdleTime = 5 * time.Minute
	config.MaxConnLifetime = time.Hour
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "15000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Initialize is explicitly invoked by migration tooling, not on every HTTP request.
func (s *Store) Initialize(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6067701)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Accounts(ctx context.Context, groupID *int64) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT * FROM aa.accounts WHERE ($1::bigint IS NULL OR group_id=$1) ORDER BY label COLLATE \"C\"", groupID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}

func (s *Store) Account(ctx context.Context, id string) (map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT * FROM aa.accounts WHERE id=$1", id)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

// Events advances over filtered-out rows too, exactly as Vault.events does.
func (s *Store) Events(ctx context.Context, after int64, groupID *int64) ([]map[string]any, int64, error) {
	rows, err := s.Pool.Query(ctx, "SELECT * FROM aa.events WHERE id>$1 ORDER BY id LIMIT 1000", after)
	if err != nil {
		return nil, after, err
	}
	all, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return nil, after, err
	}
	out := make([]map[string]any, 0, len(all))
	cursor := after
	for _, row := range all {
		delete(row, "audience_mirea")
		cursor = row["id"].(int64)
		if groupID == nil || row["group_id"] == *groupID || row["previous_group_id"] == *groupID {
			out = append(out, row)
		}
	}
	return out, cursor, nil
}

// Write serializes event publishers across processes. Sequence IDs alone do not
// guarantee commit order in PostgreSQL; a late commit must not be skipped by sync.
func (s *Store) Write(ctx context.Context, update func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6067702)"); err != nil {
		return err
	}
	if uid, _ := ctx.Value(actorContextKey{}).(string); uid != "" {
		var blocked bool
		if err = tx.QueryRow(ctx, "SELECT aa.mirea_blocked($1)", uid).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return ErrBlocked
		}
	}
	if err = update(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func Public(row map[string]any) map[string]any {
	keys := []string{"id", "label", "group_id", "group_name", "state", "expires_at", "checked_at", "version", "session_version", "pass_version", "updated_at", "username"}
	out := make(map[string]any, len(keys)+1)
	for _, key := range keys {
		out[key] = row[key]
	}
	allowed, ok := row["pass_allowed"]
	out["pass_allowed"] = !ok || (allowed != nil && allowed != int64(0))
	return out
}
