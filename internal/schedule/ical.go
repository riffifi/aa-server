// Package schedule preserves the existing Moscow civil-time calendar semantics.
package schedule

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/teambition/rrule-go"
	"golang.org/x/text/cases"
)

const Host = "https://schedule-of.mirea.ru"

var Moscow = time.FixedZone("Europe/Moscow", 3*3600)
var datePattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{4}(?:[0-9]{2})?Z?$`)
var untilUTC = regexp.MustCompile(`UNTIL=([0-9]{8}T[0-9]{6})Z`)
var foldedLine = regexp.MustCompile(`\r?\n[ \t]`)

type Lesson struct {
	Start, End                       time.Time
	Subject, Type, Location, Teacher string
}

func ICALURL(group int64) string {
	return fmt.Sprintf("%s/schedule/api/ical/Group/%d?includeMeta=true", Host, group)
}
func PageURL(group int64) string { return fmt.Sprintf("%s/?s=1_%d", Host, group) }
func Day(moment time.Time) time.Time {
	now := moment.In(Moscow)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, Moscow)
}
func parseDate(value string) (time.Time, bool) {
	if !datePattern.MatchString(value) {
		return time.Time{}, false
	}
	value = strings.TrimSuffix(value, "Z")
	layout := "20060102T150405"
	if len(value) == 13 {
		layout = "20060102T1504"
	}
	moment, err := time.ParseInLocation(layout, value, Moscow)
	return moment, err == nil && moment.Year() > 0
}
func unescape(text string) string {
	for _, pair := range [][2]string{{`\n`, "\n"}, {`\N`, "\n"}, {`\,`, ","}, {`\;`, ";"}, {`\\`, `\`}} {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}
func expand(event map[string][]string, first, end time.Time) []Lesson {
	field := func(key string) string {
		if values := event[key]; len(values) > 0 {
			return values[0]
		}
		return ""
	}
	result := []Lesson{}
	if field("TRANSP") == "TRANSPARENT" {
		return result
	}
	start, valid := parseDate(field("DTSTART"))
	if !valid {
		return result
	}
	finish, valid := parseDate(field("DTEND"))
	if !valid {
		return result
	}
	duration := finish.Sub(start)
	if duration <= 0 || duration > 24*time.Hour {
		return result
	}
	subject := field("X-META-DISCIPLINE")
	if subject == "" {
		subject = field("SUMMARY")
	}
	if subject == "" {
		return result
	}
	starts := map[time.Time]bool{}
	if rule := field("RRULE"); rule != "" {
		option, err := rrule.StrToROptionInLocation(untilUTC.ReplaceAllString(rule, "UNTIL=$1"), Moscow)
		if err != nil {
			return result
		}
		option.Dtstart = start
		recurrence, err := rrule.NewRRule(*option)
		if err != nil {
			return result
		}
		for _, value := range recurrence.Between(first, end, true) {
			starts[value] = true
		}
	} else {
		starts[start] = true
	}
	for _, raw := range event["RDATE"] {
		for _, value := range strings.Split(raw, ",") {
			if moment, ok := parseDate(value); ok {
				starts[moment] = true
			}
		}
	}
	excluded := map[time.Time]bool{}
	for _, raw := range event["EXDATE"] {
		for _, value := range strings.Split(raw, ",") {
			if moment, ok := parseDate(value); ok {
				excluded[moment] = true
			}
		}
	}
	for moment := range starts {
		if !moment.Before(first) && moment.Before(end) && !excluded[moment] {
			result = append(result, Lesson{moment, moment.Add(duration), subject, field("X-META-LESSON_TYPE"), field("LOCATION"), strings.Join(event["X-META-TEACHER"], ", ")})
		}
	}
	return result
}
func Between(source string, first time.Time, days int) map[string][]Lesson {
	first = Day(first)
	end := first.AddDate(0, 0, days)
	byDay := map[string][]Lesson{}
	for offset := 0; offset < days; offset++ {
		byDay[first.AddDate(0, 0, offset).Format("2006-01-02")] = []Lesson{}
	}
	type key struct {
		start   time.Time
		subject string
	}
	found := map[key]Lesson{}
	order := []key{}
	var event map[string][]string
	for _, raw := range strings.Split(foldedLine.ReplaceAllString(source, ""), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "BEGIN:VEVENT" {
			event = map[string][]string{}
			continue
		}
		if line == "END:VEVENT" {
			if event != nil {
				for _, lesson := range expand(event, first, end) {
					id := key{lesson.Start, lesson.Subject}
					if _, exists := found[id]; !exists {
						order = append(order, id)
					}
					found[id] = lesson
				}
			}
			event = nil
			continue
		}
		if event == nil {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name, _, _ = strings.Cut(name, ";")
		name = strings.ToUpper(name)
		event[name] = append(event[name], unescape(value))
	}
	lessons := make([]Lesson, 0, len(order))
	for _, id := range order {
		lessons = append(lessons, found[id])
	}
	sort.SliceStable(lessons, func(i, j int) bool { return lessons[i].Start.Before(lessons[j].Start) })
	for _, lesson := range lessons {
		day := lesson.Start.Format("2006-01-02")
		byDay[day] = append(byDay[day], lesson)
	}
	return byDay
}
func On(source string, day time.Time) []Lesson {
	return Between(source, day, 1)[Day(day).Format("2006-01-02")]
}
func CurrentAndNext(lessons []Lesson, now time.Time) (*Lesson, *Lesson) {
	var current, next *Lesson
	for i := range lessons {
		lesson := &lessons[i]
		if current == nil && !now.Before(lesson.Start) && now.Before(lesson.End) {
			current = lesson
		}
		if next == nil && lesson.Start.After(now) {
			next = lesson
		}
	}
	return current, next
}
func Same(a, b Lesson) bool {
	if !a.Start.Equal(b.Start) || !a.End.Equal(b.End) {
		return false
	}
	fold := cases.Fold()
	place := func(value string) string {
		return fold.String(strings.Join(strings.FieldsFunc(value, func(c rune) bool { return unicode.IsSpace(c) || c >= 0x1c && c <= 0x1f }), " "))
	}
	room := func(location string) bool {
		if location == "" {
			return false
		}
		for _, word := range []string{"дистанц", "онлайн", "сдо", "online", "remote"} {
			if strings.Contains(location, word) {
				return false
			}
		}
		return true
	}
	left, right := place(a.Location), place(b.Location)
	if room(left) || room(right) {
		return room(left) && room(right) && left == right
	}
	return fold.String(a.Subject) == fold.String(b.Subject) && fold.String(a.Teacher) == fold.String(b.Teacher)
}
func Shown(lesson *Lesson) any {
	if lesson == nil {
		return nil
	}
	return map[string]any{"subject": lesson.Subject, "type": lesson.Type, "location": lesson.Location, "teacher": lesson.Teacher, "start": lesson.Start.Format("15:04"), "end": lesson.End.Format("15:04")}
}
