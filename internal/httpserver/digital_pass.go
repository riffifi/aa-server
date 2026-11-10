package httpserver

import (
	"context"
	"net/http"
	"time"

	"aa-server/internal/pulse"
)

// Enrollment is owner-only. Sharing permits reading a stored pass, never
// requesting verification messages or replacing another person's credential.
func (s *Server) passEnrollment(w http.ResponseWriter, r *http.Request, method string, body []byte) (map[string]any, pulse.Reply, func(), bool) {
	own, ok := s.ownAccount(w, r, true)
	if !ok {
		return nil, pulse.Reply{}, nil, false
	}
	id := own["id"].(string)
	if own["pass_allowed"] == int64(0) {
		detail(w, 403, "Enable your pass in AA ID settings first")
		return nil, pulse.Reply{}, nil, false
	}
	count := 20
	if method == "pass-code" {
		count = 6
	}
	if s.limited(w, method+":"+id, count, 10*time.Minute) {
		return nil, pulse.Reply{}, nil, false
	}
	s.passMu.Lock()
	if s.passBusy == nil {
		s.passBusy = map[string]bool{}
	}
	if s.passBusy[id] {
		s.passMu.Unlock()
		detail(w, 409, "A pass operation is already running")
		return nil, pulse.Reply{}, nil, false
	}
	s.passBusy[id] = true
	s.passMu.Unlock()
	release := func() { s.passMu.Lock(); delete(s.passBusy, id); s.passMu.Unlock() }
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	done := func() { cancel(); release() }
	row, err := s.config.Vault.Account(ctx, id)
	if err != nil {
		done()
		s.failure(w, err)
		return nil, pulse.Reply{}, nil, false
	}
	if row == nil || row["pulse_id"] != identity(r) {
		done()
		detail(w, 404, "Account not found")
		return nil, pulse.Reply{}, nil, false
	}
	if row["pass_allowed"] == int64(0) {
		done()
		detail(w, 403, "Enable your pass in AA ID settings first")
		return nil, pulse.Reply{}, nil, false
	}
	secret, err := s.config.Vault.Secret(row)
	if err != nil {
		done()
		s.failure(w, err)
		return nil, pulse.Reply{}, nil, false
	}
	cookie, _ := secret["cookie"].(string)
	if cookie == "" || row["state"] != "valid" {
		done()
		detail(w, 409, "Sign in to MIREA again before enrolling a pass")
		return nil, pulse.Reply{}, nil, false
	}
	// Fetch a fresh token for this account, never accept or expose a client token.
	tokenReply, err := s.config.Pulse.RPC(ctx, cookie, "pass-token", pulse.Frame(nil, 0), "")
	if !s.passUpstream(w, tokenReply, err) {
		done()
		return nil, pulse.Reply{}, nil, false
	}
	token, err := pulse.PassToken(tokenReply.Body, tokenReply.Headers)
	if err != nil {
		done()
		detail(w, 502, "Invalid pass token response from MIREA")
		return nil, pulse.Reply{}, nil, false
	}
	reply, err := s.config.Pulse.RPC(ctx, cookie, method, body, token)
	if !s.passUpstream(w, reply, err) {
		done()
		return nil, pulse.Reply{}, nil, false
	}
	return row, reply, done, true
}

func (s *Server) passUpstream(w http.ResponseWriter, reply pulse.Reply, err error) bool {
	if err != nil {
		detail(w, 503, "MIREA is unavailable; retry later")
		return false
	}
	if reply.Status == 401 || reply.Status == 403 {
		detail(w, 409, "MIREA rejected the session; sign in again")
		return false
	}
	if reply.Status != 200 {
		detail(w, 503, "MIREA is unavailable; retry later")
		return false
	}
	_, code, err := pulse.Message(reply.Body, reply.Headers)
	if err == nil && (code == 16 || code == 7) {
		detail(w, 409, "MIREA rejected the session; sign in again")
		return false
	}
	if err != nil || code != 0 {
		detail(w, 502, "Invalid or unsuccessful pass response from MIREA")
		return false
	}
	return true
}

