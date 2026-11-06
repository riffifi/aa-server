package identityserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/cryptocompat"
	"aa-server/internal/postgres"
	qrcode "github.com/skip2/go-qrcode"
)

func loginFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, browser.ErrMissing):
		detail(w, 404, err.Error())
	case errors.Is(err, browser.ErrIncomplete):
		detail(w, 409, err.Error())
	case errors.Is(err, browser.ErrBusy):
		detail(w, 503, err.Error())
	default:
		failure(w, err)
	}
}
func (s *Server) webLogin(w http.ResponseWriter, r *http.Request) {
	if s.limit(w, r, "web-login", 10, 5*time.Minute) {
		return
	}
	state, token, err := s.config.Logins.Start()
	if err != nil {
		loginFailure(w, err)
		return
	}
	write(w, 200, map[string]any{"id": state.ID, "token": token})
}
func (s *Server) webLoginState(w http.ResponseWriter, r *http.Request) {
	state, err := s.config.Logins.State(r.PathValue("identifier"), r.Header.Get("X-Login-Token"))
	if err != nil {
		loginFailure(w, err)
		return
	}
	var png any
	if state.QR != nil {
		data, err := qrcode.Encode(*state.QR, qrcode.Medium, -10)
		if err != nil {
			failure(w, err)
			return
		}
		png = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	}
	write(w, 200, map[string]any{"status": state.Status, "error": state.Error, "qr": state.QR, "qr_png": png})
}
func browserName(agent string) string {
	name := "Браузер"
	for _, pair := range [][2]string{{"YaBrowser", "Яндекс Браузер"}, {"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}} {
		if strings.Contains(agent, pair[0]) {
			name = pair[1]
			break
		}
	}
	for _, pair := range [][2]string{{"Android", "Android"}, {"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"Linux", "Linux"}} {
		if strings.Contains(agent, pair[0]) {
			return name + " · " + pair[1]
		}
	}
	return name
}
func (s *Server) handOff(r *http.Request, user map[string]any, credentials map[string]any) bool {
	if len(credentials) == 0 {
		return false
	}
	now := s.config.Store.Now().Unix()
	token, err := cryptocompat.Sign(s.config.Seed, map[string]any{"iss": s.config.Issuer, "aud": "aa-link", "sub": user["id"], "mirea": user["pulse_uid"], "grp": user["group_id"], "usr": user["username"], "iat": now, "exp": now + 60})
	if err != nil {
		return false
	}
	body := map[string]any{}
	for key, value := range credentials {
		body[key] = value
	}
	body["group_id"] = user["group_id"]
	body["group_name"] = user["group_name"]
	encoded, err := json.Marshal(body)
	if err != nil {
		return false
	}
	request, err := http.NewRequestWithContext(r.Context(), "POST", s.config.SessionsURL+"/internal/aaid/link", bytes.NewReader(encoded))
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.config.HTTP.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == 200
}
func (s *Server) webLoginClaim(w http.ResponseWriter, r *http.Request) {
	result, err := s.config.Logins.Claim(r.PathValue("identifier"), r.Header.Get("X-Login-Token"))
	if err != nil {
		loginFailure(w, err)
		return
	}
	defer func() { result.Credentials = nil }()
	user, err := s.config.Store.BindMirea(r.Context(), result.UID, result.GroupID, result.GroupName)
	if errors.Is(err, postgres.ErrBlocked) {
		detail(w, 403, "Этот AA ID заблокирован")
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	linked := s.handOff(r, user, result.Credentials)
	_, token, err := s.config.Store.CreateSession(r.Context(), user["id"].(string), browserName(r.UserAgent()), float64(s.config.WebTTL), 1, "web")
	if err != nil {
		failure(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "aaid_web", Value: token, MaxAge: int(s.config.WebTTL), Path: "/", Secure: strings.HasPrefix(s.config.PublicURL, "https://"), HttpOnly: true, SameSite: http.SameSiteStrictMode})
	write(w, 200, map[string]any{"ok": true, "linked": linked})
}
