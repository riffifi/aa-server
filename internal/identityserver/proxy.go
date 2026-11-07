package identityserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/httpserver"
	"aa-server/internal/postgres"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type apiError struct {
	Status int
	Detail string
}

func (e *apiError) Error() string { return e.Detail }
func ascii(value string) bool {
	for _, c := range value {
		if c > 127 {
			return false
		}
	}
	return true
}
func (s *Server) access(user, session map[string]any) (string, error) {
	now := s.config.Store.Now().Unix()
	return cryptocompat.Sign(s.config.Seed, map[string]any{"iss": s.config.Issuer, "aud": s.config.Audience, "sub": user["id"], "sid": session["id"], "mirea": user["pulse_uid"], "grp": user["group_id"], "group_name": user["group_name"], "usr": user["username"], "iat": now, "exp": now + 60})
}
func (s *Server) doSessions(ctx context.Context, user, session map[string]any, method, path string, body any, params url.Values) (int, http.Header, map[string]any, error) {
	token, err := s.access(user, session)
	if err != nil {
		return 0, nil, nil, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint := s.config.SessionsURL + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.config.HTTP.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
	if err != nil || len(raw) > 64<<20 {
		return 0, nil, nil, errors.New("invalid session server response")
	}
	var data map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&data); err != nil && response.StatusCode == 200 {
		return 0, nil, nil, errors.New("invalid session server JSON")
	}
	return response.StatusCode, response.Header.Clone(), data, nil
}
func (s *Server) sessionsCall(ctx context.Context, user, session map[string]any, method, path string, body any, params url.Values) (map[string]any, error) {
	status, headers, data, err := s.doSessions(ctx, user, session, method, path, body, params)
	if err != nil {
		return nil, &apiError{503, "Сервер AntiAttendance недоступен"}
	}
	if status == 404 || status == 409 {
		message := "Ваш аккаунт МИРЭА не подключён к серверу AntiAttendance"
		if status == 409 {
			message = "Сейчас это сделать нельзя"
		}
		if strings.Contains(headers.Get("Content-Type"), "json") {
			if original, ok := data["detail"].(string); ok && !ascii(original) {
				message = original
			}
		}
		return nil, &apiError{status, message}
	}
	if status != 200 {
		return nil, &apiError{503, "Сервер AntiAttendance не ответил"}
	}
	return data, nil
}
func selectFields(data map[string]any, keys []string, includeMissing bool) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		value, exists := data[key]
		if includeMissing || exists {
			out[key] = value
		}
	}
	return out
}
func person(data map[string]any) map[string]any {
	return selectFields(data, []string{"kind", "id", "account_id", "username", "label", "group_name"}, false)
}
func accountView(data map[string]any) map[string]any {
	return selectFields(data, []string{"account", "connected", "label", "pass_allowed", "group_name"}, true)
}
func truthy(value any) bool {
	switch item := value.(type) {
	case nil:
		return false
	case bool:
		return item
	case string:
		return item != ""
	case json.Number:
		number, err := item.Float64()
		return err == nil && number != 0
	case []any:
		return len(item) > 0
	case map[string]any:
		return len(item) > 0
	}
	return true
}
func peopleList(value any) []any {
	out := []any{}
	if list, ok := value.([]any); ok {
		for _, raw := range list {
			if row, ok := raw.(map[string]any); ok {
				out = append(out, person(row))
			}
		}
	}
	return out
}
func passView(data map[string]any) map[string]any {
	return map[string]any{"pass_allowed": truthy(data["pass_allowed"]), "people": peopleList(data["people"]), "requests": peopleList(data["requests"])}
}
func passesView(data map[string]any) map[string]any {
	passes := []any{}
	if list, ok := data["passes"].([]any); ok {
		for _, raw := range list {
			if row, ok := raw.(map[string]any); ok {
				item := person(row)
				item["pass_allowed"] = truthy(row["pass_allowed"])
				passes = append(passes, item)
			}
		}
	}
	return map[string]any{"passes": passes, "requests": peopleList(data["requests"])}
}

