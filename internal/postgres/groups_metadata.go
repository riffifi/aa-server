package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
)

func (s *Store) MetadataGroups(ctx context.Context, groups []int64) ([]map[string]any, error) {
	if len(groups) == 0 {
		return []map[string]any{}, nil
	}
	rows, err := s.Pool.Query(ctx, "SELECT "+metadataColumns+` FROM aa.accounts WHERE group_id=ANY($1::bigint[]) ORDER BY label COLLATE "C"`, groups)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
