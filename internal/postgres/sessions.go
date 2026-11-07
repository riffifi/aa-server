package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"unicode/utf8"

	"aa-server/internal/cryptocompat"
	"github.com/jackc/pgx/v5"
)

func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (v *Vault) InitializeAdmin(ctx context.Context, password string) error {
	var exists bool
	if err := v.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM aa.config WHERE key='admin_password')").Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if utf8.RuneCountInString(password) < 16 {
		return errors.New("ADMIN_PASSWORD must contain at least 16 characters for first startup")
	}
	hash, err := cryptocompat.PasswordHash(password, "")
	if err != nil {
		return err
	}
	_, err = v.Pool.Exec(ctx, "INSERT INTO aa.config VALUES('admin_password',$1) ON CONFLICT(key) DO NOTHING", hash)
	return err
}

func (v *Vault) AdminLogin(ctx context.Context, password string) (token, csrf string, err error) {
	var record string
	if err = v.Pool.QueryRow(ctx, "SELECT value FROM aa.config WHERE key='admin_password'").Scan(&record); err != nil {
		return "", "", err
	}
	if !cryptocompat.PasswordMatches(password, record) {
		return "", "", nil
	}
	token, err = randomToken()
	if err != nil {
		return "", "", err
	}
	csrf, err = randomToken()
	if err != nil {
		return "", "", err
	}
	tx, err := v.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "DELETE FROM aa.panel_sessions WHERE expires<$1", v.now()); err != nil {
		return "", "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO aa.panel_sessions VALUES($1,$2,$3)", Digest(token), csrf, v.now()+43200); err != nil {
		return "", "", err
	}
	return token, csrf, tx.Commit(ctx)
}

func (v *Vault) Session(ctx context.Context, kind, token string) (map[string]any, error) {
	if token == "" {
		return nil, nil
	}
	var query string
	switch kind {
	case "panel":
		query = "SELECT * FROM aa.panel_sessions WHERE key_hash=$1 AND expires>$2"
	case "site":
		query = "SELECT * FROM aa.site_sessions WHERE key_hash=$1 AND expires>$2"
	default:
		return nil, errors.New("invalid session kind")
	}
	rows, err := v.Pool.Query(ctx, query, Digest(token), v.now())
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

func (v *Vault) Logout(ctx context.Context, kind, token string) error {
	var query string
	switch kind {
	case "panel":
		query = "DELETE FROM aa.panel_sessions WHERE key_hash=$1"
	case "site":
		query = "DELETE FROM aa.site_sessions WHERE key_hash=$1"
	default:
		return errors.New("invalid session kind")
	}
	_, err := v.Pool.Exec(ctx, query, Digest(token))
	return err
}

func (v *Vault) CreateSiteSession(ctx context.Context, aaidID, mirea string, groupID *int64, groupName *string, ttl float64) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	tx, err := v.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "DELETE FROM aa.site_sessions WHERE expires<$1", v.now()); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO aa.site_sessions(key_hash,aaid_id,mirea,group_id,group_name,expires) VALUES($1,$2,$3,$4,$5,$6)", Digest(token), aaidID, mirea, groupID, groupName, v.now()+ttl); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE aa.accounts SET page_seen_at=$1 WHERE pulse_id=$2", v.now(), mirea); err != nil {
		return "", err
	}
	return token, tx.Commit(ctx)
}

func (v *Vault) BindSiteIdentity(ctx context.Context, token, sessionID string) error {
	_, err := v.Pool.Exec(ctx, "UPDATE aa.site_sessions SET aaid_session_id=$1 WHERE key_hash=$2", sessionID, Digest(token))
	return err
}

func (v *Vault) IdentityActive(ctx context.Context, user, mirea, session string) (bool, error) {
	if user == "" || mirea == "" || session == "" {
		return false, nil
	}
	var active bool
	err := v.Pool.QueryRow(ctx, "SELECT aa.identity_active($1,$2,$3)", user, mirea, session).Scan(&active)
	return active, err
}

func (v *Vault) VerifiedIdentity(ctx context.Context, user, mirea, session string) (map[string]any, error) {
	if user == "" || mirea == "" || session == "" {
		return nil, nil
	}
	rows, err := v.Pool.Query(ctx, "SELECT * FROM aa.identity_context($1,$2,$3)", user, mirea, session)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

func (s *Store) AccountByPulse(ctx context.Context, pulseID string) (map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT * FROM aa.accounts WHERE pulse_id=$1", pulseID)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

func ClientHash(key string) any {
	if len(key) < 32 || len(key) > 128 {
		return nil
	}
	for _, b := range []byte(key) {
		if b > 127 {
			return nil
		}
	}
	return Digest(key)
}

// PassStatuses retains the old snapshot shape without enabling legacy Telegram grants.
func (v *Vault) PassStatuses(ctx context.Context, ids []string, clientKey string) (map[string]string, error) {
	rows, err := v.Pool.Query(ctx, `SELECT a.id,CASE
  WHEN a.pass_allowed=0 THEN 'disabled'
  ELSE 'owner_unlinked' END AS status
 FROM aa.accounts a WHERE a.id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err = rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

func (s *Store) SharedWith(ctx context.Context, id string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, `SELECT a.*,p.created AS shared_at FROM aa.pass_shares p JOIN aa.accounts a ON a.id=p.account_id
 WHERE p.grantee_id=$1 ORDER BY a.label COLLATE "C"`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}

func (v *Vault) DeleteAccount(ctx context.Context, id string) error {
	return v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT group_id,pulse_id FROM aa.accounts WHERE id=$1 FOR UPDATE", id)
		if err != nil {
			return err
		}
		var previous any
		if row != nil {
			previous = row["group_id"]
			if err = v.notifyGrantees(ctx, tx, id); err != nil {
				return err
			}
		}
		queries := []string{
			"DELETE FROM aa.accounts WHERE id=$1", "DELETE FROM aa.telegram_links WHERE account_id=$1",
			"UPDATE aa.telegram_users SET aaid_id=NULL,account_id=NULL,linked=NULL WHERE account_id=$1",
			"DELETE FROM aa.telegram_codes WHERE account_id=$1", "DELETE FROM aa.pass_shares WHERE account_id=$1 OR grantee_id=$1",
			"DELETE FROM aa.share_requests WHERE account_id=$1 OR requester_id=$1", "DELETE FROM aa.pass_requests WHERE account_id=$1",
			"DELETE FROM aa.pass_grants WHERE account_id=$1", "DELETE FROM aa.marks WHERE account_id=$1",
		}
		for _, query := range queries {
			if _, err = tx.Exec(ctx, query, id); err != nil {
				return err
			}
		}
		if row != nil {
			return v.event(ctx, tx, id, 0, "deleted", previous, row["pulse_id"].(string))
		}
		return v.event(ctx, tx, id, 0, "deleted", previous)
	})
}
