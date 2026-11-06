package httpserver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"aa-server/internal/browser"
	"aa-server/internal/postgres"
	qrcode "github.com/skip2/go-qrcode"
)

func positiveEnv(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	if value < 1 {
		return 1
	}
	return value
}
func (s *Server) initBrowser(ctx context.Context) {
	if s.config.Browser == nil {
		s.ownedBrowser = browser.New(ctx, browser.Config{LoginSlots: positiveEnv("LOGIN_SLOTS", 6), RenewSlots: positiveEnv("RENEW_SLOTS", 2)})
		s.config.Browser = s.ownedBrowser
	}
	if s.config.Logins == nil {
		s.config.Logins = browser.NewJobs(ctx, s.config.Browser)
		s.config.Logins.MaxActive = positiveEnv("MAX_ACTIVE_LOGINS", 24)
		s.config.Logins.TTL = time.Duration(positiveEnv("LOGIN_TTL_SECONDS", 600)) * time.Second
		s.config.Logins.Retain = 900 * time.Second
		s.ownedLogins = true
	}
}

type accountLogin struct {
	runner           browser.LoginRunner
	vault            *postgres.Vault
	label, accountID string
	sessionExpected  *int64
}

func (l accountLogin) Login(ctx context.Context, update func(browser.Update)) (*browser.Result, error) {
	result, err := l.runner.Login(ctx, update)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("missing login result")
	}
	defer func() { result.Credentials = nil }()
	row, err := l.vault.Upsert(ctx, postgres.Upsert{PulseID: result.UID, Label: l.label, AccountID: l.accountID, Credentials: result.Credentials, SessionExpected: l.sessionExpected})
	if err != nil {
		var conflict *postgres.Conflict
		if errors.As(err, &conflict) {
			return nil, &browser.UserError{Message: "The login belongs to a different account or a newer version was stored. Start again."}
		}
		return nil, err
	}
	return &browser.Result{AccountID: row["id"].(string)}, nil
}
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	body, ok := DecodeBody(w, r, []Field{{Name: "label", Kind: "string", Nullable: true, Max: 60}, {Name: "account_id", Kind: "string", Nullable: true}})
	if !ok {
		return
	}
	if _, ok = s.panel(w, r, true); !ok {
		return
	}
	label, _ := body["label"].(string)
	accountID, _ := body["account_id"].(string)
	var expected *int64
	if accountID != "" {
		row, err := s.config.Vault.MetadataAccount(r.Context(), accountID)
		if err != nil {
			s.failure(w, err)
			return
		}
		if row == nil {
			detail(w, 404, "Account not found")
			return
		}
		version := row["session_version"].(int64)
		expected = &version
	}
	runner := accountLogin{s.config.Browser, s.config.Vault, label, accountID, expected}
	state, token, err := s.config.Logins.StartWith(runner, body["label"], "Could not complete Pulse login. Retry in the panel.")
	if err != nil {
		if errors.Is(err, browser.ErrBusy) {
			detail(w, 429, "The server is busy with other logins right now; try again in a minute")
		} else {
			s.failure(w, err)
		}
		return
	}
	write(w, 200, map[string]any{"id": state.ID, "enrollment_path": "/enroll/" + state.ID + "/" + token, "expires_in": 600})
}
func (s *Server) loginJob(w http.ResponseWriter, r *http.Request, public bool) (browser.JobState, bool) {
	id := r.PathValue("identifier")
	var state browser.JobState
	var err error
	if public {
		state, err = s.config.Logins.AdminState(id)
		if err != nil || time.Since(state.Created) > 900*time.Second {
			detail(w, 404, "Login has expired")
			return state, false
		}
		state, err = s.config.Logins.State(id, r.PathValue("token"))
	} else {
		state, err = s.config.Logins.AdminState(id)
	}
	if err != nil || time.Since(state.Created) > 900*time.Second {
		message := "Login has expired"
		if public {
			message = "Login not found"
		}
		detail(w, 404, message)
		return state, false
	}
	if public && time.Since(state.Created) > 600*time.Second {
		detail(w, 404, "Login not found")
		return state, false
	}
	return state, true
}
func jobData(state browser.JobState) map[string]any {
	var account any
	if state.AccountID != "" {
		account = state.AccountID
	}
	return map[string]any{"status": state.Status, "qr": state.QR, "error": state.Error, "account_id": account, "label": state.Label}
}
func (s *Server) loginState(w http.ResponseWriter, r *http.Request) {
	public := r.PathValue("token") != ""
	if !public {
		if _, ok := s.panel(w, r, false); !ok {
			return
		}
	}
	state, ok := s.loginJob(w, r, public)
	if ok {
		write(w, 200, jobData(state))
	}
}
func (s *Server) loginQR(w http.ResponseWriter, r *http.Request) {
	public := r.PathValue("token") != ""
	if !public {
		if _, ok := s.panel(w, r, false); !ok {
			return
		}
	}
	state, ok := s.loginJob(w, r, public)
	if !ok {
		return
	}
	if state.QR == nil {
		detail(w, 404, "QR is not ready")
		return
	}
	png, err := qrcode.Encode(*state.QR, qrcode.Medium, -10)
	if err != nil {
		s.failure(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(200)
	_, _ = w.Write(png)
}
func (s *Server) cancelLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, true); !ok {
		return
	}
	if _, ok := s.loginJob(w, r, false); !ok {
		return
	}
	if err := s.config.Logins.Cancel(r.PathValue("identifier")); err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"ok": true})
}
func (s *Server) enrollment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.loginJob(w, r, true); ok {
		s.asset(w, r, "enroll.html", false)
	}
}
func (s *Server) claimEnrollment(w http.ResponseWriter, r *http.Request) {
	state, ok := s.loginJob(w, r, true)
	if !ok {
		return
	}
	if state.Status != "complete" || state.AccountID == "" {
		detail(w, 409, "Login is not complete yet")
		return
	}
	write(w, 200, map[string]any{"account_id": state.AccountID})
}
func (s *Server) loginRoutes() {
	s.handle("POST /panel/api/logins", s.startLogin)
	s.handle("GET /panel/api/logins/{identifier}", s.loginState)
	s.handle("GET /panel/api/logins/{identifier}/qr", s.loginQR)
	s.handle("DELETE /panel/api/logins/{identifier}", s.cancelLogin)
	s.handle("GET /enroll/{identifier}/{token}", s.enrollment)
	s.handle("GET /enroll/{identifier}/{token}/state", s.loginState)
	s.handle("GET /enroll/{identifier}/{token}/qr", s.loginQR)
	s.handle("POST /enroll/{identifier}/{token}/claim", s.claimEnrollment)
}
