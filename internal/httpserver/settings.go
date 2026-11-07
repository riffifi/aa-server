package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aa-server/internal/postgres"
)

func (s *Server) ownAccount(w http.ResponseWriter, r *http.Request, required bool) (map[string]any, bool) {
	claims, _ := r.Context().Value(principalKey{}).(map[string]any)
	if claims == nil {
		detail(w, 401, "AA ID sign-in required")
		return nil, false
	}
	mirea, _ := claims["mirea"].(string)
	row, err := s.config.Vault.MetadataByPulse(r.Context(), mirea)
	if err != nil {
		s.failure(w, err)
		return nil, false
	}
	if row != nil {
		if username, present := claims["usr"]; present && row["username"] != username {
			var value *string
			if name, ok := username.(string); ok {
				value = &name
			}
			if err = s.config.Vault.SetUsername(r.Context(), row["id"].(string), value); err != nil {
				s.failure(w, err)
				return nil, false
			}
			row, err = s.config.Vault.MetadataAccount(r.Context(), row["id"].(string))
			if err != nil {
				s.failure(w, err)
				return nil, false
			}
		}
	}
	if required && row == nil {
		detail(w, 404, "This MIREA account is not connected to the session server")
		return nil, false
	}
	return row, true
}
func settings(row map[string]any) map[string]any {
	if row == nil {
		return map[string]any{"connected": false, "account": false, "label": nil, "pass_allowed": nil, "group_name": nil}
	}
	return map[string]any{"connected": row["state"] == "valid", "account": true, "label": row["label"], "pass_allowed": row["pass_allowed"] != int64(0), "group_name": row["group_name"]}
}
func card(row map[string]any) map[string]any {
	return map[string]any{"account_id": row["id"], "username": row["username"], "label": row["label"], "group_name": row["group_name"]}
}
func actor(r *http.Request) string {
	claims, _ := r.Context().Value(principalKey{}).(map[string]any)
	subject, _ := claims["sub"].(string)
	return "aaid:" + subject
}
func (s *Server) limited(w http.ResponseWriter, key string, count int, window time.Duration) bool {
	now := s.config.Vault.Now()
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	if _, exists := s.attempts[key]; !exists && len(s.attempts) >= 8192 {
		for candidate, values := range s.attempts {
			retention := s.attemptWindows[candidate]
			if retention < 10*time.Minute {
				retention = 10 * time.Minute
			}
			if len(values) == 0 || values[len(values)-1].Before(now.Add(-retention)) {
				delete(s.attempts, candidate)
				delete(s.attemptWindows, candidate)
			}
		}
		if len(s.attempts) >= 8192 {
			detail(w, 429, "Server is busy; retry later")
			return true
		}
	}
	queue := s.attempts[key]
	for len(queue) > 0 && queue[0].Before(now.Add(-window)) {
		queue = queue[1:]
	}
	if len(queue) >= count {
		s.attempts[key] = queue
		w.Header().Set("Retry-After", strconv.FormatInt(int64((window+time.Second-1)/time.Second), 10))
		detail(w, 429, "Слишком часто, подождите немного")
		return true
	}
	s.attemptWindows[key] = window
	s.attempts[key] = append(queue, now)
	return false
}
func (s *Server) passList(w http.ResponseWriter, r *http.Request, own map[string]any) {
	scope, permitted := s.rosterScope(w, r)
	if !permitted {
		return
	}
	v := s.config.Vault
	id := own["id"].(string)
	people := []any{}
	requests := []any{}
	shares, err := v.PassShares(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	for _, row := range shares {
		if !scope.allows(row) {
			continue
		}
		item := card(row)
		item["kind"] = "aaid"
		item["id"] = row["id"]
		people = append(people, item)
	}
	pending, err := v.ShareRequests(r.Context(), id, true)
	if err != nil {
		s.failure(w, err)
		return
	}
	for _, row := range pending {
		if !scope.allows(row) {
			continue
		}
		item := card(row)
		item["id"] = row["request_id"]
		item["requested_at"] = row["requested_at"]
		requests = append(requests, item)
	}
	write(w, 200, map[string]any{"pass_allowed": own["pass_allowed"] != int64(0), "people": people, "requests": requests})
}
func (s *Server) myPasses(w http.ResponseWriter, r *http.Request, own map[string]any) {
	scope, permitted := s.rosterScope(w, r)
	if !permitted {
		return
	}
	id := own["id"].(string)
	shares, err := s.config.Vault.SharedMetadata(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	passes := []any{}
	requests := []any{}
	for _, row := range shares {
		if !scope.allows(row) {
			continue
		}
		item := card(row)
		item["pass_allowed"] = row["pass_allowed"] != int64(0)
		passes = append(passes, item)
	}
	pending, err := s.config.Vault.ShareRequests(r.Context(), id, false)
	if err != nil {
		s.failure(w, err)
		return
	}
	for _, row := range pending {
		if !scope.allows(row) {
			continue
		}
		item := card(row)
		item["id"] = row["request_id"]
		item["requested_at"] = row["requested_at"]
		requests = append(requests, item)
	}
	write(w, 200, map[string]any{"passes": passes, "requests": requests})
}
func (s *Server) myAccount(w http.ResponseWriter, r *http.Request) {
	row, ok := s.ownAccount(w, r, false)
	if !ok {
		return
	}
	write(w, 200, settings(row))
}
func (s *Server) changeMyAccount(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	own, ok := s.ownAccount(w, r, true)
	if !ok {
		return
	}
	id := own["id"].(string)
	v := s.config.Vault
	if label, ok := body["label"].(string); ok {
		if _, err := v.Rename(r.Context(), id, label, nil); err != nil {
			if strings.TrimSpace(label) == "" {
				detail(w, 400, "Invalid name")
			} else {
				s.failure(w, err)
			}
			return
		}
	}
	if allowed, ok := body["pass_allowed"].(bool); ok {
		if _, err := v.SetPassAllowed(r.Context(), id, allowed, nil); err != nil {
			s.failure(w, err)
			return
		}
	}
	row, err := v.MetadataAccount(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, settings(row))
}
func (s *Server) people(w http.ResponseWriter, r *http.Request) {
	scope, permitted := s.rosterScope(w, r)
	if !permitted {
		return
	}
	own, ok := s.ownAccount(w, r, scoped(r))
	if !ok {
		return
	}
	if s.limited(w, "site:people:"+actor(r), 60, time.Minute) {
		return
	}
	var exclude string
	if own != nil {
		exclude = own["id"].(string)
	}
	query := []rune(r.URL.Query().Get("q"))
	if len(query) > 60 {
		query = query[:60]
	}
	var rows []map[string]any
	var err error
	if scoped(r) {
		if s.limited(w, "exact-people:"+identity(r), 20, time.Minute) {
			return
		}
		rows, err = s.config.Vault.ExactPerson(r.Context(), string(query), exclude)
	} else {
		rows, err = s.config.Vault.SearchPeople(r.Context(), string(query), exclude, 10)
	}
	if err != nil {
		s.failure(w, err)
		return
	}
	people := []any{}
	for _, row := range rows {
		if !scope.allows(row) {
			continue
		}
		people = append(people, card(row))
	}
	write(w, 200, map[string]any{"people": people})
}
func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	own, ok := s.ownAccount(w, r, true)
	if !ok {
		return
	}
	v := s.config.Vault
	id := own["id"].(string)
	grantee, err := v.MetadataAccount(r.Context(), body["account_id"].(string))
	if err != nil {
		s.failure(w, err)
		return
	}
	scope, permitted := s.rosterScope(w, r)
	if !permitted {
		return
	}
	if !scope.allows(grantee) || grantee["id"] == id {
		detail(w, 404, "Человек не найден")
		return
	}
	if err = v.SharePass(r.Context(), id, grantee["id"].(string)); err != nil {
		s.failure(w, err)
		return
	}
	if err = v.Audit(r.Context(), "pass_shared", actor(r), &id); err != nil {
		s.failure(w, err)
		return
	}
	own, err = v.MetadataAccount(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	s.passList(w, r, own)
}
func (s *Server) unshare(w http.ResponseWriter, r *http.Request) {
	own, ok := s.ownAccount(w, r, true)
	if !ok {
		return
	}
	id := own["id"].(string)
	kind, grant := r.PathValue("kind"), r.PathValue("grant")
	removed := false
	var err error
	switch kind {
	case "aaid":
		removed, err = s.config.Vault.UnsharePass(r.Context(), id, grant)
	}
	if err != nil {
		s.failure(w, err)
		return
	}
	if !removed {
		detail(w, 404, "Такого разрешения нет")
		return
	}
	if kind == "aaid" {
		if err = s.config.Vault.Audit(r.Context(), "pass_unshared", actor(r), &id); err != nil {
			s.failure(w, err)
			return
		}
	}
	own, err = s.config.Vault.MetadataAccount(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	s.passList(w, r, own)
}
func (s *Server) askPass(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	own, ok := s.ownAccount(w, r, true)
	if !ok {
		return
	}
	if s.limited(w, "site:pass-request:"+actor(r), 20, time.Hour) {
		return
	}
	owner, err := s.config.Vault.MetadataAccount(r.Context(), body["account_id"].(string))
	if err != nil {
		s.failure(w, err)
		return
	}
	scope, permitted := s.rosterScope(w, r)
	if !permitted {
		return
	}
	if !scope.allows(owner) || owner["id"] == own["id"] {
		detail(w, 404, "Человек не найден")
		return
	}
	id := owner["id"].(string)
	if _, err = s.config.Vault.RequestShare(r.Context(), id, own["id"].(string)); err != nil {
		switch {
		case errors.Is(err, postgres.ErrShared):
			detail(w, 409, "Этот человек уже разрешил вам пропуск")
		case errors.Is(err, postgres.ErrDenied):
			detail(w, 409, "Этот человек отклонил запрос; повторить можно через сутки")
		case errors.Is(err, postgres.ErrTooMany):
			detail(w, 409, "Слишком много запросов ждут ответа")
		default:
			s.failure(w, err)
		}
		return
	}
	if err = s.config.Vault.Audit(r.Context(), "pass_requested", actor(r), &id); err != nil {
		s.failure(w, err)
		return
	}
	s.myPasses(w, r, own)
}
func (s *Server) answerPass(w http.ResponseWriter, r *http.Request) {
	own, ok := s.ownAccount(w, r, true)
	if !ok {
		return
	}
	id := own["id"].(string)
	decision := r.PathValue("decision")
	if decision != "allow" && decision != "deny" {
		detail(w, 404, "Такого запроса нет")
		return
	}
	scope, permitted := s.rosterScope(w, r)
	if !permitted {
		return
	}
	pending, err := s.config.Vault.ShareRequests(r.Context(), id, true)
	if err != nil {
		s.failure(w, err)
		return
	}
	visible := false
	for _, row := range pending {
		if row["request_id"] == r.PathValue("request") && scope.allows(row) {
			visible = true
			break
		}
	}
	if !visible {
		detail(w, 404, "Такого запроса нет")
		return
	}
	requester, err := s.config.Vault.DecideShare(r.Context(), id, r.PathValue("request"), decision == "allow")
	if err != nil {
		s.failure(w, err)
		return
	}
	if requester == nil {
		detail(w, 404, "Такого запроса нет")
		return
	}
	action := "pass_request_denied"
	if decision == "allow" {
		action = "pass_shared"
	}
	if err = s.config.Vault.Audit(r.Context(), action, actor(r), &id); err != nil {
		s.failure(w, err)
		return
	}
	own, err = s.config.Vault.MetadataAccount(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	s.passList(w, r, own)
}
