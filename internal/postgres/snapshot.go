package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

const metadataColumns = "id,pulse_id,label,group_id,group_name,state,expires_at,checked_at,version,session_version,pass_version,updated_at,username,pass_allowed,page_seen_at"

func (s *Store) Metadata(ctx context.Context, groupID *int64) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+metadataColumns+" FROM aa.accounts WHERE ($1::bigint IS NULL OR group_id=$1) ORDER BY label COLLATE \"C\"", groupID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
func (s *Store) MetadataByPulse(ctx context.Context, pulseID string) (map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+metadataColumns+" FROM aa.accounts WHERE pulse_id=$1", pulseID)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}
func (s *Store) MetadataAccount(ctx context.Context, id string) (map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+metadataColumns+" FROM aa.accounts WHERE id=$1", id)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}
func (v *Vault) OwnsSiteAccount(ctx context.Context, token, id string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var allowed bool
	err := v.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM aa.site_sessions s JOIN aa.accounts a ON a.pulse_id=s.mirea
  WHERE s.key_hash=$1 AND s.expires>$2 AND a.id=$3)`, Digest(token), v.now(), id).Scan(&allowed)
	return allowed, err
}
func (s *Store) EachAccount(ctx context.Context, visit func(map[string]any) error) error {
	rows, err := s.Pool.Query(ctx, "SELECT * FROM aa.accounts ORDER BY label COLLATE \"C\"")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := pgx.RowToMap(rows)
		if err != nil {
			return err
		}
		if err = visit(row); err != nil {
			return err
		}
	}
	return rows.Err()
}
func (s *Store) SharedMetadata(ctx context.Context, id string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT a."+strings.ReplaceAll(metadataColumns, ",", ",a.")+`,p.created AS shared_at FROM aa.pass_shares p
 JOIN aa.accounts a ON a.id=p.account_id WHERE p.grantee_id=$1 ORDER BY a.label COLLATE "C"`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}

// PassNumbers streams only authorized credentials, retaining no browser state in
// the returned snapshot. A large saved browser state is processed one row at a time.
func (v *Vault) PassNumbers(ctx context.Context, ids []string) (map[string]any, error) {
	out := map[string]any{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := v.Pool.Query(ctx, "SELECT id,secret FROM aa.accounts WHERE id=ANY($1::text[])", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, encrypted string
		if err = rows.Scan(&id, &encrypted); err != nil {
			return nil, err
		}
		secret, err := v.Secret(map[string]any{"secret": encrypted})
		if err != nil {
			return nil, err
		}
		out[id] = secret["pass_number"]
	}
	return out, rows.Err()
}