func (s *Server) digitalPassCode(w http.ResponseWriter, r *http.Request) {
	_, reply, done, ok := s.passEnrollment(w, r, "pass-code", pulse.Frame(nil, 0))
	if !ok {
		return
	}
	defer done()
	tag, data, err := pulse.PassResult(reply.Body, reply.Headers)
	if err != nil {
		detail(w, 502, "Invalid verification response from MIREA")
		return
	}
	state := "unavailable"
	var retry any
	if tag == 1 || tag == 2 {
		retry, err = pulse.PassRetryAt(data, tag == 1)
		if err != nil {
			detail(w, 502, "Invalid verification retry time from MIREA")
			return
		}
		state = "code_sent"
		if tag == 2 {
			state = "wait"
		}
	}
	write(w, 200, map[string]any{"state": state, "retry_at": retry})
}

func (s *Server) digitalPassIssue(w http.ResponseWriter, r *http.Request) {
	body, ok := DecodeBody(w, r, []Field{{Name: "code", Kind: "string", Required: true, Max: 6}})
	if !ok {
		return
	}
	code, _ := body["code"].(string)
	if !pulse.ValidPassCode(code) {
		detail(w, 422, "Enter the six-digit MIREA verification code")
		return
	}
	row, reply, done, ok := s.passEnrollment(w, r, "pass-issue", pulse.PassIssueRequest(code))
	if !ok {
		return
	}
	defer done()
	tag, data, err := pulse.PassResult(reply.Body, reply.Headers)
	if err != nil {
		detail(w, 502, "Invalid pass response from MIREA")
		return
	}
	if tag == 2 {
		detail(w, 422, "Incorrect verification code")
		return
	}
	if tag == 3 {
		detail(w, 409, "MIREA could not issue a digital pass")
		return
	}
	number, err := pulse.PassNumber(data)
	if err != nil {
		detail(w, 502, "Invalid digital pass number from MIREA")
		return
	}
	id := row["id"].(string)
	if _, err = s.config.Vault.SaveIssuedPass(r.Context(), id, number, row["pass_version"].(int64), row["session_version"].(int64)); err != nil {
		s.failure(w, err)
		return
	}
	s.changed()
	write(w, 200, map[string]any{"account_id": id, "number": number})
}

func (s *Server) passCards(w http.ResponseWriter, r *http.Request) ([]map[string]any, bool) {
	scope, ok := s.rosterScope(w, r)
	if !ok {
		return nil, false
	}
	rows, err := s.config.Vault.VisibleMetadata(r.Context(), identity(r))
	if err != nil {
		s.failure(w, err)
		return nil, false
	}
	allowed := []map[string]any{}
	for _, row := range rows {
		if scope.allows(row) {
			allowed = append(allowed, row)
		}
	}
	return allowed, true
}

func (s *Server) digitalPasses(w http.ResponseWriter, r *http.Request) {
	rows, ok := s.passCards(w, r)
	if !ok {
		return
	}
	items := []any{}
	for _, row := range rows {
		item := card(row)
		item["own"] = row["pulse_id"] == identity(r)
		item["pass_allowed"] = row["pass_allowed"] != int64(0)
		items = append(items, item)
	}
	write(w, 200, map[string]any{"passes": items})
}

func (s *Server) digitalPass(w http.ResponseWriter, r *http.Request) {
	rows, ok := s.passCards(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	for _, row := range rows {
		if row["id"] != id {
			continue
		}
		// Check the SQL permission again at the point credentials are read;
		// a revoked share cannot use an earlier cached metadata snapshot.
		scope, ok := s.rosterScope(w, r)
		if !ok {
			return
		}
		numbers, err := s.config.Vault.VisiblePassNumbers(r.Context(), identity(r), []string{id}, scope.groups)
		if err != nil {
			s.failure(w, err)
			return
		}
		number, present := numbers[id]
		if !present {
			break
		}
		write(w, 200, map[string]any{"account_id": id, "number": number})
		return
	}
	detail(w, 404, "Pass not found")
}
