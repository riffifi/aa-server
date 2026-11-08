package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"math"
)

type RenewalAttempt struct {
	Started           float64  `json:"started"`
	Finished          float64  `json:"finished"`
	Result            string   `json:"result"`
	DurationMS        float64  `json:"duration_ms"`
	BeforeExpiry      *float64 `json:"before_expiry"`
	AfterExpiry       *float64 `json:"after_expiry"`
	ExpiryDelta       *float64 `json:"expiry_delta_seconds"`
	PreviouslyExpired bool     `json:"previously_expired"`
}

type RenewalStatistics struct {
	Since            float64          `json:"since"`
	Attempts         int64            `json:"attempts"`
	Succeeded        int64            `json:"succeeded"`
	Failed           int64            `json:"failed"`
	RequiresLogin    int64            `json:"requires_login"`
	Interrupted      int64            `json:"interrupted"`
	Recovered        int64            `json:"recovered"`
	ConfirmedBenefit int64            `json:"confirmed_benefit"`
	ExpiryExtended   int64            `json:"expiry_extended"`
	ExpiryUnchanged  int64            `json:"expiry_unchanged"`
	ExpiryUnknown    int64            `json:"expiry_unknown"`
	LastAttempt      float64          `json:"last_attempt"`
	LastSuccess      float64          `json:"last_success"`
	Active           *RenewalAttempt  `json:"active"`
	History          []RenewalAttempt `json:"history"`
}

func renewalStatistics(secret map[string]any) RenewalStatistics {
	var stats RenewalStatistics
	if body, err := json.Marshal(secret["renewal_statistics"]); err == nil {
		_ = json.Unmarshal(body, &stats)
	}
	if len(stats.History) > 20 {
		stats.History = stats.History[len(stats.History)-20:]
	}
	if stats.History == nil {
		stats.History = []RenewalAttempt{}
	}
	return stats
}

func renewalExpiry(value any) *float64 {
	var expiry float64
	switch v := value.(type) {
	case float64:
		expiry = v
	case int64:
		expiry = float64(v)
	case int:
		expiry = float64(v)
	case json.Number:
		expiry, _ = v.Float64()
	}
	if expiry <= 0 || math.IsNaN(expiry) || math.IsInf(expiry, 0) {
		return nil
	}
	return &expiry
}

func finishRenewal(secret map[string]any, result string, now float64, expiry any) {
	stats := renewalStatistics(secret)
	if stats.Active == nil {
		return
	}
	entry := *stats.Active
	entry.Finished, entry.Result, entry.DurationMS = now, result, max(0, (now-entry.Started)*1000)
	if result == "succeeded" {
		benefit := entry.PreviouslyExpired
		stats.Succeeded++
		stats.LastSuccess = now
		entry.AfterExpiry = renewalExpiry(expiry)
		if entry.PreviouslyExpired {
			stats.Recovered++
		}
		if entry.BeforeExpiry != nil && entry.AfterExpiry != nil {
			delta := *entry.AfterExpiry - *entry.BeforeExpiry
			entry.ExpiryDelta = &delta
			if delta > 0 {
				stats.ExpiryExtended++
				benefit = true
			} else {
				stats.ExpiryUnchanged++
			}
		} else {
			stats.ExpiryUnknown++
		}
		if benefit {
			stats.ConfirmedBenefit++
		}
	} else if result == "requires_login" {
		stats.RequiresLogin++
	} else if result == "interrupted" {
		stats.Interrupted++
	} else {
		stats.Failed++
	}
	stats.History = append(stats.History, entry)
	if len(stats.History) > 20 {
		stats.History = stats.History[len(stats.History)-20:]
	}
	stats.Active = nil
	secret["renewal_statistics"] = stats
}

// Only explicit typed diagnostics leave the encrypted secret, in the private
// panel. A manual QR login is never counted as a successful automatic renewal.
func RenewalView(secret map[string]any) map[string]any {
	stats := renewalStatistics(secret)
	var attempt, success any
	if stats.LastAttempt > 0 {
		attempt = stats.LastAttempt
	} else {
		attempt = secret["renewal_attempted_at"]
	}
	if stats.LastSuccess > 0 {
		success = stats.LastSuccess
	}
	return map[string]any{"status": secret["renewal_status"], "last_attempt": attempt, "last_success": success, "statistics": stats}
}

// Renewal status stays inside the encrypted secret. A stale browser job must
// never pause a freshly replaced session or restore an explicitly ended one.
func (v *Vault) NoteRenewal(ctx context.Context, id string, version int64, status string, observedState ...string) error {
	switch status {
	case "running", "failed", "requires_login":
	default:
		return errors.New("invalid renewal status")
	}
	return v.Write(ctx, func(tx pgx.Tx) error {
		row, err := one(ctx, tx, "SELECT secret,session_version,expires_at FROM aa.accounts WHERE id=$1 FOR UPDATE", id)
		if err != nil || row == nil {
			return err
		}
		if row["session_version"] != version {
			return nil
		}
		secret, err := v.Secret(row)
		if err != nil {
			return err
		}
		now := v.now()
		if status == "running" {
			finishRenewal(secret, "interrupted", now, nil)
			stats := renewalStatistics(secret)
			if stats.Since == 0 {
				stats.Since = now
			}
			stats.Attempts++
			stats.LastAttempt = now
			stats.Active = &RenewalAttempt{Started: now, Result: "running", BeforeExpiry: renewalExpiry(row["expires_at"]), PreviouslyExpired: len(observedState) > 0 && observedState[0] == "expired"}
			secret["renewal_statistics"] = stats
			secret["renewal_attempted_at"] = now
		} else {
			finishRenewal(secret, status, now, nil)
		}
		secret["renewal_status"] = status
		encoded, err := v.seal(secret)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE aa.accounts SET secret=$1 WHERE id=$2", encoded, id)
		return err
	})
}
