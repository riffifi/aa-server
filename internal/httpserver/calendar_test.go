package httpserver

import (
	"aa-server/internal/schedule"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestScheduleSitePermissionsWeeksAndCalendarFailure(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 30, 0, 0, schedule.Moscow)
	calls := 0
	cache := schedule.NewCache(context.Background(), func(context.Context, string) (string, error) {
		calls++
		return "BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20261103T090000\nDTEND:20261103T103000\nSUMMARY:Математика\nEND:VEVENT\nEND:VCALENDAR", nil
	})
	defer cache.Close()
	s, v := setup(t, func(c *Config) {
		c.Calendars = cache
		c.SearchGroups = func(_ context.Context, q string) []map[string]any {
			return []map[string]any{{"id": int64(5330), "name": q, "full": "Группа"}}
		}
	})
	v.Now = func() time.Time { return now }
	group := int64(5330)
	name := "Группа"
	token, err := v.CreateSiteSession(context.Background(), "schedule-test", fmt.Sprintf("schedule-%d", time.Now().UnixNano()), &group, &name, 3600)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Cookie": "aa_site=" + token}
	w, _ := request(t, s, "GET", "/site/schedule", "", nil)
	if w.Code != 401 || calls != 0 {
		t.Fatal("anonymous request fetched a calendar")
	}
	w, result := request(t, s, "GET", "/site/schedule", "", headers)
	if w.Code != 200 || result["monday"] != "2026-11-02" || result["own"] != true || result["group_name"] != name {
		t.Fatal(w.Body.String())
	}
	days := result["days"].([]any)
	if len(days) != 7 {
		t.Fatal("week must include empty days")
	}
	lesson := days[1].(map[string]any)["lessons"].([]any)[0].(map[string]any)
	if lesson["state"] != "now" || lesson["start"] != "09:00" || lesson["teacher"] != "" {
		t.Fatal(lesson)
	}
	w, result = request(t, s, "GET", "/site/schedule?week=999", "", headers)
	if result["week"] != float64(999) || result["monday"] != schedule.Day(now).AddDate(0, 0, -1+30*7).Format("2006-01-02") {
		t.Fatal(w.Body.String())
	}
	w, result = request(t, s, "GET", "/api/groups/search?q="+strings.Repeat("a", 60), "", nil)
	if w.Code != 200 || len(result["groups"].([]any)[0].(map[string]any)["name"].(string)) != 50 {
		t.Fatal(w.Body.String())
	}
	failed := schedule.NewCache(context.Background(), func(context.Context, string) (string, error) { return "", nil })
	defer failed.Close()
	s.config.Calendars = failed
	w, result = request(t, s, "GET", "/site/schedule", "", headers)
	if w.Code != 200 || result["schedule_error"] != "Расписание МИРЭА недоступно" || len(result["days"].([]any)) != 0 {
		t.Fatal(w.Body.String())
	}
}
