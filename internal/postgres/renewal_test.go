package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenewalStatisticsPersistRealOutcomesAndExcludeManualLogins(t *testing.T) {
	v := testVault(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	v.Now = func() time.Time { return now }
	uid := fmt.Sprintf("renewal-stats-%d", time.Now().UnixNano())
	oldExpiry := float64(now.Add(-time.Minute).Unix())
	row, err := v.Upsert(ctx, Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "do-not-expose", "expires_at": oldExpiry, "renewed_at": float64(now.Unix()), "renewal_statistics": map[string]any{"attempts": 999}}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	t.Cleanup(func() { _ = v.DeleteAccount(ctx, id) })
	secret, _ := v.Secret(row)
	if view := RenewalView(secret); view["last_success"] != nil || view["statistics"].(RenewalStatistics).Attempts != 0 {
		t.Fatal("manual login or client field counted as renewal", view)
	}
	version := row["session_version"].(int64)
	if err := v.NoteRenewal(ctx, id, version, "running", "expired"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	newExpiry := float64(now.Add(time.Hour).Unix())
	row, err = v.Upsert(ctx, Upsert{PulseID: uid, AccountID: id, SessionExpected: &version, AutomaticRenewal: true, Credentials: map[string]any{"cookie": "new-private-cookie", "expires_at": newExpiry}})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ = v.Secret(row)
	stats := renewalStatistics(secret)
	if stats.History[0].FlowVersion != RenewalFlowVersion || RenewalView(secret)["flow_version"] != RenewalFlowVersion {
		t.Fatal("renewal flow provenance missing")
	}
	if stats.Attempts != 1 || stats.Succeeded != 1 || stats.Recovered != 1 || stats.ConfirmedBenefit != 1 || stats.ExpiryExtended != 1 || stats.Active != nil || len(stats.History) != 1 || stats.History[0].DurationMS != 2000 {
		t.Fatal("outcome counters differ", stats)
	}
	if *stats.History[0].BeforeExpiry != oldExpiry || *stats.History[0].AfterExpiry != newExpiry {
		t.Fatal("deadline evidence missing")
	}
	lastSuccess := stats.LastSuccess
	// A fresh QR login must retain counters but never become another auto success.
	row, err = v.Upsert(ctx, Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "fresh-qr", "renewed_at": float64(now.Add(time.Hour).Unix())}})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ = v.Secret(row)
	view := RenewalView(secret)
	if view["last_success"] != lastSuccess || view["statistics"].(RenewalStatistics).Succeeded != 1 {
		t.Fatal("QR login faked an auto success", view)
	}
	version = row["session_version"].(int64)
	for _, status := range []string{"failed", "requires_login"} {
		if err := v.NoteRenewal(ctx, id, version, "running", "valid"); err != nil {
			t.Fatal(err)
		}
		if err := v.NoteRenewal(ctx, id, version, status); err != nil {
			t.Fatal(err)
		}
	}
	// The same completed callback cannot count the failure twice.
	if err := v.NoteRenewal(ctx, id, version, "requires_login"); err != nil {
		t.Fatal(err)
	}
	row, _ = v.Account(ctx, id)
	secret, _ = v.Secret(row)
	stats = renewalStatistics(secret)
	if stats.Attempts != 3 || stats.Failed != 1 || stats.RequiresLogin != 1 || stats.Succeeded != 1 {
		t.Fatal("duplicate/incorrect terminal counters", stats)
	}
	// The encrypted record can be read by a new Vault instance after restart.
	reopened := &Vault{Store: v.Store, Cipher: v.Cipher, Now: v.Now}
	saved, err := reopened.Account(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	savedSecret, err := reopened.Secret(saved)
	if err != nil || renewalStatistics(savedSecret).Attempts != 3 {
		t.Fatal("counters lost after reopening", err)
	}
	if err := v.NoteRenewal(ctx, id, version, "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Upsert(ctx, Upsert{PulseID: uid, Credentials: map[string]any{"cookie": "replacement"}}); err != nil {
		t.Fatal(err)
	}
	if err := v.NoteRenewal(ctx, id, version, "failed"); err != nil {
		t.Fatal(err)
	}
	saved, err = v.Account(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	savedSecret, _ = v.Secret(saved)
	if s := renewalStatistics(savedSecret); s.Attempts != 4 || s.Interrupted != 1 || s.Failed != 1 || s.Succeeded != 1 {
		t.Fatal("stale attempt changed new statistics", s)
	}
	body, _ := json.Marshal(RenewalView(secret))
	if strings.Contains(string(body), "private-cookie") || strings.Contains(string(body), "do-not-expose") {
		t.Fatal("credential leaked in diagnostics")
	}
}

func TestRenewalEvidenceUnknownUnchangedAndBoundedHistory(t *testing.T) {
	for _, test := range []struct {
		before, after                any
		extended, unchanged, unknown int64
	}{{nil, 1000, 0, 0, 1}, {1000, nil, 0, 0, 1}, {1000, 1000, 0, 1, 0}, {1000, 900, 0, 1, 0}, {1000, 2000, 1, 0, 0}} {
		secret := map[string]any{"renewal_statistics": RenewalStatistics{Attempts: 1, Active: &RenewalAttempt{Started: 100, BeforeExpiry: renewalExpiry(test.before)}}}
		finishRenewal(secret, "succeeded", 102, test.after)
		finishRenewal(secret, "succeeded", 103, test.after)
		s := renewalStatistics(secret)
		if s.Succeeded != 1 || s.ConfirmedBenefit != test.extended || s.ExpiryExtended != test.extended || s.ExpiryUnchanged != test.unchanged || s.ExpiryUnknown != test.unknown {
			t.Fatal("unproven extension counted", s)
		}
	}
	secret := map[string]any{}
	for index := 0; index < 50; index++ {
		s := renewalStatistics(secret)
		s.Attempts++
		s.Active = &RenewalAttempt{Started: float64(index + 1)}
		secret["renewal_statistics"] = s
		finishRenewal(secret, "failed", float64(index+2), nil)
	}
	s := renewalStatistics(secret)
	if s.Failed != 50 || len(s.History) != 20 || s.History[0].Started != 31 {
		t.Fatal("unbounded/truncated counters", s)
	}
}
