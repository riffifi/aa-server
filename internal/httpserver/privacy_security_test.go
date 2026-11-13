package httpserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"aa-server/internal/postgres"
	"aa-server/internal/schedule"
)

func TestDayRosterAndAttendanceAreScopedPerLesson(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 10, 45, 0, 0, schedule.Moscow)
	calendar := schedule.NewCache(ctx, func(_ context.Context, url string) (string, error) {
		subject := "Shared"
		if strings.Contains(url, "/Group/990003?") {
			subject = "Different"
		}
		first := "BEGIN:VEVENT\nDTSTART:20261106T090000\nDTEND:20261106T103000\nSUMMARY:" + subject + "\nLOCATION:A-101\nX-META-TEACHER:Teacher\nEND:VEVENT\n"
		second := "BEGIN:VEVENT\nDTSTART:20261106T104000\nDTEND:20261106T121000\nSUMMARY:Private\nLOCATION:A-102\nX-META-TEACHER:Teacher\nEND:VEVENT\n"
		if !strings.Contains(url, "/Group/990001?") {
			second = ""
		}
		return "BEGIN:VCALENDAR\n" + first + second + "END:VCALENDAR", nil
	})
	defer calendar.Close()
	s, v := setup(t, func(c *Config) { c.Calendars = calendar; c.Pulse = &sitePulse{} })
	v.Now = func() time.Time { return now }
	prefix := fmt.Sprintf("privacy-%d-", time.Now().UnixNano())
	ids := []string{}
	for i := 0; i < 3; i++ {
		gid := int64(990001 + i)
		row, err := v.Upsert(ctx, postgres.Upsert{PulseID: prefix + strconv.Itoa(i), Label: prefix + strconv.Itoa(i), GroupID: &gid, Credentials: map[string]any{"cookie": "synthetic"}})
		if err != nil {
			t.Fatal(err)
		}
		id := row["id"].(string)
		ids = append(ids, id)
		t.Cleanup(func() { _ = v.DeleteAccount(ctx, id) })
	}
	username := "hidden" + strconv.FormatInt(time.Now().UnixNano(), 10)[6:]
	if err := v.SetUsername(ctx, ids[2], &username); err != nil {
		t.Fatal(err)
	}
	if _, err := v.SetPassAllowed(ctx, ids[2], true, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.SharePass(ctx, ids[2], ids[0]); err != nil {
		t.Fatal(err)
	}
	gid := int64(990001)
	token, err := v.CreateSiteSession(ctx, "privacy-user", prefix+"0", &gid, nil, 3600)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "aa_site=" + token}
	w, found := secureRequest(t, s, "GET", "/api/me/people?q="+username, "", headers)
	if w.Code != 200 || len(found["people"].([]any)) != 0 {
		t.Fatal("exact username bypassed shared-class scope", w.Body.String())
	}
	w, snapshot := secureRequest(t, s, "GET", "/api/accounts", "", headers)
	if w.Code != 200 || len(snapshot["accounts"].([]any)) != 1 || strings.Contains(w.Body.String(), ids[2]) {
		t.Fatal("old grant bypassed class scope", w.Body.String())
	}
	shared := time.Date(2026, 11, 6, 9, 0, 0, 0, schedule.Moscow)
	private := time.Date(2026, 11, 6, 10, 40, 0, 0, schedule.Moscow)
	for _, start := range []time.Time{shared, private} {
		stamp := float64(start.Unix())
		saved := now
		now = start.Add(-25 * time.Minute)
		if err := v.RecordLessonMark(ctx, ids[1], "approved", "private sentinel", "test", &stamp); err != nil {
			t.Fatal(err)
		}
		now = saved
	}
	w, today := secureRequest(t, s, "GET", "/site/today", "", headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(today["people"].([]any)) != 2 || strings.Contains(w.Body.String(), ids[2]) {
		t.Fatal("unrelated roster exposed", w.Body.String())
	}
	lessons := today["lessons"].([]any)
	if lessons[0].(map[string]any)["marks"].(map[string]any)[ids[1]] == nil {
		t.Fatal("shared attendance missing")
	}
	if _, ok := lessons[1].(map[string]any)["marks"].(map[string]any)[ids[1]]; ok {
		t.Fatal("private attendance exposed")
	}
	for _, person := range today["people"].([]any) {
		p := person.(map[string]any)
		if p["id"] == ids[1] {
			if _, ok := p["session"]; ok {
				t.Fatal("foreign session state exposed")
			}
		}
	}
	// The shared class ended 15 minutes ago; the next class is in progress.
	w, state := secureRequest(t, s, "GET", "/site/state?lesson_start="+strconv.FormatInt(shared.Unix(), 10), "", headers)
	if w.Code != 200 || len(state["people"].([]any)) != 2 {
		t.Fatal("previous class unavailable", w.Body.String())
	}
	for _, p := range state["people"].([]any) {
		row := p.(map[string]any)
		if row["id"] == ids[1] && row["mark"] == nil {
			t.Fatal("early mark omitted", w.Body.String())
		}
	}
	w, state = secureRequest(t, s, "GET", "/site/state?lesson_start="+strconv.FormatInt(private.Unix(), 10), "", headers)
	if w.Code != 200 || len(state["people"].([]any)) != 1 {
		t.Fatal("private class exposed peers", w.Body.String())
	}
	now = shared.Add(2*time.Hour + time.Second) // end + 30m + 1s
	w, _ = secureRequest(t, s, "GET", "/site/state?lesson_start="+strconv.FormatInt(shared.Unix(), 10), "", headers)
	if w.Code != 409 {
		t.Fatal("expired selection accepted", w.Body.String())
	}
	for _, offset := range []time.Duration{-30 * time.Minute, 2 * time.Hour} {
		if !lessonWindow(schedule.Lesson{Start: shared, End: shared.Add(90 * time.Minute)}, shared.Add(offset).Unix()) {
			t.Fatal("inclusive boundary rejected")
		}
	}
	for _, offset := range []time.Duration{-30*time.Minute - time.Second, 2*time.Hour + time.Second} {
		if lessonWindow(schedule.Lesson{Start: shared, End: shared.Add(90 * time.Minute)}, shared.Add(offset).Unix()) {
			t.Fatal("outside boundary accepted")
		}
	}
}

