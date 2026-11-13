package httpserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
)

type profileRoundTrip func(*http.Request) (*http.Response, error)

func (f profileRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sessionModelBrowser struct {
	calls int
	renew func() (*browser.Result, error)
}

func (b *sessionModelBrowser) Login(context.Context, func(browser.Update)) (*browser.Result, error) {
	return nil, nil
}
func (b *sessionModelBrowser) Renew(context.Context, map[string]any) (*browser.Result, error) {
	b.calls++
	return b.renew()
}

func TestOfficialStyleSlidingCookiesKeepWorkingAcrossTwoDays(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	uid := fmt.Sprintf("webview-session-%d", now.UnixNano())
	current := ".AspNetCore.Cookies=ticket-0"
	validUntil := now.Add(time.Hour)
	calls := 0
	rejected := false
	upstream := pulse.NewTransport(profileRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != pulse.Methods["profile"] {
			return nil, fmt.Errorf("unexpected extra query: %s", r.URL.Path)
		}
		calls++
		request, _ := io.ReadAll(r.Body)
		if !bytes.Equal(request, pulse.ProfileRequest()) {
			return nil, fmt.Errorf("profile did not request rememberMe")
		}
		var reply pulse.Reply
		if rejected || r.Header.Get("Cookie") != current || !now.Before(validUntil) {
			reply = grpcReply(pulse.Field(2, nil))
		} else {
			user := append(pulse.Field(1, []byte(uid)), pulse.Field(2, []byte("Иван"))...)
			user = append(user, pulse.Field(3, []byte("Иванов"))...)
			reply = grpcReply(pulse.Field(1, pulse.Field(1, user)))
			current = ".AspNetCore.Cookies=ticket-" + strconv.Itoa(calls)
			validUntil = now.Add(time.Hour)
			reply.Headers.Add("Set-Cookie", current+"; Path=/; Secure; HttpOnly; Expires="+validUntil.UTC().Format(http.TimeFormat))
		}
		return &http.Response{StatusCode: 200, Header: reply.Headers, Body: io.NopCloser(bytes.NewReader(reply.Body))}, nil
	}))
	defer upstream.Close()
	b := &sessionModelBrowser{renew: func() (*browser.Result, error) {
		if rejected {
			return nil, browser.ErrReauthenticationRequired
		}
		return &browser.Result{UID: uid, Credentials: map[string]any{"cookie": current, "renewed_at": float64(now.Unix()), "expires_at": float64(validUntil.Unix()), "storage_state": map[string]any{"cookies": []any{}, "origins": []any{}}}}, nil
	}}
	s, v := setup(t, func(c *Config) { c.Pulse = upstream; c.Browser = b })
	v.Now = func() time.Time { return now }
	group := int64(5330)
	row, err := v.Upsert(ctx, postgres.Upsert{PulseID: uid, Label: "Без имени", GroupID: &group, Credentials: map[string]any{"cookie": current, "expires_at": float64(validUntil.Unix()), "storage_state": map[string]any{"cookies": []any{}, "origins": []any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	defer v.DeleteAccount(ctx, id)
	for i := 0; i < 64; i++ {
		now = now.Add(45 * time.Minute)
		row, err = s.refresh(ctx, id)
		if err != nil || row["state"] != "valid" {
			t.Fatalf("step %d: state=%v err=%v", i, row, err)
		}
		secret, err := v.Secret(row)
		if err != nil || secret["cookie"] != current {
			t.Fatalf("step %d lost sliding ticket: %v", i, err)
		}
	}
	if b.calls != 64 || calls != 64 || row["label"] != "Иванов Иван" {
		t.Fatal("extra browser/query or missing automatic name", b.calls, calls, row["label"])
	}
	secret, _ := v.Secret(row)
	stats := postgres.RenewalView(secret)["statistics"].(postgres.RenewalStatistics)
	if stats.CookieRefreshes != 64 || stats.Attempts != 64 {
		t.Fatal("cookie refresh counters", stats)
	}
	if _, err = v.Rename(ctx, id, "Моё имя", nil); err != nil {
		t.Fatal(err)
	}
	row, err = s.refresh(ctx, id)
	if err != nil || row["label"] != "Моё имя" {
		t.Fatal("manual name overwritten", err)
	}
	// A real authentication rejection still attempts SSO once, then asks for QR.
	now = now.Add(6 * time.Minute) // The last heartbeat's retry backoff has elapsed.
	rejected = true
	row, err = s.refresh(ctx, id)
	if err != nil || b.calls != 65 || row["state"] != "expired" {
		t.Fatal("upstream rejection ignored", err, b.calls)
	}
	if _, err = s.refresh(ctx, id); err != nil || b.calls != 65 {
		t.Fatal("rejected SSO retried indefinitely", err, b.calls)
	}
}

func TestStaleProfileCannotNameOrReviveAnEndedSession(t *testing.T) {
	uid := fmt.Sprintf("profile-logout-%d", time.Now().UnixNano())
	var end func()
	transport := pulse.NewTransport(profileRoundTrip(func(*http.Request) (*http.Response, error) {
		end()
		user := append(pulse.Field(1, []byte(uid)), pulse.Field(2, []byte("Stale"))...)
		reply := grpcReply(pulse.Field(1, pulse.Field(1, user)))
		reply.Headers.Add("Set-Cookie", ".AspNetCore.Cookies=late; Path=/; Max-Age=86400")
		return &http.Response{StatusCode: 200, Header: reply.Headers, Body: io.NopCloser(bytes.NewReader(reply.Body))}, nil
	}))
	defer transport.Close()
	s, v := setup(t, func(c *Config) { c.Pulse = transport })
	row, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: uid, Label: "Без имени", Credentials: map[string]any{"cookie": ".AspNetCore.Cookies=old"}})
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	defer v.DeleteAccount(context.Background(), id)
	end = func() {
		if _, err := v.EndSession(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	row, err = s.refresh(context.Background(), id)
	if err != nil || row["state"] != "expired" || row["label"] != "Без имени" {
		t.Fatal("stale response changed ended account", row, err)
	}
}

type rotatingPassPulse struct {
	enrollmentPulse
	sent []string
}

func (p *rotatingPassPulse) RPC(ctx context.Context, cookie, method string, body []byte, authorization string) (pulse.Reply, error) {
	p.sent = append(p.sent, cookie)
	reply, err := p.enrollmentPulse.RPC(ctx, "session=synthetic", method, body, authorization)
	reply.Headers.Add("Set-Cookie", fmt.Sprintf(".AspNetCore.Cookies=step%d; Path=/; Secure; HttpOnly; Max-Age=86400", len(p.sent)))
	return reply, err
}

func TestPassEnrollmentAdoptsItsOwnCookieRotation(t *testing.T) {
	uid := fmt.Sprintf("pass-cookie-%d", time.Now().UnixNano())
	p := &rotatingPassPulse{}
	s, v := setup(t, func(c *Config) {
		c.Pulse = p
		c.Verify = func(context.Context, string) (map[string]any, error) {
			return map[string]any{"mirea": uid, "sub": uid}, nil
		}
	})
	row := account(t, v, uid)
	if _, err := v.SetPassAllowed(context.Background(), row["id"].(string), true, nil); err != nil {
		t.Fatal(err)
	}
	w, _ := secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"012345"}`, map[string]string{"Authorization": "Bearer owner"})
	if w.Code != 200 || len(p.sent) != 2 || p.sent[1] != ".AspNetCore.Cookies=step1; session=synthetic" {
		t.Fatal("rotation broke multi-step enrollment", w.Code, p.sent)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("upstream cookie exposed to client")
	}
}
