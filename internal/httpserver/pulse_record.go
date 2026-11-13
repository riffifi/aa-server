package httpserver

import (
	"aa-server/internal/browser"
	"context"
	"errors"
	"net/http"
)

func (s *Server) pulseRecord(w http.ResponseWriter, r *http.Request) {
	body, ok := DecodeBody(w, r, []Field{{Name: "account_id", Kind: "string", Required: true, Min: 1, Max: 64}})
	if !ok {
		return
	}
	if _, ok = s.panel(w, r, true); !ok {
		return
	}
	id := body["account_id"].(string)
	row, err := s.config.Vault.Account(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	if row == nil {
		detail(w, 404, "Account not found")
		return
	}
	secret, err := s.config.Vault.Secret(row)
	if err := s.config.Vault.RequireUnblocked(r.Context(), row["pulse_id"].(string)); err != nil {
		s.failure(w, err)
		return
	}
	if err != nil {
		s.failure(w, err)
		return
	}
	state, _ := secret["storage_state"].(map[string]any)
	recorder, ok := s.config.Browser.(interface {
		Record(context.Context, map[string]any) (map[string]any, error)
	})
	if !ok {
		detail(w, 503, "Browser diagnostics unavailable")
		return
	}
	report, err := recorder.Record(r.Context(), state)
	if err != nil {
		var public *browser.UserError
		switch {
		case errors.As(err, &public):
			detail(w, 409, public.Message)
		case errors.Is(err, context.DeadlineExceeded):
			detail(w, 504, "Pulse did not answer in time")
		default:
			detail(w, 503, "Pulse is unavailable")
		}
		return
	}
	if err = s.config.Vault.Audit(r.Context(), "pulse_recorded", "panel", &id); err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, report)
}