func TestMainAPIChecksLiveIdentityAndCurrentGroup(t *testing.T) {
	ctx := context.Background()
	claims := map[string]any{}
	s, v := setup(t, func(c *Config) {
		c.RequireIdentityState = true
		c.Verify = func(context.Context, string) (map[string]any, error) {
			copy := map[string]any{}
			for k, x := range claims {
				copy[k] = x
			}
			return copy, nil
		}
	})
	identity := postgres.NewIdentity(v.Store)
	pulseID := fmt.Sprintf("live-%d", time.Now().UnixNano())
	gid := int64(991001)
	user, err := identity.BindMirea(ctx, pulseID, &gid, nil)
	if err != nil {
		t.Fatal(err)
	}
	uid := user["id"].(string)
	session, _, err := identity.CreateSession(ctx, uid, "test", 3600, 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	sid := session["id"].(string)
	t.Cleanup(func() {
		for _, q := range []string{"DELETE FROM aaid.web_tokens WHERE session_id IN(SELECT id FROM aaid.sessions WHERE user_id=$1)", "DELETE FROM aaid.refresh_tokens WHERE session_id IN(SELECT id FROM aaid.sessions WHERE user_id=$1)", "DELETE FROM aaid.audit WHERE user_id=$1", "DELETE FROM aaid.sessions WHERE user_id=$1", "DELETE FROM aaid.mirea_links WHERE user_id=$1", "DELETE FROM aaid.users WHERE id=$1"} {
			if _, err := v.Pool.Exec(ctx, q, uid); err != nil {
				t.Error(err)
			}
		}
	})
	claims = map[string]any{"sub": uid, "mirea": pulseID, "sid": sid, "grp": int64(123)}
	token, err := v.CreateSiteSession(ctx, uid, pulseID, &gid, nil, 3600)
	if err != nil {
		t.Fatal(err)
	}
	cookie := map[string]string{"Cookie": "aa_site=" + token}
	bearer := map[string]string{"Authorization": "Bearer test"}
	w, _ := secureRequest(t, s, "GET", "/site/state", "", cookie)
	if w.Code != 401 {
		t.Fatal("unbound old cookie accepted")
	}
	if err := v.BindSiteIdentity(ctx, token, sid); err != nil {
		t.Fatal(err)
	}
	for _, h := range []map[string]string{cookie, bearer} {
		w, data := secureRequest(t, s, "GET", "/site/state", "", h)
		if w.Code != 200 || data["me"].(map[string]any)["group_id"] != float64(gid) {
			t.Fatal("stale group trusted", w.Body.String())
		}
	}
	if _, err := v.Pool.Exec(ctx, "UPDATE aaid.mirea_links SET group_id=NULL,group_name=NULL WHERE user_id=$1", uid); err != nil {
		t.Fatal(err)
	}
	w, data := secureRequest(t, s, "GET", "/site/state", "", bearer)
	if w.Code != 200 || data["me"].(map[string]any)["group_id"] != nil {
		t.Fatal("removed group restored from client", w.Body.String())
	}
	if _, err := v.Pool.Exec(ctx, "UPDATE aaid.users SET status='blocked' WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	for _, h := range []map[string]string{cookie, bearer} {
		w, _ := secureRequest(t, s, "GET", "/api/accounts", "", h)
		if w.Code != 403 {
			t.Fatal("blocked identity accepted", w.Body.String())
		}
	}
	if _, err := v.Pool.Exec(ctx, "UPDATE aaid.users SET status='active' WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Pool.Exec(ctx, "UPDATE aaid.sessions SET revoked_at=EXTRACT(EPOCH FROM CURRENT_TIMESTAMP) WHERE id=$1", sid); err != nil {
		t.Fatal(err)
	}
	for _, h := range []map[string]string{cookie, bearer} {
		w, _ := secureRequest(t, s, "GET", "/api/accounts", "", h)
		if w.Code != 401 {
			t.Fatal("revoked session accepted", w.Body.String())
		}
	}
}
