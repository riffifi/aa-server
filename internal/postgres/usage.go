package postgres

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
)

type actorContextKey struct{}

func WithActor(ctx context.Context, uid string) context.Context {
	return context.WithValue(ctx, actorContextKey{}, uid)
}

func (s *Store) MireaBlocked(ctx context.Context, uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	var blocked bool
	err := s.Pool.QueryRow(ctx, "SELECT aa.mirea_blocked($1)", uid).Scan(&blocked)
	return blocked, err
}
func (s *Store) RequireUnblocked(ctx context.Context, uid string) error {
	b, e := s.MireaBlocked(ctx, uid)
	if e != nil {
		return e
	}
	if b {
		return ErrBlocked
	}
	return nil
}
func (v *Vault) Usage(ctx context.Context, kind, actor, target, account string, details map[string]any) error {
	data, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = v.Pool.Exec(ctx, `INSERT INTO aa.usage_events(created,kind,actor_mirea,target_mirea,account_id,details) VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6::jsonb)`, v.now(), kind, actor, target, account, string(data))
	return err
}
func (v *Vault) UsageList(ctx context.Context, before int64, uid, kind string) ([]map[string]any, error) {
	rows, err := v.Pool.Query(ctx, `SELECT e.*,a.label AS target_label,actor.label AS actor_label FROM aa.usage_events e LEFT JOIN aa.accounts a ON a.pulse_id=e.target_mirea LEFT JOIN aa.accounts actor ON actor.pulse_id=e.actor_mirea WHERE e.kind IN ('attendance','login','pass_number_read','pass_enrolled','moderation') AND ($1::bigint=0 OR e.id<$1) AND ($2='' OR e.actor_mirea=$2 OR e.target_mirea=$2) AND ($3='' OR e.kind=$3) ORDER BY e.id DESC LIMIT 200`, before, uid, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
