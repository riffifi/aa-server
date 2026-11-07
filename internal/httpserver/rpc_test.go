package httpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	if w.Code != 404 {
		t.Fatal("ended session remained usable")
	}
}
