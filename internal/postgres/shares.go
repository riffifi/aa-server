package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/cases"
)

var ErrShared = errors.New("shared")
var ErrDenied = errors.New("denied")
var ErrTooMany = errors.New("too many")

func (v *Vault) SetUsername(ctx context.Context, id string, username *string) error {
	return v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT username FROM aa.accounts WHERE id=$1 FOR UPDATE", id)
		if err != nil {
			return err
		}
		if row == nil {
			return nil
		}
		var value any
		if username != nil {
			value = *username
		}
		if row["username"] == value {
			return nil
		}
		if username != nil && *username != "" {
			if _, err = tx.Exec(ctx, "UPDATE aa.accounts SET username=NULL WHERE username=$1 AND id<>$2", *username, id); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, "UPDATE aa.accounts SET username=$1,updated_at=$2 WHERE id=$3", value, v.now(), id)
		return err
	})
}
func (v *Vault) bump(ctx context.Context, tx pgx.Tx, id string) error {
	row, err := one(ctx, tx, "UPDATE aa.accounts SET version=version+1,updated_at=$1 WHERE id=$2 RETURNING version", v.now(), id)
	if err != nil || row == nil {
		return err
	}
	return v.event(ctx, tx, id, row["version"].(int64), "pass_agreement", nil)
}
func (v *Vault) SharePass(ctx context.Context, id, grantee string) error {
	return v.Write(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO aa.pass_shares VALUES($1,$2,$3) ON CONFLICT DO NOTHING", id, grantee, v.now()); err != nil {
			return err
		}
		return v.bump(ctx, tx, id)
	})
}
func (v *Vault) UnsharePass(ctx context.Context, id, grantee string) (bool, error) {
	removed := false
	err := v.Write(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, "DELETE FROM aa.pass_shares WHERE account_id=$1 AND grantee_id=$2", id, grantee)
		if err != nil {
			return err
		}
		removed = result.RowsAffected() > 0
		if removed {
			if err := v.notifyAccess(ctx, tx, []string{grantee}); err != nil {
				return err
			}
			return v.bump(ctx, tx, id)
		}
		return nil
	})
	return removed, err
}
func (s *Store) PassShares(ctx context.Context, id string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, "SELECT a."+strings.ReplaceAll(metadataColumns, ",", ",a.")+`,p.created AS shared_at FROM aa.pass_shares p
 JOIN aa.accounts a ON a.id=p.grantee_id WHERE p.account_id=$1 ORDER BY a.label COLLATE "C"`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
func (v *Vault) RequestShare(ctx context.Context, id, requester string) (map[string]any, error) {
	var request map[string]any
	err := v.Write(ctx, func(tx pgx.Tx) error {
		var shared bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM aa.pass_shares WHERE account_id=$1 AND grantee_id=$2)", id, requester).Scan(&shared); err != nil {
			return err
		}
		if shared {
			return ErrShared
		}
		last, err := one(ctx, tx, "SELECT * FROM aa.share_requests WHERE account_id=$1 AND requester_id=$2 ORDER BY created DESC LIMIT 1", id, requester)
		if err != nil {
			return err
		}
		if last != nil && last["state"] == "pending" {
			request = last
			return nil
		}
		if last != nil && last["state"] == "denied" && last["decided"].(float64) > v.now()-86400 {
			return ErrDenied
		}
		var pending int
		if err = tx.QueryRow(ctx, "SELECT COUNT(*) FROM aa.share_requests WHERE requester_id=$1 AND state='pending'", requester).Scan(&pending); err != nil {
			return err
		}
		if pending >= 20 {
			return ErrTooMany
		}
		var raw [12]byte
		if _, err = rand.Read(raw[:]); err != nil {
			return err
		}
		identifier := base64.RawURLEncoding.EncodeToString(raw[:])
		request, err = one(ctx, tx, "INSERT INTO aa.share_requests(id,account_id,requester_id,state,created) VALUES($1,$2,$3,'pending',$4) RETURNING *", identifier, id, requester, v.now())
		return err
	})
	return request, err
}
func (s *Store) ShareRequests(ctx context.Context, id string, asOwner bool) ([]map[string]any, error) {
	column, other := "requester_id", "account_id"
	if asOwner {
		column, other = "account_id", "requester_id"
	}
	rows, err := s.Pool.Query(ctx, "SELECT a."+strings.ReplaceAll(metadataColumns, ",", ",a.")+`,r.id AS request_id,r.created AS requested_at
 FROM aa.share_requests r JOIN aa.accounts a ON a.id=r.`+other+` WHERE r.`+column+`=$1 AND r.state='pending' ORDER BY r.created`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
func (v *Vault) DecideShare(ctx context.Context, id, requestID string, allow bool) (*string, error) {
	var requester *string
	err := v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT * FROM aa.share_requests WHERE id=$1 AND account_id=$2 AND state='pending' FOR UPDATE", requestID, id)
		if err != nil || row == nil {
			return err
		}
		state := "denied"
		if allow {
			state = "allowed"
		}
		if _, err = tx.Exec(ctx, "UPDATE aa.share_requests SET state=$1,decided=$2 WHERE id=$3", state, v.now(), requestID); err != nil {
			return err
		}
		value := row["requester_id"].(string)
		requester = &value
		if allow {
			if _, err = tx.Exec(ctx, "INSERT INTO aa.pass_shares VALUES($1,$2,$3) ON CONFLICT DO NOTHING", id, value, v.now()); err != nil {
				return err
			}
			return v.bump(ctx, tx, id)
		}
		return nil
	})
	return requester, err
}
func (s *Store) SearchPeople(ctx context.Context, query string, exclude string, maximum int) ([]map[string]any, error) {
	fold := cases.Fold()
	query = fold.String(strings.TrimLeft(strings.TrimSpace(query), "@"))
	result := make([]map[string]any, 0)
	if utf8.RuneCountInString(query) < 2 || maximum <= 0 {
		return result, nil
	}
	type candidate struct {
		row                 map[string]any
		notExact, notPrefix bool
		label               string
	}
	found := []candidate{}
	rows, err := s.Pool.Query(ctx, "SELECT "+metadataColumns+" FROM aa.accounts ORDER BY label COLLATE \"C\"")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := pgx.RowToMap(rows)
		if err != nil {
			return nil, err
		}
		if row["id"] == exclude {
			continue
		}
		username, _ := row["username"].(string)
		username = fold.String(username)
		label := fold.String(row["label"].(string))
		prefix := strings.HasPrefix(username, query)
		if !prefix && !strings.Contains(label, query) {
			continue
		}
		found = append(found, candidate{row, username != query, !prefix, label})
		sort.SliceStable(found, func(i, j int) bool {
			a, b := found[i], found[j]
			if a.notExact != b.notExact {
				return !a.notExact
			}
			if a.notPrefix != b.notPrefix {
				return !a.notPrefix
			}
			return a.label < b.label
		})
		if len(found) > maximum {
			found = found[:maximum]
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, item := range found {
		result = append(result, item.row)
	}
	return result, nil
}
