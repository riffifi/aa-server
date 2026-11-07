package httpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

func returnPage(path string) bool { return path == "/today" }
func (s *Server) siteCookie(name, value, path string, age int, sameSite http.SameSite) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: path, MaxAge: age, HttpOnly: true, Secure: strings.HasPrefix(s.config.PublicURL, "https://"), SameSite: sameSite}
}
func redirect(w http.ResponseWriter, location string, status int) {
	w.Header().Set("Location", location)
	w.WriteHeader(status)
}
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	if (s.config.Verify == nil && s.config.Exchange == nil) || s.config.AAIDURL == "" {
		detail(w, 503, "AA ID не настроен на сервере")
		return
	}
	fresh, ok := integer(w, r, "fresh", 0)
	if !ok {
		return
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		s.failure(w, err)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(nonce[:])
	location := strings.TrimRight(s.config.AAIDURL, "/") + "/?continue=aa&state=" + url.QueryEscape(state)
	if fresh != 0 {
		location += "&fresh=1"
	}
	http.SetCookie(w, s.siteCookie("aa_state", state, "/auth", 900, http.SameSiteLaxMode))
	if next := r.URL.Query().Get("next"); returnPage(next) {
		http.SetCookie(w, s.siteCookie("aa_next", next, "/auth", 900, http.SameSiteLaxMode))
	}
	redirect(w, location, 303)
}
func optionalGroup(value any) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	switch number := value.(type) {
	case int64:
		return &number, nil
	case json.Number:
		value, err := number.Int64()
		return &value, err
	case float64:
		value := int64(number)
		return &value, nil
	}
	return nil, nil
}
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	expected := cookie(r, "aa_state")
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	var person map[string]any
	if s.config.Exchange != nil && utf8.RuneCountInString(code) >= 20 && utf8.RuneCountInString(code) <= 200 && expected != "" && hmac.Equal([]byte(state), []byte(expected)) {
		person, _ = s.config.Exchange(r.Context(), code)
	}
	if person == nil {
		redirect(w, "/?signin=failed", 303)
		return
	}
	id, ok := person["id"].(string)
	if !ok {
		redirect(w, "/?signin=failed", 303)
		return
	}
	mirea, ok := person["mirea"].(string)
	if !ok {
		redirect(w, "/?signin=failed", 303)
		return
	}
	groupID, err := optionalGroup(person["group_id"])
	if err != nil {
		s.failure(w, err)
		return
	}
	var groupName *string
	if name, ok := person["group_name"].(string); ok {
		groupName = &name
	}
	v := s.config.Vault
	token, err := v.CreateSiteSession(r.Context(), id, mirea, groupID, groupName, 30*86400)
	if err != nil {
		s.failure(w, err)
		return
	}
	if sessionID, ok := person["session_id"].(string); ok && sessionID != "" {
		if err = v.BindSiteIdentity(r.Context(), token, sessionID); err != nil {
			s.failure(w, err)
			return
		}
	} else if s.config.RequireIdentityState {
		_ = v.Logout(r.Context(), "site", token)
		redirect(w, "/?signin=failed", 303)
		return
	}
	own, err := v.MetadataByPulse(r.Context(), mirea)
	if err != nil {
		s.failure(w, err)
		return
	}
	if own != nil && own["group_id"] == nil && groupID != nil && *groupID != 0 {
		if _, err = v.UpdateGroup(r.Context(), own["id"].(string), groupID, groupName, own["version"].(int64)); err != nil {
			s.failure(w, err)
			return
		}
	}
	back := cookie(r, "aa_next")
	if !returnPage(back) {
		back = "/"
	}
	for _, name := range []string{"aa_state", "aa_next"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/auth", MaxAge: -1, Expires: time.Unix(0, 0)})
	}
	http.SetCookie(w, s.siteCookie("aa_site", token, "/", 30*86400, http.SameSiteStrictMode))
	redirect(w, back, 303)
}
func (s *Server) siteLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.config.Vault.Logout(r.Context(), "site", cookie(r, "aa_site")); err != nil {
		s.failure(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "aa_site", Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0)})
	write(w, 200, map[string]any{"ok": true})
}
