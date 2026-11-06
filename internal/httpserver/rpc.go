package httpserver

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
)

func (s *Server) requirePass(w http.ResponseWriter, r *http.Request, row map[string]any, operation string) bool {
	if row["pass_allowed"] == int64(0) {
		detail(w, 403, "The owner has turned the pass off")
		return false
	}
	id := row["id"].(string)
	allowed, err := s.owner(r, id)
	if err != nil {
		s.failure(w, err)
		return false
	}
	if allowed {
		return true
	}
	claims, _ := r.Context().Value(principalKey{}).(map[string]any)
	if claims != nil {
		mirea, _ := claims["mirea"].(string)
		own, err := s.config.Vault.MetadataByPulse(r.Context(), mirea)
		if err != nil {
			s.failure(w, err)
			return false
		}
		if own != nil {
			if own["id"] == id {
				return true
			}
			var shared bool
			err = s.config.Vault.Pool.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM aa.pass_shares WHERE account_id=$1 AND grantee_id=$2)", id, own["id"]).Scan(&shared)
			if err != nil {
				s.failure(w, err)
				return false
			}
			if shared {
				return true
			}
		}
	}
	daily := cryptocompat.Daily(cryptocompat.PassTestingKey([]byte(s.config.MasterKey)), s.config.Vault.Now())
	if password := r.Header.Get("X-Pass-Test-Key"); password != "" && cryptocompat.MatchDaily(password, daily) {
		if err := s.config.Vault.Audit(r.Context(), "pass_testing_"+operation, "daily-admin", &id); err != nil {
			s.failure(w, err)
			return false
		}
		return true
	}
	if r.Header.Get("X-Pass-Test-Key") != "" {
		w.Header().Set("X-Pass-Test-Invalid", "1")
	}
	detail(w, 403, "Owner approval through AA ID is required for this person")
	return false
}

