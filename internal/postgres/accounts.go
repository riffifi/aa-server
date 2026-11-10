package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aa-server/internal/cryptocompat"
	"github.com/jackc/pgx/v5"
)

var ErrNotFound = errors.New("account not found")

type Conflict struct{ Message string }

func (e *Conflict) Error() string { return e.Message }

type Vault struct {
	*Store
	Cipher *cryptocompat.Fernet
	Now    func() time.Time
}

func NewVault(store *Store, masterKey string) (*Vault, error) {
	cipher, err := cryptocompat.NewFernet(masterKey)
	if err != nil {
		return nil, err
	}
	return &Vault{Store: store, Cipher: cipher, Now: time.Now}, nil
}
func (v *Vault) now() float64 { return float64(v.Now().UnixMicro()) / 1e6 }
func (v *Vault) Secret(row map[string]any) (map[string]any, error) {
	encoded, ok := row["secret"].(string)
	if !ok {
		return nil, errors.New("missing encrypted credentials")
	}
	plain, err := v.Cipher.Open(encoded)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(plain)))
	decoder.UseNumber()
	if err = decoder.Decode(&out); err != nil || out == nil {
		return nil, errors.New("invalid credential object")
	}
	return out, nil
}
func (v *Vault) seal(secret map[string]any) (string, error) {
	plain, err := json.Marshal(secret)
	if err != nil {
		return "", err
	}
	return v.Cipher.Seal(plain, v.Now())
}
func uuid() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 15) | 64
	raw[8] = (raw[8] & 63) | 128
	encoded := hex.EncodeToString(raw[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}
func one(ctx context.Context, tx pgx.Tx, sql string, args ...any) (map[string]any, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToMap)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return out, err
}
func (v *Vault) event(ctx context.Context, tx pgx.Tx, id string, version int64, kind string, previous any, audience ...string) error {
	var recipient any
	if len(audience) > 0 {
		recipient = audience[0]
	}
	_, err := tx.Exec(ctx, `INSERT INTO aa.events(account_id,version,kind,created,group_id,previous_group_id,audience_mirea)
 VALUES($1,$2,$3,$4,(SELECT group_id FROM aa.accounts WHERE id=$1),$5,$6)`, id, version, kind, v.now(), previous, recipient)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "DELETE FROM aa.events WHERE created<$1", v.now()-7*86400)
	return err
}

type Upsert struct {
	AutomaticRenewal                   bool
	PulseID, Label, AccountID          string
	Credentials                        map[string]any
	Expected, SessionExpected, GroupID *int64
	GroupName                          *string
}

func (v *Vault) Upsert(ctx context.Context, in Upsert) (map[string]any, error) {
	var id string
	err := v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT * FROM aa.accounts WHERE pulse_id=$1 FOR UPDATE", in.PulseID)
		if err != nil {
			return err
		}
		if in.AccountID != "" && (row == nil || row["id"] != in.AccountID) {
			return &Conflict{"This login belongs to a different Pulse account"}
		}
		secret := map[string]any{}
		var version int64 = 1
		var previous any
		if row != nil {
			id = row["id"].(string)
			version = row["version"].(int64) + 1
			previous = row["group_id"]
			if in.Expected != nil && *in.Expected != row["version"] {
				return &Conflict{"A newer version is already stored; refresh before publishing"}
			}
			if in.SessionExpected != nil && *in.SessionExpected != row["session_version"] {
				return &Conflict{"A newer session is already stored"}
			}
			secret, err = v.Secret(row)
			if err != nil {
				return err
			}
		} else {
			if (in.Expected != nil && *in.Expected != 0) || (in.SessionExpected != nil && *in.SessionExpected != 0) {
				return &Conflict{"Account does not exist"}
			}
			id, err = uuid()
			if err != nil {
				return err
			}
		}
		for key, value := range in.Credentials {
			if key == "renewal_statistics" {
				continue
			}
			secret[key] = value
		}
		if _, replacing := in.Credentials["cookie"]; replacing {
			if in.AutomaticRenewal {
				finishRenewal(secret, "succeeded", v.now(), in.Credentials["expires_at"])
			} else {
				finishRenewal(secret, "interrupted", v.now(), nil)
			}
			delete(secret, "renewal_status")
			delete(secret, "renewal_attempted_at")
			if in.AutomaticRenewal {
				secret["renewal_status"] = "succeeded"
				secret["renewal_attempted_at"] = renewalStatistics(secret).LastAttempt
			}
		}
		sealed, err := v.seal(secret)
		if err != nil {
			return err
		}
		expiry := in.Credentials["expires_at"]
		if number, ok := expiry.(json.Number); ok {
			expiry, err = number.Float64()
			if err != nil {
				return err
			}
		}
		label := in.Label
		var groupID, groupName any = in.GroupID, in.GroupName
		if row != nil {
			if label == "" {
				label = row["label"].(string)
			}
			if in.GroupID == nil {
				groupID = row["group_id"]
			}
			if in.GroupName == nil {
				groupName = nil
				if in.GroupID == nil || row["group_id"] == *in.GroupID {
					groupName = row["group_name"]
				}
			}
			_, err = tx.Exec(ctx, `UPDATE aa.accounts SET label=$1,group_id=$2,group_name=$3,secret=$4,state='valid',expires_at=$5,
    checked_at=$6,version=$7,session_version=session_version+1,updated_at=$6 WHERE id=$8`, label, groupID, groupName, sealed, expiry, v.now(), version, id)
		} else {
			if label == "" {
				label = "Account"
			}
			_, err = tx.Exec(ctx, `INSERT INTO aa.accounts(id,pulse_id,label,group_id,group_name,secret,state,expires_at,checked_at,version,updated_at,pass_allowed)
    VALUES($1,$2,$3,$4,$5,$6,'valid',$7,$8,1,$8,0)`, id, in.PulseID, label, groupID, groupName, sealed, expiry, v.now())
		}
		if err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "session", previous)
	})
	if err != nil {
		return nil, err
	}
	return v.Account(ctx, id)
}

