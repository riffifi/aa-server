package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
)

func (v *Vault) RecordMark(ctx context.Context, id, state, message, source string) error {
	runes := []rune(message)
	if len(runes) > 200 {
		message = string(runes[:200])
	}
	tx, err := v.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO aa.marks(account_id,state,message,source,created) VALUES($1,$2,$3,$4,$5)", id, state, message, source, v.now()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM aa.marks WHERE created<$1", v.now()-30*86400); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (v *Vault) MarksSince(ctx context.Context, ids []string, since float64) ([]map[string]any, error) {
	rows, err := v.Pool.Query(ctx, "SELECT * FROM aa.marks WHERE created>=$1 AND account_id=ANY($2::text[]) ORDER BY created,id", since, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
func (v *Vault) LatestMarks(ctx context.Context, ids []string, since float64) (map[string]map[string]any, error) {
	rows, err := v.MarksSince(ctx, ids, since)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		id := row["account_id"].(string)
		previous := out[id]
		if previous == nil || previous["state"] != "approved" {
			out[id] = row
		}
	}
	return out, nil
}
func (v *Vault) Audit(ctx context.Context, action, actor string, id *string) error {
	tx, err := v.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO aa.audit_events(action,actor,account_id,created) VALUES($1,$2,$3,$4)", action, actor, id, v.now()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM aa.audit_events WHERE created<$1", v.now()-30*86400); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM aa.audit_events WHERE id NOT IN(SELECT id FROM aa.audit_events ORDER BY id DESC LIMIT 10000)"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
