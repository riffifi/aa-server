package schedule

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestDifferentCalendarsCannotCreateUnboundedDownloads(t *testing.T) {
	var started atomic.Int64
	cache := NewCache(context.Background(), func(ctx context.Context, _ string) (string, error) {
		started.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	})
	defer cache.Close()
	// Cancel readers immediately; shared downloads keep running until cache.Close.
	reader, cancel := context.WithCancel(context.Background())
	cancel()
	for id := int64(1); id <= 1000; id++ {
		cache.Source(reader, id)
	}
	deadline := time.Now().Add(time.Second)
	for started.Load() < 16 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != 16 {
		t.Fatal("download bound differs", got)
	}
	cache.mu.Lock()
	flights := len(cache.flights)
	cache.mu.Unlock()
	if flights != 16 {
		t.Fatal("unbounded pending flights", flights)
	}
}

func TestSharedLessonFailsClosedForAmbiguousOrDifferentClasses(t *testing.T) {
	start := time.Date(2026, 11, 6, 9, 0, 0, 0, Moscow)
	base := Lesson{Start: start, End: start.Add(90 * time.Minute), Subject: "Physics", Teacher: "Teacher", Location: "A-101", Type: "Lecture"}
	if !SharedLesson(base, base) {
		t.Fatal("identical class rejected")
	}
	for index := 0; index < 4; index++ {
		other := base
		switch index {
		case 0:
			other.Subject = "Math"
		case 1:
			other.Teacher = "Another teacher"
		case 2:
			other.Teacher = ""
		case 3:
			other.Type = "Lab"
		}
		if SharedLesson(base, other) {
			t.Fatal(fmt.Sprintf("different or ambiguous class %d accepted", index))
		}
	}
}
