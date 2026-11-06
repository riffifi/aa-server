package identityserver

import (
	"aa-server/internal/schedule"
	"context"
	"errors"
	"testing"
	"time"
)

func TestWebScheduleOmitsTeacherAndKeepsStaleCalendarOnFailure(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 30, 0, 0, schedule.Moscow)
	calls := 0
	cache := schedule.NewCache(context.Background(), func(context.Context, string) (string, error) {
		calls++
		if calls > 1 {
			return "", errors.New("offline")
		}
		return "BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20261103T090000\nDTEND:20261103T103000\nSUMMARY:Математика\nX-META-TEACHER:Иванов\nEND:VEVENT\nBEGIN:VEVENT\nDTSTART:20261103T120000\nDTEND:20261103T133000\nSUMMARY:Физика\nEND:VEVENT\nEND:VCALENDAR", nil
	})
	defer cache.Close()
	cache.Retry = 0
	cache.Now = func() time.Time { return now }
	s, store, uid := setup(t, func(c *Config) { c.Calendars = cache })
	store.Now = func() time.Time { return now }
	group := int64(5330)
	user, err := store.BindMirea(context.Background(), uid, &group, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := store.CreateSession(context.Background(), user["id"].(string), "calendar", 3600, 1, "web")
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "aaid_web=" + token}
	w, _ := request(t, s, "GET", "/web/schedule", "", nil)
	if w.Code != 401 || calls != 0 {
		t.Fatal("anonymous request fetched a calendar")
	}
	w, result := request(t, s, "GET", "/web/schedule", "", headers)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	current := result["current"].(map[string]any)
	if current["subject"] != "Математика" || result["next"].(map[string]any)["start"] != "12:00" {
		t.Fatal(w.Body.String())
	}
	if _, exists := current["teacher"]; exists {
		t.Fatal("AA ID response gained a teacher field")
	}
	now = now.Add(900 * time.Second)
	request(t, s, "GET", "/web/schedule", "", headers)
	if calls != 1 {
		t.Fatal("inclusive 900 second TTL changed")
	}
	now = now.Add(time.Second)
	w, result = request(t, s, "GET", "/web/schedule", "", headers)
	if w.Code != 200 || result["current"] == nil || calls != 2 {
		t.Fatal("failed refresh discarded saved calendar", w.Body.String())
	}
	request(t, s, "GET", "/web/schedule", "", headers)
	if calls != 3 {
		t.Fatal("AA ID failed refresh must retry on next read")
	}
}
