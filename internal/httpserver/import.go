package httpserver

import (
	"net/http"
	"strings"

	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
)

func optionalInt(body map[string]any, key string) *int64 {
	value, ok := body[key].(int64)
	if !ok {
		return nil
	}
	return &value
}
func optionalString(body map[string]any, key string) *string {
	value, ok := body[key].(string)
	if !ok {
		return nil
	}
	return &value
}
func (s *Server) importAccount(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	cookie := body["cookie"].(string)
	if pulse.ValidateCookie(cookie) != nil {
		detail(w, 400, "Invalid cookie")
		return
	}
	state, uid := s.config.Pulse.Identity(r.Context(), cookie)
	if scoped(r) && state == "valid" && uid != identity(r) {
		detail(w, 403, "This session belongs to another MIREA account")
		return
	}
	if scoped(r) {
		if requested, ok := body["group_id"].(int64); ok {
			claims, _ := r.Context().Value(principalKey{}).(map[string]any)
			if requested != int64(numberAny(claims["grp"])) {
				detail(w, 403, "Group must match the verified MIREA identity")
				return
			}
		}
	}
	if state != "valid" {
		detail(w, 422, "Pulse did not confirm this session")
		return
	}
	v := s.config.Vault
	existing, err := v.MetadataByPulse(r.Context(), uid)
	if err != nil {
		s.failure(w, err)
		return
	}
	accountID, _ := body["account_id"].(string)
	if accountID != "" && (existing == nil || existing["id"] != accountID) {
		s.failure(w, &postgres.Conflict{Message: "This session belongs to a different Pulse account"})
		return
	}
	expected, sessionExpected := optionalInt(body, "expected_version"), optionalInt(body, "expected_session_version")
	groupID, groupName := optionalInt(body, "group_id"), optionalString(body, "group_name")
	if existing != nil && expected == nil && sessionExpected == nil && existing["state"] == "valid" {
		if existing["group_id"] == nil && groupID != nil {
			metadata, err := v.UpdateGroup(r.Context(), existing["id"].(string), groupID, groupName, existing["version"].(int64))
			if err != nil {
				s.failure(w, err)
				return
			}
			write(w, 200, metadata)
			return
		}
		write(w, 200, postgres.Public(existing))
		return
	}
	if expected == nil && sessionExpected == nil {
		var version int64
		if existing != nil {
			version = existing["version"].(int64)
		}
		expected = &version
	}
	if existing != nil && existing["group_id"] != nil {
		groupID, groupName = nil, nil
	}
	row, err := v.Upsert(r.Context(), postgres.Upsert{PulseID: uid, Label: body["label"].(string), Credentials: map[string]any{"cookie": cookie}, AccountID: accountID, Expected: expected, SessionExpected: sessionExpected, GroupID: groupID, GroupName: groupName})
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, postgres.Public(row))
}
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
	groupID, groupName := optionalInt(body, "group_id"), optionalString(body, "group_name")
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