func (s *Server) rpc(w http.ResponseWriter, r *http.Request) {
	id, method := r.PathValue("id"), r.PathValue("method")
	if _, ok := pulse.Methods[method]; !ok {
		detail(w, 404, "RPC not allowed")
		return
	}
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
	if (method == "pass-token" || method == "pass-code" || method == "pass-issue") && !s.requirePass(w, r, row, method) {
		return
	}
	if method == "pass-code" || method == "pass-issue" {
		key := "rpc:" + method + ":" + id
		now := v.Now()
		s.attemptsMu.Lock()
		queue := s.attempts[key]
		for len(queue) > 0 && queue[0].Before(now.Add(-600*time.Second)) {
			queue = queue[1:]
		}
		limit := 20
		if method == "pass-code" {
			limit = 6
		}
		if len(queue) >= limit {
			s.attempts[key] = queue
			s.attemptsMu.Unlock()
			detail(w, 429, "Too many verification attempts; wait before retrying")
			return
		}
		s.attempts[key] = append(queue, now)
		s.attemptsMu.Unlock()
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil {
		detail(w, 400, "Invalid unary gRPC request")
		return
	}
	if len(body) > 65536 {
		detail(w, 413, "Request too large")
		return
	}
	if len(body) < 5 || body[0] != 0 || binary.BigEndian.Uint32(body[1:5]) != uint32(len(body)-5) {
		detail(w, 400, "Invalid unary gRPC request")
		return
	}
	if scoped(r) && method == "acs-timeline" {
		fields, err := pulse.Fields(body[5:])
		if err != nil {
			detail(w, 400, "Invalid unary gRPC request")
			return
		}
		uid, _ := fields[1].([]byte)
		if !bytes.Equal(uid, []byte(row["pulse_id"].(string))) {
			detail(w, 403, "Only your own turnstile timeline is available")
			return
		}
	}
	secret, err := v.Secret(row)
	if err != nil {
		s.failure(w, err)
		return
	}
	cookie, _ := secret["cookie"].(string)
	if cookie == "" {
		detail(w, 409, "The session was ended; sign in again")
		return
	}
	authorization := ""
	if method == "pass-code" || method == "pass-issue" {
		authorization = r.Header.Get("X-Pulse-Token")
		s.stateMu.Lock()
		expiry := s.issued[id][postgres.Digest(authorization)]
		s.stateMu.Unlock()
		if authorization == "" || float64(expiry) <= float64(v.Now().UnixMicro())/1e6 {
			detail(w, 403, "Obtain a fresh token for this account first")
			return
		}
	}
	reply, err := s.config.Pulse.RPC(r.Context(), cookie, method, body, authorization)
	if err != nil {
		detail(w, 503, "Pulse is unavailable; the stored session was retained")
		return
	}
	if method == "profile" {
		state, uid := pulse.AuthenticationState(reply.Status, reply.Headers, reply.Body)
		if state == "valid" && uid != row["pulse_id"] {
			detail(w, 503, "Account identity mismatch")
			return
		}
		if err = v.UpdateState(r.Context(), id, state, row["version"].(int64)); err != nil {
			s.failure(w, err)
			return
		}
	}
	if method == "attendance" {
		state, message := pulse.Approval(reply.Status, reply.Headers, reply.Body)
		if err = v.RecordMark(r.Context(), id, state, message, "app"); err != nil {
			s.failure(w, err)
			return
		}
	}
	if reply.Status == 200 && (method == "pass-token" || method == "pass-issue") {
		message, status, err := pulse.Message(reply.Body, reply.Headers)
		if err == nil && status == 0 && message != nil {
			values, err := pulse.Fields(message)
			if err == nil {
				if method == "pass-token" {
					token, _ := values[1].([]byte)
					expiry, err := pulse.TokenExpiry(string(token))
					if err == nil {
						s.stateMu.Lock()
						tokens := s.issued[id]
						if tokens == nil {
							tokens = map[string]int64{}
							s.issued[id] = tokens
						}
						now := v.Now().Unix()
						for key, expires := range tokens {
							if expires <= now {
								delete(tokens, key)
							}
						}
						if len(tokens) > 100 {
							clear(tokens)
						}
						tokens[postgres.Digest(string(token))] = expiry
						s.stateMu.Unlock()
					}
				} else if nested, ok := values[1].([]byte); ok {
					fields, err := pulse.Fields(nested)
					if err == nil {
						if number, ok := fields[1].(uint64); ok && number > 0 {
							version := row["pass_version"].(int64)
							_, _ = v.SavePass(r.Context(), id, strconv.FormatUint(number, 10), &version)
						}
					}
				}
			}
		}
	}
	if reply.Status == 401 {
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(200)
		_, _ = w.Write(pulse.Frame([]byte("grpc-status: 16\r\n"), 128))
		return
	}
	for _, header := range []string{"Content-Type", "Grpc-Status", "Grpc-Message", "Location"} {
		if value := reply.Headers.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(reply.Status)
	_, _ = w.Write(reply.Body)
}

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
	s.stateMu.Lock()
	delete(s.issued, id)
	s.stateMu.Unlock()
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
	s.stateMu.Lock()
	delete(s.issued, id)
	s.stateMu.Unlock()
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
func (s *Server) publishPass(w http.ResponseWriter, r *http.Request) {
	body, ok := input(w, r)
	if !ok {
		return
	}
	version, ok := expected(w, body)
	if !ok || version == nil {
		return
	}
	id := r.PathValue("id")
	row, err := s.config.Vault.Account(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	if row == nil {
		detail(w, 404, "Account not found")
		return
	}
	if !s.requirePass(w, r, row, "publish") {
		return
	}
	number := body["number"].(string)
	valid := number != "" && number[0] >= '1' && number[0] <= '9'
	for _, c := range number {
		if c < '0' || c > '9' {
			valid = false
		}
	}
	if _, parseErr := strconv.ParseInt(number, 10, 64); parseErr != nil {
		valid = false
	}
	if !valid {
		detail(w, 400, "Invalid pass identifier")
		return
	}
	result, err := s.config.Vault.SavePass(r.Context(), id, number, version)
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, result)
}
