package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrBlocked = errors.New("AA ID is blocked")
var ErrUsernameTaken = errors.New("username is taken")

type Identity struct {
	*Store
	Now func() time.Time
}

func NewIdentity(store *Store) *Identity { return &Identity{store, time.Now} }
func (s *Identity) now() float64         { return float64(s.Now().UnixMicro()) / 1e6 }
func (s *Identity) User(ctx context.Context, id string) (map[string]any, error) {
	rows, err := s.Pool.Query(ctx, `SELECT u.id,u.status,u.created,u.username,l.pulse_uid,l.group_id,l.group_name,l.verified_at
 FROM aaid.users u JOIN aaid.mirea_links l ON l.user_id=u.id WHERE u.id=$1`, id)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}
func (s *Identity) audit(ctx context.Context, tx pgx.Tx, action string, userID, sessionID *string) error {
	_, err := tx.Exec(ctx, "INSERT INTO aaid.audit(user_id,action,session_id,created) VALUES($1,$2,$3,$4)", userID, action, sessionID, s.now())
	return err
}
func (s *Identity) BindMirea(ctx context.Context, pulseID string, groupID *int64, groupName *string) (map[string]any, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,6067703))", pulseID); err != nil {
		return nil, err
	}
	row, err := one(ctx, tx, "SELECT user_id FROM aaid.mirea_links WHERE pulse_uid=$1", pulseID)
	if err != nil {
		return nil, err
	}
	var id string
	if row == nil {
		id, err = uuid()
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO aaid.users(id,created) VALUES($1,$2)", id, s.now()); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO aaid.mirea_links(user_id,pulse_uid,group_id,group_name,linked_at,verified_at) VALUES($1,$2,$3,$4,$5,$5)", id, pulseID, groupID, groupName, s.now()); err != nil {
			return nil, err
		}
		if err = s.audit(ctx, tx, "created", &id, nil); err != nil {
			return nil, err
		}
	} else {
		id = row["user_id"].(string)
		var name *string
		if groupID != nil {
			name = groupName
		}
		if _, err = tx.Exec(ctx, "UPDATE aaid.mirea_links SET verified_at=$1,group_id=COALESCE($2,group_id),group_name=COALESCE($3,group_name) WHERE user_id=$4", s.now(), groupID, name, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	user, err := s.User(ctx, id)
	if err != nil {
		return nil, err
	}
	if user["status"] != "active" {
		return nil, ErrBlocked
	}
	return user, nil
}
func (s *Identity) SetUsername(ctx context.Context, id string, username *string) (map[string]any, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "UPDATE aaid.users SET username=$1 WHERE id=$2", username, id); err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	if err = s.audit(ctx, tx, "username", &id, nil); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.User(ctx, id)
}
func (s *Identity) Session(ctx context.Context, id string) (map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id,user_id,device,created,last_used,expires,revoked_at,revoke_reason,kind FROM aaid.sessions WHERE id=$1", id)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}
func (s *Identity) ActiveSession(ctx context.Context, id string) (map[string]any, error) {
	row, err := s.Session(ctx, id)
	if err != nil || row == nil {
		return nil, err
	}
	if row["revoked_at"] != nil || row["expires"].(float64) <= s.now() {
		return nil, nil
	}
	return row, nil
}
func (s *Identity) CreateSession(ctx context.Context, userID, device string, ttl float64, maximum int, kind string) (map[string]any, string, error) {
	if kind != "app" && kind != "web" {
		return nil, "", errors.New("invalid session kind")
	}
	if maximum < 1 {
		return nil, "", errors.New("invalid session limit")
	}
	id, err := uuid()
	if err != nil {
		return nil, "", err
	}
	token, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	owner, err := one(ctx, tx, "SELECT id FROM aaid.users WHERE id=$1 FOR UPDATE", userID)
	if err != nil {
		return nil, "", err
	}
	if owner == nil {
		return nil, "", ErrNotFound
	}
	now := s.now()
	if _, err = tx.Exec(ctx, "INSERT INTO aaid.sessions(id,user_id,device,created,last_used,expires,kind) VALUES($1,$2,$3,$4,$4,$5,$6)", id, userID, device, now, now+ttl, kind); err != nil {
		return nil, "", err
	}
	table := "aaid.refresh_tokens"
	if kind == "web" {
		table = "aaid.web_tokens"
	}
	if _, err = tx.Exec(ctx, "INSERT INTO "+table+"(hash,session_id,created) VALUES($1,$2,$3)", Digest(token), id, now); err != nil {
		return nil, "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE aaid.sessions SET revoked_at=$1,revoke_reason='evicted' WHERE id IN
  (SELECT id FROM aaid.sessions WHERE user_id=$2 AND kind=$3 AND revoked_at IS NULL AND expires>$1
   ORDER BY last_used DESC,created DESC,ordinal OFFSET $4)`, now, userID, kind, maximum); err != nil {
		return nil, "", err
	}
	if err = s.audit(ctx, tx, "signed_in", &userID, &id); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	row, err := s.Session(ctx, id)
	return row, token, err
}
func (s *Identity) WebSession(ctx context.Context, token string) (map[string]any, error) {
	if token == "" {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT s.id,s.user_id,s.device,s.created,s.last_used,s.expires,s.revoked_at,s.revoke_reason,s.kind
 FROM aaid.sessions s JOIN aaid.web_tokens t ON t.session_id=s.id WHERE t.hash=$1 AND s.revoked_at IS NULL AND s.expires>$2`, Digest(token), s.now())
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}
func (s *Identity) Rotate(ctx context.Context, refresh string, ttl float64) (map[string]any, string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	token, err := one(ctx, tx, "SELECT * FROM aaid.refresh_tokens WHERE hash=$1 FOR UPDATE", Digest(refresh))
	if err != nil {
		return nil, "", err
	}
	if token == nil {
		return nil, "", tx.Commit(ctx)
	}
	sessionID := token["session_id"].(string)
	session, err := one(ctx, tx, "SELECT * FROM aaid.sessions WHERE id=$1 FOR UPDATE", sessionID)
	if err != nil {
		return nil, "", err
	}
	if session == nil {
		return nil, "", tx.Commit(ctx)
	}
	now := s.now()
	userID := session["user_id"].(string)
	if token["used_at"] != nil {
		if _, err = tx.Exec(ctx, "UPDATE aaid.sessions SET revoked_at=$1,revoke_reason='refresh_reuse' WHERE id=$2 AND revoked_at IS NULL", now, sessionID); err != nil {
			return nil, "", err
		}
		if err = s.audit(ctx, tx, "refresh_reuse", &userID, &sessionID); err != nil {
			return nil, "", err
		}
		return nil, "", tx.Commit(ctx)
	}
	if session["revoked_at"] != nil || session["expires"].(float64) <= now {
		return nil, "", tx.Commit(ctx)
	}
	fresh, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE aaid.refresh_tokens SET used_at=$1 WHERE hash=$2", now, token["hash"]); err != nil {
		return nil, "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO aaid.refresh_tokens(hash,session_id,created) VALUES($1,$2,$3)", Digest(fresh), sessionID, now); err != nil {
		return nil, "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE aaid.sessions SET last_used=$1,expires=$2 WHERE id=$3", now, now+ttl, sessionID); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	row, err := s.Session(ctx, sessionID)
	return row, fresh, err
}
func (s *Identity) Sessions(ctx context.Context, userID string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id,device,kind,created,last_used FROM aaid.sessions WHERE user_id=$1 AND revoked_at IS NULL AND expires>$2 ORDER BY last_used DESC,ordinal", userID, s.now())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
func (s *Identity) Revoke(ctx context.Context, userID, id, reason string) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	changed, err := tx.Exec(ctx, "UPDATE aaid.sessions SET revoked_at=$1,revoke_reason=$2 WHERE id=$3 AND user_id=$4 AND revoked_at IS NULL", s.now(), reason, id, userID)
	if err != nil {
		return false, err
	}
	if changed.RowsAffected() > 0 {
		if err = s.audit(ctx, tx, "revoked:"+reason, &userID, &id); err != nil {
			return false, err
		}
	}
	return changed.RowsAffected() > 0, tx.Commit(ctx)
}
func (s *Identity) SetStatus(ctx context.Context, id, status string) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	changed, err := tx.Exec(ctx, "UPDATE aaid.users SET status=$1 WHERE id=$2", status, id)
	if err != nil {
		return false, err
	}
	if changed.RowsAffected() > 0 {
		if status != "active" {
			if _, err = tx.Exec(ctx, "UPDATE aaid.sessions SET revoked_at=$1,revoke_reason='blocked' WHERE user_id=$2 AND revoked_at IS NULL", s.now(), id); err != nil {
				return false, err
			}
		}
		if err = s.audit(ctx, tx, "status:"+status, &id, nil); err != nil {
			return false, err
		}
	}
	return changed.RowsAffected() > 0, tx.Commit(ctx)
}
