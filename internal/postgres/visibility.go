package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
	"regexp"
	"strings"
)

var shareUsername = regexp.MustCompile(`^[a-z][a-z0-9_]{2,23}$`)

func (s *Store) ExactPerson(ctx context.Context, query, exclude string) ([]map[string]any, error) {
	query = strings.ToLower(strings.TrimLeft(strings.TrimSpace(query), "@"))
	if !shareUsername.MatchString(query) {
		return []map[string]any{}, nil
	}
	rows, err := s.Pool.Query(ctx, "SELECT id,pulse_id,group_id,username,label,group_name FROM aa.accounts WHERE username=$1 AND id<>$2 LIMIT 1", query, exclude)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}

const visibleAccount = `(a.pulse_id=$1 OR (a.pass_allowed=1 AND EXISTS(SELECT 1 FROM aa.pass_shares p JOIN aa.accounts own ON own.id=p.grantee_id WHERE p.account_id=a.id AND own.pulse_id=$1)))`

// VisibleMetadata applies the permission in SQL before any account data is read.
// Group IDs are display filters, never an authorization credential.
func (s *Store) VisibleMetadata(ctx context.Context, mirea string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+metadataColumns+" FROM aa.accounts a WHERE "+visibleAccount+` ORDER BY label COLLATE "C"`, mirea)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
func (v *Vault) VisiblePassNumbers(ctx context.Context, mirea string, ids []string) (map[string]any, error) {
	out := map[string]any{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := v.Pool.Query(ctx, "SELECT id,secret FROM aa.accounts a WHERE a.id=ANY($2::text[]) AND a.pass_allowed=1 AND "+visibleAccount, mirea, ids)
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
func (s *Store) VisibleEvents(ctx context.Context, mirea string, after int64, group *int64) ([]map[string]any, int64, error) {
	rows, err := s.Pool.Query(ctx, `WITH batch AS (SELECT * FROM aa.events WHERE id>$2 ORDER BY id LIMIT 1000)
 SELECT e.*,(e.audience_mirea=$1 OR EXISTS(SELECT 1 FROM aa.accounts a WHERE a.id=e.account_id AND a.pulse_id=$1)) IS TRUE AS visible FROM batch e ORDER BY id`, mirea, after)
	if err != nil {
		return nil, after, err
	}
	all, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return nil, after, err
	}
	result := []map[string]any{}
	cursor := after
	for _, row := range all {
		cursor = row["id"].(int64)
		visible, _ := row["visible"].(bool)
		personal := row["audience_mirea"] == mirea
		delete(row, "audience_mirea")
		delete(row, "visible")
		if visible && (personal || group == nil || row["group_id"] == *group || row["previous_group_id"] == *group) {
			result = append(result, row)
		}
	}
	return result, cursor, nil
}
