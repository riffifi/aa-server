package httpserver

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"aa-server/internal/postgres"
	"aa-server/internal/schedule"
)

func TestLimiterCapsKeyCountWithoutDroppingLongWindows(t *testing.T) {
	now := time.Now()
	s := &Server{config: Config{Vault: &postgres.Vault{Now: func() time.Time { return now }}}, attempts: map[string][]time.Time{}, attemptWindows: map[string]time.Duration{}}
	for index := 0; index < 8192; index++ {
		key := fmt.Sprint(index)
		s.attempts[key] = []time.Time{now}
		s.attemptWindows[key] = time.Hour
	}
	now = now.Add(11 * time.Minute)
	w := httptest.NewRecorder()
	if !s.limited(w, "new", 5, time.Minute) || w.Code != 429 || len(s.attempts) != 8192 {
		t.Fatal("key limit or long window bypassed")
	}
	now = now.Add(time.Hour)
	if s.limited(httptest.NewRecorder(), "new", 5, time.Minute) || len(s.attempts) != 1 {
		t.Fatal("expired keys were not reclaimed")
	}
}

func TestForeignJournalRequiresExactTimeAndSubject(t *testing.T) {
	start := time.Date(2026, 11, 6, 9, 0, 0, 0, schedule.Moscow)
	lesson := &schedule.Lesson{Start: start, End: start.Add(90 * time.Minute), Subject: "Physics"}
	person := map[string]any{"group_id": int64(1), "pulse_id": "private"}
	row := journalLesson{Start: float64(start.Unix()), End: float64(lesson.End.Unix()), Subject: "  PHYSICS  ", Status: map[string]string{"private": "present"}}
	for _, change := range []string{"none", "subject", "start", "end"} {
		other := row
		switch change {
		case "subject":
			other.Subject = "Math"
		case "start":
			other.Start++
		case "end":
			other.End++
		}
		result := journalStatusFor(map[int64]journalDay{1: {Lessons: []journalLesson{other}}}, person, lesson, true)
		if change == "none" && result != "present" {
			t.Fatal("identical journal class rejected")
		}
		if change != "none" && result != nil {
			t.Fatal("private journal class exposed", change)
		}
	}
}