// mutate holds the account row and event publication lock for the complete change.
func (v *Vault) mutate(ctx context.Context, id string, change func(pgx.Tx, map[string]any) error) (map[string]any, error) {
	err := v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT * FROM aa.accounts WHERE id=$1 FOR UPDATE", id)
		if err != nil {
			return err
		}
		if row == nil {
			return ErrNotFound
		}
		return change(tx, row)
	})
	if err != nil {
		return nil, err
	}
	row, err := v.Account(ctx, id)
	if err != nil {
		return nil, err
	}
	return Public(row), nil
}

func expectedVersion(row map[string]any, expected *int64) error {
	if expected != nil && *expected != row["version"] {
		return &Conflict{"Account changed; refresh and retry"}
	}
	return nil
}
func (v *Vault) Rename(ctx context.Context, id, label string, expected *int64) (map[string]any, error) {
	label = strings.TrimSpace(label)
	if label == "" || utf8.RuneCountInString(label) > 60 {
		return nil, errors.New("Invalid label")
	}
	return v.mutate(ctx, id, func(tx pgx.Tx, row map[string]any) error {
		if err := expectedVersion(row, expected); err != nil {
			return err
		}
		if label == row["label"] {
			return nil
		}
		version := row["version"].(int64) + 1
		if _, err := tx.Exec(ctx, "UPDATE aa.accounts SET label=$1,version=$2,updated_at=$3 WHERE id=$4", label, version, v.now(), id); err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "label", nil)
	})
}

func (v *Vault) SetPassAllowed(ctx context.Context, id string, allowed bool, expected *int64) (map[string]any, error) {
	return v.mutate(ctx, id, func(tx pgx.Tx, row map[string]any) error {
		if err := expectedVersion(row, expected); err != nil {
			return err
		}
		var flag int64
		if allowed {
			flag = 1
		}
		if flag == row["pass_allowed"] {
			return nil
		}
		version := row["version"].(int64) + 1
		if _, err := tx.Exec(ctx, "UPDATE aa.accounts SET pass_allowed=$1,version=$2,updated_at=$3 WHERE id=$4", flag, version, v.now(), id); err != nil {
			return err
		}
		if err := v.notifyGrantees(ctx, tx, id); err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "pass_permission", nil)
	})
}

func (v *Vault) SavePass(ctx context.Context, id, number string, expected *int64) (map[string]any, error) {
	return v.savePass(ctx, id, number, expected, nil)
}

// SaveIssuedPass rejects an enrollment result if login/logout or another pass
// changed the account while the university request was in flight.
func (v *Vault) SaveIssuedPass(ctx context.Context, id, number string, expected, sessionExpected int64) (map[string]any, error) {
	return v.savePass(ctx, id, number, &expected, &sessionExpected)
}

