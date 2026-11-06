package identityserver

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aa-server/internal/httpserver"
	"aa-server/internal/postgres"
)

func webCookie(r *http.Request) string {
	cookie, err := r.Cookie("aaid_web")
	if err != nil {
		return ""
	}
	return cookie.Value
}
func (s *Server) webSession(w http.ResponseWriter, r *http.Request) (map[string]any, map[string]any, bool) {
	session, err := s.config.Store.WebSession(r.Context(), webCookie(r))
	if err != nil {
		failure(w, err)
		return nil, nil, false
	}
	if session == nil {
		detail(w, 401, "Войдите в AA ID")
		return nil, nil, false
	}
	user, err := s.config.Store.User(r.Context(), session["user_id"].(string))
	if err != nil {
		failure(w, err)
		return nil, nil, false
	}
	if user == nil || user["status"] != "active" {
		detail(w, 403, "Этот AA ID заблокирован")
		return nil, nil, false
	}
	return session, user, true
}
func (s *Server) webMe(w http.ResponseWriter, r *http.Request) {
	session, user, ok := s.webSession(w, r)
	if !ok {
		return
	}
	devices, err := s.config.Store.Sessions(r.Context(), user["id"].(string))
	if err != nil {
		failure(w, err)
		return
	}
	for _, device := range devices {
		device["current"] = device["id"] == session["id"]
	}
	data := public(user)
	data["sessions"] = devices
	data["max_sessions"] = s.config.MaxSessions
	var site any
	if s.config.SiteURL != "" {
		site = s.config.SiteURL
	}
	data["site_url"] = site
	write(w, 200, data)
}
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	body, ok := httpserver.DecodeBody(w, r, []httpserver.Field{{Name: "state", Kind: "string", Required: true, Pattern: "^[A-Za-z0-9_-]{16,128}$"}})
	if !ok {
		return
	}
	_, user, ok := s.webSession(w, r)
	if !ok {
		return
	}
	if s.config.SiteURL == "" {
		detail(w, 404, "Вход на сайт AntiAttendance не настроен")
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		failure(w, err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := s.config.Store.Now()
	s.mu.Lock()
	for key, entry := range s.codes {
		if entry.Expires.Before(now) {
			delete(s.codes, key)
		}
	}
	s.codes[postgres.Digest(token)] = code{now.Add(time.Minute), user["id"].(string)}
	s.mu.Unlock()
	write(w, 200, map[string]any{"redirect": s.config.SiteURL + "/auth/callback?code=" + url.QueryEscape(token) + "&state=" + url.QueryEscape(body["state"].(string))})
}
func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	body, ok := httpserver.DecodeBody(w, r, []httpserver.Field{{Name: "code", Kind: "string", Required: true, Min: 20, Max: 200}})
	if !ok {
		return
	}
	if s.limit(w, r, "exchange", 60, time.Minute) {
		return
	}
	hash := postgres.Digest(body["code"].(string))
	s.mu.Lock()
	entry, exists := s.codes[hash]
	delete(s.codes, hash)
	s.mu.Unlock()
	if !exists || entry.Expires.Before(s.config.Store.Now()) {
		detail(w, 400, "Код входа недействителен")
		return
	}
	user, err := s.config.Store.User(r.Context(), entry.UserID)
	if err != nil {
		failure(w, err)
		return
	}
	if user == nil || user["status"] != "active" {
		detail(w, 403, "Этот AA ID заблокирован")
		return
	}
	write(w, 200, map[string]any{"id": user["id"], "mirea": user["pulse_uid"], "group_id": user["group_id"], "group_name": user["group_name"]})
}
func (s *Server) webRevoke(w http.ResponseWriter, r *http.Request) {
	_, user, ok := s.webSession(w, r)
	if !ok {
		return
	}
	changed, err := s.config.Store.Revoke(r.Context(), user["id"].(string), r.PathValue("session"), "owner")
	if err != nil {
		failure(w, err)
		return
	}
	if !changed {
		detail(w, 404, "Устройство не найдено")
		return
	}
	write(w, 200, map[string]any{"ok": true})
}
func (s *Server) webLogout(w http.ResponseWriter, r *http.Request) {
	session, err := s.config.Store.WebSession(r.Context(), webCookie(r))
	if err != nil {
		failure(w, err)
		return
	}
	if session != nil {
		if _, err = s.config.Store.Revoke(r.Context(), session["user_id"].(string), session["id"].(string), "logout"); err != nil {
			failure(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "aaid_web", Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0)})
	write(w, 200, map[string]any{"ok": true})
}
func (s *Server) webRoutes() {
	s.handle("POST /web/logins", s.webLogin)
	s.handle("GET /web/logins/{identifier}", s.webLoginState)
	s.handle("POST /web/logins/{identifier}/claim", s.webLoginClaim)
	s.handle("GET /web/me", s.webMe)
	s.handle("POST /web/authorize", s.authorize)
	s.handle("POST /v1/site/exchange", s.exchange)
	s.handle("DELETE /web/sessions/{session}", s.webRevoke)
	s.handle("POST /web/logout", s.webLogout)
	s.proxyRoutes()
	if s.config.StaticDir != "" {
		s.handle("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				detail(w, 404, "Not Found")
				return
			}
			httpserver.ServeAsset(w, r, s.config.StaticDir, "index.html", false)
		})
		s.handle("GET /static/", func(w http.ResponseWriter, r *http.Request) {
			httpserver.ServeAsset(w, r, s.config.StaticDir, strings.TrimPrefix(r.URL.Path, "/static/"), true)
		})
	}
}
