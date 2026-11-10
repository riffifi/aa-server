package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"aa-server/internal/pulse"
)

type enrollmentPulse struct {
	fakePulse
	methods   []string
	issue     []byte
	codeReply []byte
	onIssue   func()
	wrongCode bool
	malformed bool
}

func (p *enrollmentPulse) RPC(_ context.Context, cookie, method string, body []byte, authorization string) (pulse.Reply, error) {
	p.methods = append(p.methods, method)
	if cookie != "session=synthetic" {
		return pulse.Reply{}, fmt.Errorf("wrong account cookie")
	}
	if method == "pass-token" {
		if authorization != "" {
			return pulse.Reply{}, fmt.Errorf("client token reused")
		}
		return grpcReply(pulse.Field(1, []byte("server.only.token"))), nil
	}
	if authorization != "server.only.token" {
		return pulse.Reply{}, fmt.Errorf("token missing")
	}
	if method == "pass-code" {
		if len(body) != 5 {
			return pulse.Reply{}, fmt.Errorf("arbitrary request forwarded")
		}
		if p.codeReply != nil {
			return grpcReply(p.codeReply), nil
		}
		return grpcReply(pulse.Field(1, nil)), nil
	}
	if method == "pass-issue" {
		p.issue = append([]byte(nil), body...)
		if p.onIssue != nil {
			p.onIssue()
		}
		if p.malformed {
			return grpcReply(pulse.Field(1, nil)), nil
		}
		if p.wrongCode {
			return grpcReply(pulse.Field(2, nil)), nil
		}
		return grpcReply(pulse.Field(1, pulse.VField(1, 9223372036854775807))), nil
	}
	return pulse.Reply{}, fmt.Errorf("unexpected RPC %s", method)
}