func (v *Vault) savePass(ctx context.Context, id, number string, expected, sessionExpected *int64) (map[string]any, error) {
	if number == "" || number[0] < '1' || number[0] > '9' {
		return nil, errors.New("Invalid pass identifier")
	}
	for _, digit := range number {
		if digit < '0' || digit > '9' {
			return nil, errors.New("Invalid pass identifier")
		}
	}
	value, err := strconv.ParseInt(number, 10, 64)
	if err != nil || value <= 0 {
		return nil, errors.New("Invalid pass identifier")
	}
	return v.mutate(ctx, id, func(tx pgx.Tx, row map[string]any) error {
		if sessionExpected != nil && row["pass_allowed"] == int64(0) {
			return &Conflict{"The pass was disabled during enrollment"}
		}
		if sessionExpected != nil && *sessionExpected != row["session_version"] {
			return &Conflict{"The session changed; retry pass enrollment"}
		}
		if expected != nil && *expected != row["pass_version"] {
			return &Conflict{"A newer pass is already stored"}
		}
		secret, err := v.Secret(row)
		if err != nil {
			return err
		}
		if secret["pass_number"] == number {
			return nil
		}
		secret["pass_number"] = number
		sealed, err := v.seal(secret)
		if err != nil {
			return err
		}
		version := row["version"].(int64) + 1
		if _, err = tx.Exec(ctx, "UPDATE aa.accounts SET secret=$1,version=$2,pass_version=pass_version+1,updated_at=$3 WHERE id=$4", sealed, version, v.now(), id); err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "pass", nil)
	})
}

func (v *Vault) EndSession(ctx context.Context, id string) (map[string]any, error) {
	return v.mutate(ctx, id, func(tx pgx.Tx, row map[string]any) error {
		secret, err := v.Secret(row)
		if err != nil {
			return err
		}
		finishRenewal(secret, "interrupted", v.now(), nil)
		for _, key := range []string{"cookie", "storage_state", "expires_at", "renewed_at", "renewal_status", "renewal_attempted_at"} {
			delete(secret, key)
		}
		sealed, err := v.seal(secret)
		if err != nil {
			return err
		}
		version := row["version"].(int64) + 1
		if _, err = tx.Exec(ctx, `UPDATE aa.accounts SET secret=$1,state='expired',expires_at=NULL,version=$2,
   session_version=session_version+1,updated_at=$3 WHERE id=$4`, sealed, version, v.now(), id); err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "session", nil)
	})
}

func (v *Vault) UpdateGroup(ctx context.Context, id string, groupID *int64, groupName *string, expected int64) (map[string]any, error) {
	return v.mutate(ctx, id, func(tx pgx.Tx, row map[string]any) error {
		if row["version"] != expected {
			return &Conflict{"Account changed; refresh before assigning a group"}
		}
		var name any
		if groupID != nil && groupName != nil && *groupName != "" {
			name = strings.TrimSpace(*groupName)
		}
		var gid any
		if groupID != nil {
			gid = *groupID
		}
		if row["group_id"] == gid && row["group_name"] == name {
			return nil
		}
		version := row["version"].(int64) + 1
		if _, err := tx.Exec(ctx, "UPDATE aa.accounts SET group_id=$1,group_name=$2,version=$3,updated_at=$4 WHERE id=$5", gid, name, version, v.now(), id); err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "group", row["group_id"])
	})
}

func (v *Vault) UpdateState(ctx context.Context, id, state string, expected int64) error {
	return v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT * FROM aa.accounts WHERE id=$1 FOR UPDATE", id)
		if err != nil {
			return err
		}
		if row == nil || row["version"] != expected {
			return nil
		}
		if row["state"] == state {
			_, err = tx.Exec(ctx, "UPDATE aa.accounts SET checked_at=$1 WHERE id=$2", v.now(), id)
			return err
		}
		version := row["version"].(int64) + 1
		if _, err = tx.Exec(ctx, "UPDATE aa.accounts SET state=$1,checked_at=$2,version=$3,updated_at=$2 WHERE id=$4", state, v.now(), version, id); err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "state", nil)
	})
}

func (s *Store) Cursor(ctx context.Context) (int64, error) {
	var cursor int64
	err := s.Pool.QueryRow(ctx, "SELECT COALESCE(MAX(id),0) FROM aa.events").Scan(&cursor)
	return cursor, err
}
func (s *Store) Groups(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, `SELECT group_id AS id,MAX(group_name COLLATE "C") AS name,COUNT(*) AS accounts
  FROM aa.accounts WHERE group_id IS NOT NULL GROUP BY group_id ORDER BY MAX(group_name COLLATE "C") NULLS FIRST`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}
