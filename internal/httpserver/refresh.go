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
	secret, err := v.Secret(row)
	if err != nil {
		return nil, err
	}
	cookie, _ := secret["cookie"].(string)
	state := "expired"
	if cookie != "" {
		var uid string
		state, uid = s.config.Pulse.Identity(ctx, cookie)
		if state == "valid" && uid != row["pulse_id"] {
			state = "forbidden"
		}
	}
	now := v.Now()
	due := cookie != "" && (state == "expired" || (state == "valid" && ((row["expires_at"] != nil && numberFloat(row["expires_at"]) <= float64(now.Unix())+s.config.RenewBefore.Seconds()) || float64(now.Unix())-numberFloat(secret["renewed_at"]) >= s.config.RenewInterval.Seconds())))
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
			result, renewErr := renewer.Renew(ctx, storage)
			if renewErr == nil && result != nil && result.UID == row["pulse_id"] && len(result.Credentials) > 0 {
				expected := row["session_version"].(int64)
				fresh, writeErr := v.Upsert(ctx, postgres.Upsert{PulseID: result.UID, AccountID: id, Credentials: result.Credentials, SessionExpected: &expected})
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
		reply, callErr := s.config.Pulse.RPC(ctx, cookie, "schedules", pulse.Frame(nil, 0), "")
		if callErr == nil && reply.Status == 200 {
			group, name := pulse.DecodeGroup(reply.Headers, reply.Body)
			if group != nil {
				_, _ = v.UpdateGroup(ctx, id, group, name, fresh["version"].(int64))
			}
		}
	}
	return v.Account(ctx, id)
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
