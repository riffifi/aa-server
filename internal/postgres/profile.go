package postgres

import (
	"context"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"aa-server/internal/pulse"
	"github.com/jackc/pgx/v5"
)

func MissingLabel(label string) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "", "без имени", "account":
		return true
	}
	return false
}

func validProfileName(name string) bool {
	if strings.TrimSpace(name) != name || name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 60 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// FillProfileName never replaces a chosen label, even if it changed after the
// upstream request started. Session version prevents a stale login's write.
func (v *Vault) FillProfileName(ctx context.Context, id, name string, sessionVersion int64) (map[string]any, error) {
	if !validProfileName(name) {
		return nil, nil
	}
	var updated map[string]any
	err := v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT * FROM aa.accounts WHERE id=$1 FOR UPDATE", id)
		if err != nil || row == nil {
			return err
		}
		if row["session_version"] != sessionVersion || !MissingLabel(row["label"].(string)) {
			return nil
		}
		version := row["version"].(int64) + 1
		updated, err = one(ctx, tx, "UPDATE aa.accounts SET label=$1,version=$2,updated_at=$3 WHERE id=$4 RETURNING *", name, version, v.now(), id)
		if err != nil {
			return err
		}
		return v.event(ctx, tx, id, version, "label", nil)
	})
	return updated, err
}

// SavePulseCookies atomically merges an upstream response into the encrypted
// credentials. Both the session revision and sent header must still match;
// logout, fresh login and out-of-order responses cannot resurrect old tickets.
func (v *Vault) SavePulseCookies(ctx context.Context, id, sent string, sessionVersion int64, headers http.Header) (map[string]any, error) {
	var uid string
	if err := v.Pool.QueryRow(ctx, "SELECT pulse_id FROM aa.accounts WHERE id=$1", id).Scan(&uid); err != nil {
		return nil, err
	}
	if err := v.RequireUnblocked(ctx, uid); err != nil {
		return nil, err
	}
	if len(headers.Values("Set-Cookie")) == 0 {
		return nil, nil
	}
	var updated map[string]any
	err := v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT * FROM aa.accounts WHERE id=$1 FOR UPDATE", id)
		if err != nil || row == nil {
			return err
		}
		if row["session_version"] != sessionVersion {
			return nil
		}
		secret, err := v.Secret(row)
		if err != nil {
			return err
		}
		if secret["cookie"] != sent || sent == "" {
			return nil
		}
		patch, auth, err := pulse.MergeSessionCookies(secret, headers, v.Now())
		if err != nil {
			return err
		}
		if patch == nil {
			return nil
		}
		if secret["sso_renewed_at"] == nil {
			secret["sso_renewed_at"] = secret["renewed_at"]
		}
		for key, value := range patch {
			secret[key] = value
		}
		if auth && strings.Contains(patch["cookie"].(string), ".AspNetCore.Cookies=") {
			finishRenewal(secret, "interrupted", v.now(), nil)
			secret["renewed_at"] = v.now()
			if patch["cookie"] != "" {
				delete(secret, "renewal_status")
				delete(secret, "renewal_attempted_at")
			}
			stats := renewalStatistics(secret)
			if stats.Since == 0 {
				stats.Since = v.now()
			}
			stats.CookieRefreshes++
			stats.LastCookieRefresh = v.now()
			secret["renewal_statistics"] = stats
		}
		sealed, err := v.seal(secret)
		if err != nil {
			return err
		}
		// Any cookie change is a credential revision, including challenge cookies.
		var expiry any
		if deadline := renewalExpiry(patch["expires_at"]); deadline != nil {
			expiry = *deadline
		}
		updated, err = one(ctx, tx, `UPDATE aa.accounts SET secret=$1,expires_at=$2,version=version+1,session_version=session_version+1,updated_at=$3 WHERE id=$4 RETURNING *`, sealed, expiry, v.now(), id)
		if err != nil {
			return err
		}
		return v.event(ctx, tx, id, updated["version"].(int64), "session", nil)
	})
	return updated, err
}
