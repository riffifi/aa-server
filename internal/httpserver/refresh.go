package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
)

type refreshFlight struct {
	done chan struct{}
	row  map[string]any
	err  error
}

func (s *Server) refresh(ctx context.Context, id string) (map[string]any, error) {
	s.refreshMu.Lock()
	if s.refreshRoot.Err() != nil {
		s.refreshMu.Unlock()
		return nil, s.refreshRoot.Err()
	}
	flight := s.refreshing[id]
	if flight == nil {
		flight = &refreshFlight{done: make(chan struct{})}
		s.refreshing[id] = flight
		s.refreshWG.Add(1)
		go func() {
			defer s.refreshWG.Done()
			work, cancel := context.WithTimeout(s.refreshRoot, 90*time.Second)
			defer cancel()
			row, err := s.refreshOne(work, id)
			s.refreshMu.Lock()
			flight.row, flight.err = row, err
			delete(s.refreshing, id)
			close(flight.done)
			s.refreshMu.Unlock()
		}()
	}
	s.refreshMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return flight.row, flight.err
	}
}
func numberFloat(value any) float64 {
	switch value := value.(type) {
	case float64:
		return value
	case json.Number:
		out, _ := value.Float64()
		return out
	case int64:
		return float64(value)
	}
	return 0
}
func (s *Server) refreshOne(ctx context.Context, id string) (map[string]any, error) {
	v := s.config.Vault
	row, err := v.Account(ctx, id)
	if err != nil || row == nil {
		return row, err
	}
	if err := v.RequireUnblocked(ctx, row["pulse_id"].(string)); err != nil {
		return row, err
	}
	secret, err := v.Secret(row)
	if err != nil {
		return nil, err
	}
	cookie, _ := secret["cookie"].(string)
	state := "expired"
	if cookie != "" {
		var uid, name string
		state, uid, name, _, err = s.probeProfile(ctx, row, cookie)
		if err != nil {
			return nil, err
		}
		if state == "valid" && uid != row["pulse_id"] {
			state = "forbidden"
		}
		if state == "valid" && name != "" && postgres.MissingLabel(row["label"].(string)) {
			var named map[string]any
			if named, err = v.FillProfileName(ctx, id, name, row["session_version"].(int64)); err != nil {
				return nil, err
			}
			if named != nil {
				row = named
			}
		}
		secret, err = v.Secret(row)
		if err != nil {
			return nil, err
		}
		cookie, _ = secret["cookie"].(string)
	}
	now := v.Now()
	ssoRenewed := secret["sso_renewed_at"]
	if ssoRenewed == nil {
		ssoRenewed = secret["renewed_at"]
	}
	// A successful profile is not proof that the SSO session or its refresh
	// credentials were renewed. Cookie rotation must not postpone this timer.
	paused := secret["renewal_status"] == "requires_login" && secret["renewal_flow_version"] == postgres.RenewalFlowVersion
	due := cookie != "" && !paused && renewalDue(state, now, row["expires_at"], ssoRenewed, s.config.RenewBefore, s.config.RenewInterval)
	storage, _ := secret["storage_state"].(map[string]any)
	if renewer, ok := s.config.Browser.(interface {
		Renew(context.Context, map[string]any) (*browser.Result, error)
	}); ok && due && len(storage) > 0 {
		s.refreshMu.Lock()
		last := s.renewed[id]
		retry := last.IsZero() || now.Sub(last) >= s.config.RenewBackoff
		if retry {
			s.renewed[id] = now
		}
		s.refreshMu.Unlock()
		if retry {
			if err := v.NoteRenewal(ctx, id, row["session_version"].(int64), "running", state); err != nil {
				return nil, err
			}
			result, renewErr := s.guardedRenew(ctx, row["pulse_id"].(string), renewer, storage)
			if blockedErr := v.RequireUnblocked(ctx, row["pulse_id"].(string)); blockedErr != nil {
				return nil, blockedErr
			}
			if renewErr == nil && result != nil && result.UID == row["pulse_id"] && len(result.Credentials) > 0 {
				expected := row["session_version"].(int64)
				fresh, writeErr := v.Upsert(ctx, postgres.Upsert{PulseID: result.UID, ProfileName: result.ProfileName, AccountID: id, Credentials: result.Credentials, SessionExpected: &expected, AutomaticRenewal: true})
				result.Credentials = nil
				if writeErr == nil {
					row = fresh
					state = "valid"
				} else {
					var conflict *postgres.Conflict
					if !errors.As(writeErr, &conflict) {
						return nil, writeErr
					}
				}
			} else {
				status := "failed"
				if errors.Is(renewErr, browser.ErrReauthenticationRequired) {
					status = "requires_login"
				}
				if err := v.NoteRenewal(ctx, id, row["session_version"].(int64), status); err != nil {
					return nil, err
				}
			}
			if result != nil {
				result.Credentials = nil
			}
		}
	}
	if err = v.UpdateState(ctx, id, state, row["version"].(int64)); err != nil {
		return nil, err
	}
	fresh, err := v.Account(ctx, id)
	if err != nil || fresh == nil {
		return fresh, err
	}
	if updated, readErr := v.Secret(fresh); readErr == nil {
		cookie, _ = updated["cookie"].(string)
	}
	if fresh["group_id"] == nil && state == "valid" && cookie != "" {
		reply, callErr := s.pulseRPC(ctx, fresh, "schedules", pulse.Frame(nil, 0), "")
		if callErr == nil && reply.Status == 200 {
			group, name := pulse.DecodeGroup(reply.Headers, reply.Body)
			if group != nil {
				_, _ = v.UpdateGroup(ctx, id, group, name, fresh["version"].(int64))
			}
		}
	}
	return v.Account(ctx, id)
}

