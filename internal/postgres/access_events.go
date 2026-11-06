package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
)

func (v *Vault) notifyAccess(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `WITH bumped AS(UPDATE aa.accounts SET version=version+1,updated_at=$2 WHERE id=ANY($1::text[]) RETURNING id,version,group_id,pulse_id) INSERT INTO aa.events(account_id,version,kind,created,group_id,audience_mirea) SELECT id,version,'access',$2,group_id,pulse_id FROM bumped ORDER BY id`, ids, v.now())
	return err
}
func (v *Vault) notifyGrantees(ctx context.Context, tx pgx.Tx, id string) error {
	rows, err := tx.Query(ctx, "SELECT grantee_id FROM aa.pass_shares WHERE account_id=$1", id)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return v.notifyAccess(ctx, tx, ids)
}
