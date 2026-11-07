package httpserver

import (
	"net/http"
	"strings"
)

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if !s.allowOwner(w, r) {
		return
	}
	id := r.PathValue("id")
	v := s.config.Vault
	row, err := v.Account(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	if row == nil {
		detail(w, 404, "Account not found")
		return
	}
	secret, err := v.Secret(row)
	if err != nil {
		s.failure(w, err)
		return
	}
	if cookie, ok := secret["cookie"].(string); ok && cookie != "" {
		_ = s.config.Pulse.Logout(r.Context(), cookie)
	}
	ended, err := v.EndSession(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	actor := "owner"
	if strings.HasPrefix(r.URL.Path, "/panel/") {
		actor = "panel"
	}
	if err = v.Audit(r.Context(), "session_ended", actor, &id); err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, ended)
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, true); !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.config.Vault.DeleteAccount(r.Context(), id); err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"ok": true})
}
func (s *Server) assignGroup(w http.ResponseWriter, r *http.Request) {
	if scoped(r) {
		detail(w, 403, "Group assignment requires the panel")
		return
	}
	body, ok := input(w, r)
	if !ok {
		return
	}
	version, ok := expected(w, body)
	if !ok || version == nil {
		return
	}
	if strings.HasPrefix(r.URL.Path, "/panel/") {
		if _, ok = s.panel(w, r, true); !ok {
			return
		}
	}
	var gid *int64
	var name *string
	if value, ok := body["group_id"].(int64); ok {
		gid = &value
	}
	if value, ok := body["group_name"].(string); ok {
		name = &value
	}
	row, err := s.config.Vault.UpdateGroup(r.Context(), r.PathValue("id"), gid, name, *version)
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, row)
}