// Cancel active browser renewal after a ban in any server instance. Requests
// already accepted by the provider cannot be recalled, but no result is stored.
func (s *Server) guardedRenew(ctx context.Context, uid string, runner interface {
	Renew(context.Context, map[string]any) (*browser.Result, error)
}, storage map[string]any) (*browser.Result, error) {
	if err := s.config.Vault.RequireUnblocked(ctx, uid); err != nil {
		return nil, err
	}
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				if err := s.config.Vault.RequireUnblocked(work, uid); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	result, err := runner.Renew(work, storage)
	cancel()
	<-done
	return result, err
}

func renewalDue(state string, now time.Time, expires, renewed any, before, interval time.Duration) bool {
	if state == "expired" {
		return true
	}
	if state != "valid" {
		return false
	}
	expiry, last := numberFloat(expires), numberFloat(renewed)
	heartbeat := float64(now.Unix())-last >= interval.Seconds()
	if expiry > 0 {
		margin := before.Seconds()
		// A 15m margin must not renew a fresh 10m cookie on every background scan.
		if lifetime := expiry - last; last > 0 && lifetime > 0 {
			margin = min(margin, lifetime/5)
		}
		// Keep the SSO heartbeat even for long-lived Pulse cookies: an upstream
		// SSO idle timeout can be shorter than the Pulse cookie lifetime.
		return heartbeat || expiry <= float64(now.Unix())+margin
	}
	// Session cookies have no visible deadline; keep the existing SSO heartbeat.
	return heartbeat
}
func (s *Server) refreshLoop() {
	defer s.refreshWG.Done()
	for {
		last := ""
		for {
			s.refreshMu.Lock()
			stopped := s.refreshRoot.Err() != nil
			s.refreshMu.Unlock()
			if stopped {
				return
			}
			rows, err := s.config.Vault.Pool.Query(s.refreshRoot, "SELECT id FROM aa.accounts WHERE id>$1 ORDER BY id LIMIT 100", last)
			if err != nil {
				break
			}
			ids := []string{}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			if len(ids) == 0 {
				break
			}
			jobs := make(chan string)
			var workers sync.WaitGroup
			for i := 0; i < 4; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for id := range jobs {
						_, _ = s.refresh(s.refreshRoot, id)
					}
				}()
			}
			for _, id := range ids {
				jobs <- id
			}
			close(jobs)
			workers.Wait()
			last = ids[len(ids)-1]
		}
		s.refreshMu.Lock()
		for id, at := range s.renewed {
			if s.config.Vault.Now().Sub(at) > time.Hour {
				delete(s.renewed, id)
			}
		}
		s.refreshMu.Unlock()
		timer := time.NewTimer(s.config.RefreshEvery)
		select {
		case <-s.refreshRoot.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (s *Server) refreshHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, true); !ok {
		return
	}
	row, err := s.refresh(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return
	}
	if row == nil {
		detail(w, 404, "Account not found")
		return
	}
	write(w, 200, postgres.Public(row))
}