func TestServerOwnedPassEnrollmentAndPermissionRevocation(t *testing.T) {
	prefix := fmt.Sprintf("digital-pass-%d-", time.Now().UnixNano())
	upstream := &enrollmentPulse{}
	s, v := setup(t, func(c *Config) {
		c.Pulse = upstream
		c.Verify = func(_ context.Context, token string) (map[string]any, error) {
			if token != "owner" && token != "guest" {
				return nil, nil
			}
			return map[string]any{"mirea": prefix + token, "sub": token}, nil
		}
	})
	ctx := context.Background()
	owner, guest := account(t, v, prefix+"owner"), account(t, v, prefix+"guest")
	id, guestID := owner["id"].(string), guest["id"].(string)
	if _, err := v.SetPassAllowed(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}
	group := int64(5330)
	for _, id := range []string{id, guestID} {
		current, err := v.MetadataAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.UpdateGroup(ctx, id, &group, nil, current["version"].(int64)); err != nil {
			t.Fatal(err)
		}
	}
	headers := map[string]string{"Authorization": "Bearer owner", "X-Pulse-Token": "untrusted"}
	for _, path := range []string{"/site/pass/code", "/site/pass/issue", "/site/passes", "/site/passes/" + id} {
		method := "GET"
		if path == "/site/pass/code" || path == "/site/pass/issue" {
			method = "POST"
		}
		w, _ := secureRequest(t, s, method, path, `{"code":"123456"}`, nil)
		if w.Code != 401 {
			t.Fatal("anonymous pass access", path, w.Code)
		}
	}
	if len(upstream.methods) != 0 {
		t.Fatal("anonymous call reached university")
	}
	w, _ := secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"12345a"}`, headers)
	if w.Code != 422 || len(upstream.methods) != 0 {
		t.Fatal(w.Body.String())
	}
	w, result := secureRequest(t, s, "POST", "/site/pass/code", `{}`, headers)
	if w.Code != 200 || result["state"] != "code_sent" {
		t.Fatal(w.Body.String())
	}
	w, result = secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"012345","account_id":"untrusted","number":"1"}`, headers)
	if w.Code != 200 || result["number"] != "9223372036854775807" {
		t.Fatal(w.Body.String())
	}
	if string(upstream.issue) != string(pulse.PassIssueRequest("012345")) {
		t.Fatal("client controlled protobuf")
	}
	w, result = secureRequest(t, s, "GET", "/site/passes/"+id, "", headers)
	if w.Code != 200 || result["number"] != "9223372036854775807" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Body.String())
	}
	guestHeaders := map[string]string{"Authorization": "Bearer guest"}
	w, _ = secureRequest(t, s, "GET", "/site/passes/"+id, "", guestHeaders)
	if w.Code != 404 {
		t.Fatal("no share leaked pass", w.Body.String())
	}
	if err := v.SharePass(ctx, id, guestID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.SetPassAllowed(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}
	w, result = secureRequest(t, s, "GET", "/site/passes/"+id, "", guestHeaders)
	if w.Code != 200 || result["number"] != "9223372036854775807" {
		t.Fatal(w.Body.String())
	}
	for _, action := range []string{"code", "issue"} {
		w, result = secureRequest(t, s, "POST", "/site/passes/"+id+"/"+action, `{"code":"012345"}`, guestHeaders)
		if w.Code != 200 {
			t.Fatal("shared enrollment", action, w.Body.String())
		}
		if action == "issue" && result["account_id"] != id {
			t.Fatal("enrolled wrong owner")
		}
	}
	before := len(upstream.methods)
	// Even a pass grant never authorizes the old arbitrary enrollment proxy.
	w, _ = secureRequest(t, s, "POST", "/api/accounts/"+id+"/rpc/pass-issue", `{}`, guestHeaders)
	if w.Code != 404 || len(upstream.methods) != before {
		t.Fatal("grant allowed enrollment")
	}
	if _, err := v.SetPassAllowed(ctx, id, false, nil); err != nil {
		t.Fatal(err)
	}
	w, _ = secureRequest(t, s, "GET", "/site/passes/"+id, "", guestHeaders)
	if w.Code != 404 {
		t.Fatal("disabled sharing leaked pass")
	}
	w, _ = secureRequest(t, s, "GET", "/site/passes/"+id, "", headers)
	if w.Code != 404 {
		t.Fatal("absolute owner switch leaked own pass")
	}
	w, _ = secureRequest(t, s, "POST", "/site/pass/code", `{}`, headers)
	if w.Code != 403 {
		t.Fatal("disabled pass allowed sending a code")
	}
	if _, err := v.SetPassAllowed(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := v.UnsharePass(ctx, id, guestID); err != nil {
		t.Fatal(err)
	}
	w, _ = secureRequest(t, s, "GET", "/site/passes/"+id, "", guestHeaders)
	if w.Code != 404 {
		t.Fatal("revoked share leaked pass")
	}
	for _, action := range []string{"code", "issue"} {
		before := len(upstream.methods)
		w, _ = secureRequest(t, s, "POST", "/site/passes/"+id+"/"+action, `{"code":"012345"}`, guestHeaders)
		if w.Code != 404 || len(upstream.methods) != before {
			t.Fatal("revoked share allowed enrollment")
		}
	}
	// A grant outside today's roster scope must not reveal names or numbers.
	foreign := int64(999999)
	currentGuest, err := v.MetadataAccount(ctx, guestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.UpdateGroup(ctx, guestID, &foreign, nil, currentGuest["version"].(int64)); err != nil {
		t.Fatal(err)
	}
	if err := v.SharePass(ctx, id, guestID); err != nil {
		t.Fatal(err)
	}
	w, _ = secureRequest(t, s, "GET", "/site/passes/"+id, "", guestHeaders)
	if w.Code != 404 {
		t.Fatal("foreign group leaked pass")
	}
	upstream.wrongCode = true
	w, _ = secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"111111"}`, headers)
	if w.Code != 422 {
		t.Fatal(w.Body.String())
	}
	upstream.wrongCode = false
	upstream.malformed = true
	w, _ = secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"111111"}`, headers)
	if w.Code != 502 {
		t.Fatal(w.Body.String())
	}
	upstream.malformed = false
	upstream.onIssue = func() {
		if _, err := v.SetPassAllowed(ctx, id, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	w, _ = secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"111111"}`, headers)
	if w.Code != 409 {
		t.Fatal("disabled pass accepted pending issuance", w.Body.String())
	}
	if _, err := v.SetPassAllowed(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}
	upstream.onIssue = func() {
		busy, _ := secureRequest(t, s, "POST", "/site/pass/code", `{}`, headers)
		if busy.Code != 409 {
			t.Fatal("concurrent pass operation was admitted")
		}
		if _, err := v.EndSession(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	w, _ = secureRequest(t, s, "POST", "/site/pass/issue", `{"code":"111111"}`, headers)
	if w.Code != 409 {
		t.Fatal("logout did not invalidate pending issuance", w.Body.String())
	}
	before = len(upstream.methods)
	w, _ = secureRequest(t, s, "POST", "/site/pass/code", `{}`, headers)
	if w.Code != 409 || len(upstream.methods) != before {
		t.Fatal("ended session reached university")
	}
}

func TestPassCodeResultsAndRateLimit(t *testing.T) {
	uid := fmt.Sprintf("pass-rate-%d", time.Now().UnixNano())
	p := &enrollmentPulse{}
	s, v := setup(t, func(c *Config) {
		c.Pulse = p
		c.Verify = func(context.Context, string) (map[string]any, error) {
			return map[string]any{"mirea": uid, "sub": uid}, nil
		}
	})
	row := account(t, v, uid)
	if _, err := v.SetPassAllowed(context.Background(), row["id"].(string), true, nil); err != nil {
		t.Fatal(err)
	}
	h := map[string]string{"Authorization": "Bearer test"}
	p.codeReply = pulse.Field(2, pulse.VField(1, 1794300000))
	w, result := secureRequest(t, s, "POST", "/site/pass/code", `{}`, h)
	if w.Code != 200 || result["state"] != "wait" || result["retry_at"] != float64(1794300000) {
		t.Fatal(w.Body.String())
	}
	p.codeReply = pulse.Field(3, nil)
	w, result = secureRequest(t, s, "POST", "/site/pass/code", `{}`, h)
	if w.Code != 200 || result["state"] != "unavailable" {
		t.Fatal(w.Body.String())
	}
	for i := 0; i < 4; i++ {
		secureRequest(t, s, "POST", "/site/pass/code", `{}`, h)
	}
	before := len(p.methods)
	w, _ = secureRequest(t, s, "POST", "/site/pass/code", `{}`, h)
	if w.Code != http.StatusTooManyRequests || before != len(p.methods) {
		t.Fatal("unbounded code sending", w.Body.String())
	}
}