func (s *Server) proxy(method, path string, specs []httpserver.Field, view func(map[string]any) map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body any
		if len(specs) > 0 {
			input, ok := httpserver.DecodeBody(w, r, specs)
			if !ok {
				return
			}
			clean := map[string]any{}
			for _, field := range specs {
				if value := input[field.Name]; value != nil {
					clean[field.Name] = value
				}
			}
			body = clean
		}
		session, user, ok := s.webSession(w, r)
		if !ok {
			return
		}
		params := url.Values{}
		if path == "/api/me/people" {
			query := []rune(r.URL.Query().Get("q"))
			if len(query) > 60 {
				query = query[:60]
			}
			params.Set("q", string(query))
		}
		target := path
		if strings.Contains(path, "{kind}") {
			kind, id := r.PathValue("kind"), r.PathValue("grant")
			if kind != "aaid" || !safeID.MatchString(id) {
				detail(w, 404, "Такого разрешения нет")
				return
			}
			target = strings.ReplaceAll(strings.ReplaceAll(path, "{kind}", kind), "{grant}", id)
		}
		if strings.Contains(path, "{request}") {
			id, decision := r.PathValue("request"), r.PathValue("decision")
			if !safeID.MatchString(id) || (decision != "allow" && decision != "deny") {
				detail(w, 404, "Такого запроса нет")
				return
			}
			target = strings.ReplaceAll(strings.ReplaceAll(path, "{request}", id), "{decision}", decision)
		}
		data, err := s.sessionsCall(r.Context(), user, session, method, target, body, params)
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, view(data))
	}
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,23}$`)

func (s *Server) username(w http.ResponseWriter, r *http.Request) {
	body, ok := httpserver.DecodeBody(w, r, []httpserver.Field{{Name: "username", Kind: "string", Required: true, Min: 1, Max: 40}})
	if !ok {
		return
	}
	session, user, ok := s.webSession(w, r)
	if !ok {
		return
	}
	username := cases.Lower(language.Und).String(strings.TrimLeft(strings.TrimSpace(body["username"].(string)), "@"))
	if !usernamePattern.MatchString(username) {
		detail(w, 400, "Username: 3–24 символа, латинские буквы, цифры и _, начинается с буквы")
		return
	}
	if user["username"] != username {
		if s.limit(w, r, "username:"+user["id"].(string), 10, time.Hour) {
			return
		}
		var err error
		user, err = s.config.Store.SetUsername(r.Context(), user["id"].(string), &username)
		if errors.Is(err, postgres.ErrUsernameTaken) {
			detail(w, 409, "Этот username уже занят")
			return
		}
		if err != nil {
			failure(w, err)
			return
		}
		_, _ = s.sessionsCall(r.Context(), user, session, "GET", "/api/me/account", nil, nil)
	}
	write(w, 200, map[string]any{"username": user["username"]})
}
func (s *Server) attendance(w http.ResponseWriter, r *http.Request) {
	session, user, ok := s.webSession(w, r)
	if !ok {
		return
	}
	status, _, data, err := s.doSessions(r.Context(), user, session, "GET", "/api/me/attendance", nil, nil)
	if err != nil {
		detail(w, 503, "Сервер сессий недоступен")
		return
	}
	if status == 404 {
		detail(w, 404, "Ваш аккаунт МИРЭА не подключён к серверу AntiAttendance")
		return
	}
	if status == 409 {
		detail(w, 409, "Сессия МИРЭА на сервере завершена, войдите на сервере заново")
		return
	}
	if status != 200 {
		detail(w, 503, "Не удалось получить данные турникетов")
		return
	}
	write(w, 200, selectFields(data, []string{"inside", "last_in", "last_out", "events"}, true))
}
func (s *Server) proxyRoutes() {
	settings := []httpserver.Field{{Name: "label", Kind: "string", Nullable: true, Min: 1, Max: 60}, {Name: "pass_allowed", Kind: "bool", Nullable: true}}
	share := []httpserver.Field{{Name: "account_id", Kind: "string", Required: true, Min: 1, Max: 64}}
	s.handle("GET /web/account", s.proxy("GET", "/api/me/account", nil, accountView))
	s.handle("PUT /web/account", s.proxy("PUT", "/api/me/account", settings, accountView))
	s.handle("PUT /web/username", s.username)
	s.handle("GET /web/people", s.proxy("GET", "/api/me/people", nil, func(data map[string]any) map[string]any { return map[string]any{"people": peopleList(data["people"])} }))
	s.handle("GET /web/passes", s.proxy("GET", "/api/me/passes", nil, passesView))
	s.handle("POST /web/pass/requests", s.proxy("POST", "/api/me/pass/requests", share, passesView))
	s.handle("GET /web/pass", s.proxy("GET", "/api/me/pass", nil, passView))
	s.handle("POST /web/pass/people", s.proxy("POST", "/api/me/pass/people", share, passView))
	s.handle("DELETE /web/pass/people/{kind}/{grant}", s.proxy("DELETE", "/api/me/pass/people/{kind}/{grant}", nil, passView))
	s.handle("POST /web/pass/requests/{request}/{decision}", s.proxy("POST", "/api/me/pass/requests/{request}/{decision}", nil, passView))
	s.handle("GET /web/attendance", s.attendance)
}
