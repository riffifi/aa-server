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
	rows, err := v.Pool.Query(ctx, `SELECT e.*,a.label AS target_label,actor.label AS actor_label FROM aa.usage_events e LEFT JOIN aa.accounts a ON a.pulse_id=e.target_mirea LEFT JOIN aa.accounts actor ON actor.pulse_id=e.actor_mirea WHERE ($1::bigint=0 OR e.id<$1) AND ($2='' OR e.actor_mirea=$2 OR e.target_mirea=$2) AND ($3='' OR e.kind=$3) ORDER BY e.id DESC LIMIT 200`, before, uid, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}

// Observations originate in the provider ACS response, never a client claim.
// Historical/repeated responses cannot roll back the latest known state.
func (v *Vault) ObservePresence(ctx context.Context, uid, id string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	return v.Write(ctx, func(tx pgx.Tx) error {
		previous, err := one(ctx, tx, "SELECT state,event_time FROM aa.presence_observations WHERE pulse_uid=$1 FOR UPDATE", uid)
		if err != nil {
			return err
		}
		for _, e := range events {
			stamp := float64(e["time"].(uint64))
			state := int64(2)
			if e["direction"] == "in" {
				state = 1
			}
			if previous != nil && stamp <= previous["event_time"].(float64) {
				continue
			}
			var old any
			if previous != nil {
				old = previous["state"]
			}
			if previous == nil || old != state {
				data, _ := json.Marshal(map[string]any{"previous_state": old, "new_state": state, "event_time": stamp, "source": "mirea_acs"})
				if _, err := tx.Exec(ctx, `INSERT INTO aa.usage_events(created,kind,target_mirea,account_id,details) VALUES($1,'presence_transition',$2,$3,$4::jsonb)`, v.now(), uid, id, string(data)); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO aa.presence_observations VALUES($1,$2,$3) ON CONFLICT(pulse_uid) DO UPDATE SET state=$2,event_time=$3`, uid, state, stamp); err != nil {
				return err
			}
			previous = map[string]any{"state": state, "event_time": stamp}
		}
		return nil
	})
}
