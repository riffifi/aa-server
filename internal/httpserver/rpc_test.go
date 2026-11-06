package httpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/pulse"
)

type fakePulse struct {
	reply         pulse.Reply
	err           error
	calls         int
	authorization string
	logoutCalls   int
	identityUID   string
}

func (p *fakePulse) RPC(_ context.Context, cookie, method string, body []byte, authorization string) (pulse.Reply, error) {
	p.calls++
	p.authorization = authorization
	return p.reply, p.err
}
func (p *fakePulse) Identity(context.Context, string) (string, string) {
	if p.identityUID != "" {
		return "valid", p.identityUID
	}
	return "valid", "synthetic"
}
func (p *fakePulse) Logout(context.Context, string) error {
	p.logoutCalls++
	return errors.New("synthetic upstream unavailable")
}
func binaryRequest(s *Server, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://testserver"+path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/grpc-web+proto")
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.serveHTTP(w, r)
	return w
}
func grpcReply(message []byte) pulse.Reply {
	return pulse.Reply{Status: 200, Headers: http.Header{"Content-Type": []string{"application/grpc-web+proto"}}, Body: append(pulse.Frame(message, 0), pulse.Frame([]byte("grpc-status: 0\r\n"), 128)...)}
}

func TestRPCChecksAccountBoundTokenAndNeverLosesSessionOnOutage(t *testing.T) {
	upstream := &fakePulse{}
	s, v := setup(t, func(c *Config) { c.Pulse = upstream })
	first := account(t, v, fmt.Sprintf("token-first-%d", time.Now().UnixNano()))
	second := account(t, v, fmt.Sprintf("token-second-%d", time.Now().UnixNano()))
	id, other := first["id"].(string), second["id"].(string)
	if _, err := v.SetPassAllowed(context.Background(), id, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := v.SetPassAllowed(context.Background(), other, true, nil); err != nil {
		t.Fatal(err)
	}
	daily := cryptocompat.Daily(cryptocompat.PassTestingKey([]byte(s.config.MasterKey)), v.Now())
	headers := map[string]string{"X-Pass-Test-Key": daily.Password}
	payload, _ := json.Marshal(map[string]any{"exp": time.Now().Unix() + 600})
	token := "synthetic." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	upstream.reply = grpcReply(pulse.Field(1, []byte(token)))
	w := binaryRequest(s, "/api/accounts/"+id+"/rpc/pass-token", pulse.Frame(nil, 0), headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	headers["X-Pulse-Token"] = token
	w = binaryRequest(s, "/api/accounts/"+other+"/rpc/pass-code", pulse.Frame(nil, 0), headers)
	if w.Code != 403 {
		t.Fatal("token crossed account boundary")
	}
	upstream.reply = grpcReply(nil)
	w = binaryRequest(s, "/api/accounts/"+id+"/rpc/pass-code", pulse.Frame(nil, 0), headers)
	if w.Code != 200 || upstream.authorization != token {
		t.Fatal("issued token was not forwarded")
	}
	upstream.err = errors.New("synthetic timeout")
	w = binaryRequest(s, "/api/accounts/"+id+"/rpc/profile", pulse.Frame(nil, 0), nil)
	if w.Code != 503 {
		t.Fatal("upstream outage status changed")
	}
	fresh, err := v.Account(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := v.Secret(fresh)
	if err != nil || secret["cookie"] != "session=synthetic" {
		t.Fatal("outage destroyed stored credentials")
	}
}

func TestProfileMismatchAttendanceAndPulse401Mapping(t *testing.T) {
	upstream := &fakePulse{}
	s, v := setup(t, func(c *Config) { c.Pulse = upstream })
	row := account(t, v, fmt.Sprintf("rpc-%d", time.Now().UnixNano()))
	id := row["id"].(string)
	upstream.reply = grpcReply(pulse.Field(1, pulse.Field(1, pulse.Field(1, []byte("wrong-person")))))
	w := binaryRequest(s, "/api/accounts/"+id+"/rpc/profile", pulse.Frame(nil, 0), nil)
	if w.Code != 503 {
		t.Fatal("identity mismatch accepted")
	}
	upstream.reply = grpcReply(pulse.Field(2, pulse.Field(1, []byte("lesson"))))
	w = binaryRequest(s, "/api/accounts/"+id+"/rpc/attendance", pulse.Frame(nil, 0), nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	marks, err := v.MarksSince(context.Background(), []string{id}, 0)
	if err != nil || len(marks) != 1 || marks[0]["state"] != "approved" || marks[0]["source"] != "app" {
		t.Fatalf("app mark missing: %v", err)
	}
	upstream.reply = pulse.Reply{Status: 401, Headers: http.Header{}, Body: []byte("unauthorized")}
	w = binaryRequest(s, "/api/accounts/"+id+"/rpc/profile", pulse.Frame(nil, 0), nil)
	_, status, err := pulse.Message(w.Body.Bytes(), w.Header())
	if w.Code != 200 || err != nil || status != 16 {
		t.Fatal("Pulse and server authentication errors became indistinguishable")
	}
	w = binaryRequest(s, "/api/accounts/"+id+"/rpc/profile", []byte("invalid"), nil)
	if w.Code != 400 {
		t.Fatal("malformed frame accepted")
	}
}

func TestEndSessionRevokesLocalCredentialsEvenWhenPulseLogoutFails(t *testing.T) {
	upstream := &fakePulse{}
	s, v := setup(t, func(c *Config) { c.Pulse = upstream })
	row := account(t, v, fmt.Sprintf("end-%d", time.Now().UnixNano()))
	id := row["id"].(string)
	token, err := v.CreateSiteSession(context.Background(), "synthetic-aaid", row["pulse_id"].(string), nil, nil, 3600)
	if err != nil {
		t.Fatal(err)
	}
	w, result := request(t, s, "POST", "/api/accounts/"+id+"/end-session", "", map[string]string{"Cookie": "aa_site=" + token})
	if w.Code != 200 || result["state"] != "expired" || upstream.logoutCalls != 1 {
		t.Fatal("local revocation did not survive upstream failure")
	}
	w = binaryRequest(s, "/api/accounts/"+id+"/rpc/profile", pulse.Frame(nil, 0), nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "session was ended") {
		t.Fatal("ended session remained usable")
	}
}
