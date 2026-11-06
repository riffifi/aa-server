package schedule

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type normalizedLesson struct{ Start, End, Subject, Type, Location, Teacher string }

func normalized(lesson Lesson) normalizedLesson {
	return normalizedLesson{lesson.Start.Format("2006-01-02T15:04:05"), lesson.End.Format("2006-01-02T15:04:05"), lesson.Subject, lesson.Type, lesson.Location, lesson.Teacher}
}
func fromNormalized(t *testing.T, raw normalizedLesson) Lesson {
	t.Helper()
	start, err := time.ParseInLocation("2006-01-02T15:04:05", raw.Start, Moscow)
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.ParseInLocation("2006-01-02T15:04:05", raw.End, Moscow)
	if err != nil {
		t.Fatal(err)
	}
	return Lesson{start, end, raw.Subject, raw.Type, raw.Location, raw.Teacher}
}
func TestPythonCalendarRecurrenceAndLessonMatching(t *testing.T) {
	var document struct {
		Cases []struct {
			Source, First string
			Days          int
			Expected      map[string][]normalizedLesson
		}
		Pairs []struct {
			A, B     normalizedLesson
			Expected bool
		}
	}
	data, err := os.ReadFile("testdata/python_calendar.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for index, item := range document.Cases {
		first, err := time.ParseInLocation("2006-01-02", item.First, Moscow)
		if err != nil {
			t.Fatal(err)
		}
		actual := map[string][]normalizedLesson{}
		for day, lessons := range Between(item.Source, first, item.Days) {
			out := []normalizedLesson{}
			for _, lesson := range lessons {
				out = append(out, normalized(lesson))
			}
			actual[day] = out
		}
		if !reflect.DeepEqual(actual, item.Expected) {
			t.Fatalf("calendar %d differs: actual=%#v expected=%#v", index, actual, item.Expected)
		}
	}
	for index, pair := range document.Pairs {
		if actual := Same(fromNormalized(t, pair.A), fromNormalized(t, pair.B)); actual != pair.Expected {
			t.Fatalf("class matching changed for pair %d", index)
		}
	}
}
func TestStaleCalendarRefreshIsSharedAndDoesNotHoldReaders(t *testing.T) {
	var calls atomic.Int64
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	started, release := make(chan struct{}), make(chan struct{})
	cache := NewCache(context.Background(), func(ctx context.Context, _ string) (string, error) {
		n := calls.Add(1)
		if n == 2 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if n == 1 {
			return "BEGIN:VCALENDAR\nold\nEND:VCALENDAR", nil
		}
		return "BEGIN:VCALENDAR\nnew\nEND:VCALENDAR", nil
	})
	defer cache.Close()
	cache.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
	old, _ := cache.Source(context.Background(), 1)
	clock.Add(901)
	stale, _ := cache.Source(context.Background(), 1)
	if stale != old {
		t.Fatal("saved calendar was not served immediately")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}
	for range 10 {
		if text, _ := cache.Source(context.Background(), 1); text != old {
			t.Fatal("refresh held or changed saved data")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("duplicate group download")
	}
	cache.mu.Lock()
	done := cache.flights[1].Done
	cache.mu.Unlock()
	close(release)
	<-done
	fresh, _ := cache.Source(context.Background(), 1)
	if fresh == old {
		t.Fatal("new calendar was not published")
	}
}
func TestCancelledFirstReaderDoesNotCancelSharedDownload(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	cache := NewCache(context.Background(), func(ctx context.Context, _ string) (string, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return "BEGIN:VCALENDAR\nEND:VCALENDAR", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	go func() { cache.Source(ctx, 1); close(first) }()
	<-started
	cancel()
	<-first
	close(release)
	text, _ := cache.Source(context.Background(), 1)
	if text == "" || calls.Load() != 1 {
		t.Fatal("reader cancellation destroyed shared work")
	}
}
func TestCalendarCacheBoundsNegativeEntriesAndClosesWorkers(t *testing.T) {
	cache := NewCache(context.Background(), func(context.Context, string) (string, error) { return "", nil })
	cache.MaxEntries = 2
	for id := int64(1); id <= 10; id++ {
		cache.Source(context.Background(), id)
	}
	cache.mu.Lock()
	count := len(cache.entries)
	cache.mu.Unlock()
	if count > 2 {
		t.Fatal("negative cache grew without bound")
	}
	cache.Close()
	if text, _ := cache.Source(context.Background(), 20); text != "" {
		t.Fatal("closed cache accepted new work")
	}
}
