package httpserver

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestOwnerRequestsSharingAndRevocationPreservePersonBoundAccess(t *testing.T) {
	suffix := fmt.Sprint(time.Now().UnixNano())
	uids := map[string]string{"owner": "owner-" + suffix, "guest": "guest-" + suffix, "other": "other-" + suffix}
	s, v := setup(t, func(c *Config) {
		c.Verify = func(_ context.Context, token string) (map[string]any, error) {
			uid, ok := uids[token]
			if !ok {
				return nil, nil
			}
			return map[string]any{"sub": "aaid-" + uid, "mirea": uid, "usr": token + suffix}, nil
		}
	})
	owner := account(t, v, uids["owner"])
	guest := account(t, v, uids["guest"])
	_ = account(t, v, uids["other"])
	ownerID, guestID := owner["id"].(string), guest["id"].(string)
	if _, err := v.SavePass(context.Background(), ownerID, "987654321", nil); err != nil {
		t.Fatal(err)
	}
	ownerHeaders := map[string]string{"Authorization": "Bearer owner"}
	guestHeaders := map[string]string{"Authorization": "Bearer guest"}
	otherHeaders := map[string]string{"Authorization": "Bearer other"}
	w, settings := request(t, s, "PUT", "/api/me/account", `{"label":"Owner changed","pass_allowed":true}`, ownerHeaders)
	if w.Code != 200 || settings["label"] != "Owner changed" || settings["pass_allowed"] != true {
		t.Fatal(w.Body.String())
	}
	if w, _ = request(t, s, "GET", "/api/me/account", "", guestHeaders); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w, people := request(t, s, "GET", "/api/me/people?q=@guest"+suffix, "", ownerHeaders)
	if w.Code != 200 || len(people["people"].([]any)) != 1 || people["people"].([]any)[0].(map[string]any)["account_id"] != guestID {
		t.Fatal("signed username search changed")
	}
	body := `{"account_id":"` + ownerID + `"}`
	w, pending := request(t, s, "POST", "/api/me/pass/requests", body, guestHeaders)
	if w.Code != 200 || len(pending["requests"].([]any)) != 1 {
		t.Fatal(w.Body.String())
	}
	requestID := pending["requests"].([]any)[0].(map[string]any)["id"].(string)
	w, again := request(t, s, "POST", "/api/me/pass/requests", body, guestHeaders)
	if w.Code != 200 || again["requests"].([]any)[0].(map[string]any)["id"] != requestID {
		t.Fatal("duplicate pending request created")
	}
	w, _ = request(t, s, "POST", "/api/me/pass/requests/"+requestID+"/allow", "", otherHeaders)
	if w.Code != 404 {
		t.Fatal("another person approved owner's request")
	}
	w, allowed := request(t, s, "POST", "/api/me/pass/requests/"+requestID+"/allow", "", ownerHeaders)
	if w.Code != 200 || len(allowed["people"].([]any)) != 1 || allowed["people"].([]any)[0].(map[string]any)["account_id"] != guestID {
		t.Fatal(w.Body.String())
	}
	w, snapshot := request(t, s, "GET", "/api/accounts?group_id=999999", "", guestHeaders)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	found := false
	for _, raw := range snapshot["passes"].([]any) {
		pass := raw.(map[string]any)
		if pass["account_id"] == ownerID {
			found = true
			if pass["number"] != "987654321" {
				t.Fatal("allowed pass was not included outside group filter")
			}
		}
	}
	if !found {
		t.Fatal("shared account disappeared behind group filter")
	}
	w, _ = request(t, s, "DELETE", "/api/me/pass/people/aaid/"+guestID, "", ownerHeaders)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w, snapshot = request(t, s, "GET", "/api/accounts?group_id=999999", "", guestHeaders)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, raw := range snapshot["passes"].([]any) {
		if raw.(map[string]any)["account_id"] == ownerID {
			t.Fatal("revoked account remained accessible across groups")
		}
	}
	w, pending = request(t, s, "POST", "/api/me/pass/requests", body, guestHeaders)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	requestID = pending["requests"].([]any)[0].(map[string]any)["id"].(string)
	w, _ = request(t, s, "POST", "/api/me/pass/requests/"+requestID+"/deny", "", ownerHeaders)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w, _ = request(t, s, "POST", "/api/me/pass/requests", body, guestHeaders)
	if w.Code != 409 {
		t.Fatal("denial cooldown was not enforced")
	}
}
