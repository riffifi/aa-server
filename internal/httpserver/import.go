package httpserver

import (
	"net/http"
	"strings"

	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
)

func (s *Server) linkIdentity(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	var claims map[string]any
	if s.config.Verifier != nil && strings.EqualFold(scheme, "bearer") {
		claims, _ = s.config.Verifier.Verify(r.Context(), token, "aa-link")
	}
	if claims == nil {
		detail(w, 401, "Invalid AA ID link")
		return
	}
	cookie := body["cookie"].(string)
	if pulse.ValidateCookie(cookie) != nil {
		detail(w, 400, "Invalid cookie")
		return
	}
	state, uid := s.config.Pulse.Identity(r.Context(), cookie)
	if state != "valid" || uid != claims["mirea"] {
		detail(w, 422, "Pulse did not confirm this session")
		return
	}
	v := s.config.Vault
	renewed, ok := body["renewed_at"].(float64)
	if !ok || renewed == 0 {
		renewed = float64(v.Now().UnixMicro()) / 1e6
	}
	credentials := map[string]any{"cookie": cookie, "expires_at": body["expires_at"], "renewed_at": renewed}
	if storage, ok := body["storage_state"].(map[string]any); ok && len(storage) > 0 {
		credentials["storage_state"] = storage
	}
	existing, err := v.MetadataByPulse(r.Context(), uid)
	if err != nil {
		s.failure(w, err)
		return
	}
	label := "Без имени"
	if existing != nil {
		label = ""
	}
	groupID, err := optionalGroup(claims["grp"])
	if err != nil {
		detail(w, 422, "Invalid verified group")
		return
	}
	var groupName *string
	if name, ok := claims["group_name"].(string); ok {
		groupName = &name
	}
	if existing != nil && existing["group_id"] != nil {
		groupID, groupName = nil, nil
	}
	row, err := v.Upsert(r.Context(), postgres.Upsert{PulseID: uid, Label: label, Credentials: credentials, GroupID: groupID, GroupName: groupName})
	if err != nil {
		s.failure(w, err)
		return
	}
	id := row["id"].(string)
	if raw, present := claims["usr"]; present {
		var username *string
		if value, ok := raw.(string); ok {
			username = &value
		}
		if err = v.SetUsername(r.Context(), id, username); err != nil {
			s.failure(w, err)
			return
		}
	}
	if err = v.Audit(r.Context(), "aaid_linked", "aaid:"+claims["sub"].(string), &id); err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"account_id": id})
}
