package httpserver

import (
	"aa-server/internal/postgres"
	"aa-server/internal/pulse"
	"aa-server/internal/schedule"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sitePulse struct{ marks atomic.Int64 }

func (p *sitePulse) RPC(ctx context.Context, cookie, method string, body []byte, authorization string) (pulse.Reply, error) {
	if method == "attendance" {
		p.marks.Add(1)
		return grpcReply(pulse.Field(2, pulse.Field(1, []byte("lesson")))), nil
	}
	return grpcReply(nil), nil
}
func (p *sitePulse) Identity(context.Context, string) (string, string) { return "unavailable", "" }
func (p *sitePulse) Logout(context.Context, string) error              { return nil }
func TestSiteClassScopeMarkAndToday(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 30, 0, 0, schedule.Moscow)
	upstream := &sitePulse{}
	calendars := schedule.NewCache(context.Background(), func(_ context.Context, url string) (string, error) {
		room := "А-101"
		if strings.Contains(url, "/Group/900003?") {
			room = "Б-202"
		}
		return "BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20261106T090000\nDTEND:20261106T103000\nSUMMARY:Физика\nLOCATION:" + room + "\nEND:VEVENT\nEND:VCALENDAR", nil
	})
	defer calendars.Close()
	var ownerUID string
	s, v := setup(t, func(c *Config) {
		c.Pulse = upstream
		c.Calendars = calendars
		c.Verify = func(_ context.Context, token string) (map[string]any, error) {
			if token != "app" {
				return nil, nil
			}
			return map[string]any{"mirea": ownerUID, "sub": "site-owner", "grp": int64(900001)}, nil
		}
	})
	v.Now = func() time.Time { return now }
	prefix := fmt.Sprintf("site-scope-%d-", time.Now().UnixNano())
	ownerUID = prefix + "0"
	ids := []string{}
	for i := 0; i < 3; i++ {
		gid := int64(900001 + i)
		name := "ГР-" + strconv.Itoa(i)
		row, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: prefix + strconv.Itoa(i), Label: name, GroupID: &gid, GroupName: &name, Credentials: map[string]any{"cookie": "session=" + strconv.Itoa(i)}})
		if err != nil {
			t.Fatal(err)
		}
		id := row["id"].(string)
		ids = append(ids, id)
		t.Cleanup(func() { _ = v.DeleteAccount(context.Background(), id) })
	}
	group := int64(900001)
	token, err := v.CreateSiteSession(context.Background(), "site-owner", prefix+"0", &group, nil, 3600)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "aa_site=" + token}
	w, state := secureRequest(t, s, "GET", "/site/state", "", headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	people := state["people"].([]any)
	if len(people) != 2 {
		t.Fatal("class scope differs", w.Body.String())
	}
	for _, person := range people {
		if person.(map[string]any)["id"] == ids[2] {
			t.Fatal("other room exposed")
		}
	}
	app := map[string]string{"Authorization": "Bearer app"}
	for _, path := range []string{"/site/state", "/site/today", "/site/schedule", "/site/schedule/search?q=group"} {
		webResponse, webData := secureRequest(t, s, "GET", path, "", headers)
		appResponse, appData := secureRequest(t, s, "GET", path, "", app)
		if webResponse.Code != 200 || appResponse.Code != 200 || !reflect.DeepEqual(webData, appData) {
			t.Fatalf("website/app mismatch %s: web=%s app=%s", path, webResponse.Body, appResponse.Body)
		}
	}
	qr := "https://attendance-app.mirea.ru/selfapprove?token=synthetic"
	body := fmt.Sprintf(`{"qr":%q,"account_ids":[%q,%q]}`, qr, ids[1], ids[2])
	w, _ = secureRequest(t, s, "POST", "/site/mark", body, app)
	if w.Code != 403 || upstream.marks.Load() != 0 {
		t.Fatal("cross-class mark reached upstream")
	}
	body = fmt.Sprintf(`{"qr":%q,"account_ids":[%q,%q,%q]}`, qr, ids[0], ids[1], ids[1])
	w, result := secureRequest(t, s, "POST", "/site/mark", body, app)
	if w.Code != 200 || len(result["results"].([]any)) != 2 || upstream.marks.Load() != 2 {
		t.Fatal(w.Body.String())
	}
	w, today := secureRequest(t, s, "GET", "/site/today", "", headers)
	if w.Code != 200 || len(today["people"].([]any)) != 2 {
		t.Fatal(w.Body.String())
	}
	lesson := today["lessons"].([]any)[0].(map[string]any)
	if lesson["marks"].(map[string]any)[ids[0]].(map[string]any)["state"] != "approved" {
		t.Fatal("successful mark not shown", w.Body.String())
	}
	w, _ = secureRequest(t, s, "POST", "/site/mark", `{"qr":"https://evil.example/?token=x","own_group":true}`, headers)
	if w.Code != http.StatusBadRequest {
		t.Fatal("foreign QR accepted")
	}
}
