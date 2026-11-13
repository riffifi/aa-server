package httpserver

import (
	"context"

	"aa-server/internal/pulse"
)

// row belongs to the calling request/worker. Updating it after our own cookie
// rotation lets a multi-step action keep its CAS guard without adopting a
// concurrent login or logout performed by somebody else.
func (s *Server) rememberPulseResponse(ctx context.Context, row map[string]any, sent string, reply pulse.Reply) error {
	if reply.Status < 200 || reply.Status >= 400 || len(reply.Headers.Values("Set-Cookie")) == 0 {
		return nil
	}
	fresh, err := s.config.Vault.SavePulseCookies(ctx, row["id"].(string), sent, row["session_version"].(int64), reply.Headers)
	if err != nil {
		return err
	}
	if fresh != nil {
		for key, value := range fresh {
			row[key] = value
		}
	}
	return nil
}

func (s *Server) pulseRPC(ctx context.Context, row map[string]any, method string, body []byte, token string) (pulse.Reply, error) {
	if err := s.config.Vault.RequireUnblocked(ctx, row["pulse_id"].(string)); err != nil {
		return pulse.Reply{}, err
	}
	if claims, ok := ctx.Value(principalKey{}).(map[string]any); ok {
		if uid, ok := claims["mirea"].(string); ok {
			if err := s.config.Vault.RequireUnblocked(ctx, uid); err != nil {
				return pulse.Reply{}, err
			}
		}
	}
	secret, err := s.config.Vault.Secret(row)
	if err != nil {
		return pulse.Reply{}, err
	}
	cookie, _ := secret["cookie"].(string)
	reply, err := s.config.Pulse.RPC(ctx, cookie, method, body, token)
	if err == nil && method == "profile" {
		state, uid := pulse.AuthenticationState(reply.Status, reply.Headers, reply.Body)
		if state != "valid" || uid != row["pulse_id"] {
			return reply, nil
		}
	}
	if err == nil {
		err = s.rememberPulseResponse(ctx, row, cookie, reply)
	}
	return reply, err
}

// A detailed profile response lets production persist Set-Cookie and read the
// verified name without a second request. Other Pulse adapters retain the
// original Identity contract, with browser fallback for renewal.
func (s *Server) probeProfile(ctx context.Context, row map[string]any, cookie string) (state, uid, name string, detailed bool, err error) {
	if transport, ok := s.config.Pulse.(interface {
		Profile(context.Context, string) (pulse.Reply, error)
	}); ok {
		detailed = true
		var reply pulse.Reply
		reply, err = transport.Profile(ctx, cookie)
		if err != nil {
			state = "unavailable"
			err = nil // Preserve the existing unknown-state behavior on a network outage.
			return
		}
		state, uid = pulse.AuthenticationState(reply.Status, reply.Headers, reply.Body)
		if state == "valid" && uid == row["pulse_id"] {
			name = pulse.ProfileName(reply.Body, reply.Headers, uid)
			err = s.rememberPulseResponse(ctx, row, cookie, reply)
		}
		return
	}
	state, uid = s.config.Pulse.Identity(ctx, cookie)
	return
}
