package httpserver

import (
	"aa-server/internal/pulse"
	"aa-server/internal/schedule"
	"context"
	"fmt"
	"golang.org/x/text/cases"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type journalLesson struct {
	ID      string
	Start   float64
	End     float64
	Subject string
	Status  map[string]string
}
type journalDay struct {
	Fetched float64
	Error   any
	Lessons []journalLesson
}
type siteService struct {
	s        *Server
	journals *siteMemo[string, journalDay]
	presence *siteMemo[string, *bool]
}

func (s *Server) initSite(ctx context.Context) {
	service := &siteService{s: s}
	service.journals = newSiteMemo(ctx, service.readJournal)
	service.presence = newSiteMemo(ctx, func(ctx context.Context, id string) *bool {
		row, err := s.config.Vault.Account(ctx, id)
		if err != nil || row == nil {
			return nil
		}
		secret, err := s.config.Vault.Secret(row)
		if err != nil {
			return nil
		}
		cookie, _ := secret["cookie"].(string)
		if cookie == "" {
			return nil
		}
		start := schedule.Day(s.config.Vault.Now()).Unix()
		reply, err := s.pulseRPC(ctx, row, "acs-timeline", pulse.ACSTimelineRequest(row["pulse_id"].(string), uint64(start), uint64(start+86399)), "")
		if err != nil || reply.Status != 200 {
			return nil
		}
		_, inside := pulse.ACSEvents(reply.Body, reply.Headers)
		return &inside
	})
	s.site = service
}
func (s *siteService) readJournal(ctx context.Context, key string) journalDay {
	out := journalDay{Fetched: float64(s.s.config.Vault.Now().UnixMilli()) / 1000, Error: "Нет подключённых аккаунтов группы", Lessons: []journalLesson{}}
	previous, _ := s.journals.peek(key)
	known := map[string]journalLesson{}
	for _, lesson := range previous.Lessons {
		known[lesson.ID] = lesson
	}
	parts := strings.Split(key, "/")
	gid, _ := strconv.ParseInt(parts[0], 10, 64)
	day, err := time.ParseInLocation("2006-01-02", parts[1], schedule.Moscow)
	if err != nil {
		return out
	}
	rows, err := s.s.config.Vault.Metadata(ctx, &gid)
	if err != nil {
		return out
	}
	attempts := 0
	for _, meta := range rows {
		if meta["state"] != "valid" {
			continue
		}
		attempts++
		if attempts > 2 {
			break
		}
		row, err := s.s.config.Vault.Account(ctx, meta["id"].(string))
		if err != nil || row == nil {
			continue
		}
		secret, err := s.s.config.Vault.Secret(row)
		if err != nil {
			continue
		}
		cookie, _ := secret["cookie"].(string)
		if cookie == "" {
			continue
		}
		rpc := func(method string, body []byte) (pulse.Reply, bool) {
			reply, err := s.s.pulseRPC(ctx, row, method, body, "")
			return reply, err == nil && reply.Status == 200
		}
		reply, ok := rpc("visiting-logs", pulse.Frame(nil, 0))
		if !ok {
			out.Error = "Пульс не отдал журнал группы"
			continue
		}
		logs := pulse.Logs(reply.Body, reply.Headers)
		if len(logs) == 0 {
			out.Error = "Пульс не отдал журнал группы"
			continue
		}
		reply, ok = rpc("day-lessons", pulse.DayLessonsRequest(uint64(day.Year()), uint64(day.Month()), uint64(day.Day())))
		if !ok {
			out.Error = "Пульс не отдал журнал группы"
			continue
		}
		lessons := pulse.Lessons(reply.Body, reply.Headers)
		if lessons == nil {
			out.Error = "Пульс не отдал журнал группы"
			continue
		}
		journal := logs[0]
		for _, candidate := range logs {
			if numberAny(candidate["start"]) <= out.Fetched && numberAny(candidate["end"]) >= out.Fetched {
				journal = candidate
				break
			}
		}
		for _, lesson := range lessons {
			status := map[string]string{}
			start := numberAny(lesson["start"])
			old := known[lesson["id"].(string)]
			if len(old.Status) > 0 && numberAny(lesson["end"]) < previous.Fetched-3600 {
				status = old.Status
			} else if start <= out.Fetched+1800 {
				reply, ok = rpc("lesson-attendance", pulse.LessonAttendanceRequest(lesson["id"].(string), journal["id"].(string)))
				if ok {
					status = pulse.LessonStatus(reply.Body, reply.Headers)
				}
			}
			subject, _ := lesson["subject"].(string)
			out.Lessons = append(out.Lessons, journalLesson{ID: lesson["id"].(string), Start: start, End: numberAny(lesson["end"]), Subject: subject, Status: status})
		}
		out.Error = nil
		break
	}
	return out
}
func numberAny(value any) float64 {
	if value, ok := value.(uint64); ok {
		return float64(value)
	}
	return numberFloat(value)
}

type siteContext struct {
	authorizationFresh    bool
	own                   map[string]any
	group                 *int64
	name                  any
	now                   time.Time
	lessons               []schedule.Lesson
	current, next, active *schedule.Lesson
	known                 map[int64]any
	days                  map[int64][]schedule.Lesson
	rows                  []map[string]any
	error                 any
}

func (s *Server) siteContext(ctx context.Context, session map[string]any) (*siteContext, error) {
	v := s.config.Vault
	own, err := v.MetadataByPulse(ctx, session["mirea"].(string))
	if err != nil {
		return nil, err
	}
	result := &siteContext{own: own, group: idValue(session["group_id"]), name: session["group_name"], now: v.Now().In(schedule.Moscow), known: map[int64]any{}, days: map[int64][]schedule.Lesson{}, rows: []map[string]any{}}
	if result.group == nil && own != nil && session["group_authoritative"] != true {
		result.group = idValue(own["group_id"])
	}
	if (result.name == nil || result.name == "") && own != nil && sameID(result.group, idValue(own["group_id"])) {
		result.name = own["group_name"]
	}
	if result.group == nil {
		return result, nil
	}
	result.rows, err = v.Metadata(ctx, result.group)
	if err != nil {
		return nil, err
	}
	lessons, available := s.config.Calendars.Lessons(ctx, *result.group, result.now)
	if !available {
		result.error = "Расписание МИРЭА недоступно"
		return result, nil
	}
	result.lessons = lessons
	freshOwn, fresh := s.config.Calendars.FreshLessons(ctx, *result.group, result.now)
	result.authorizationFresh = fresh
	if fresh {
		lessons = freshOwn
		result.lessons = freshOwn
	}
	result.current, result.next = schedule.CurrentAndNext(lessons, result.now)
	result.active = result.current
	if result.active == nil && result.next != nil && result.next.Start.Sub(result.now) <= 30*time.Minute {
		result.active = result.next
	}
	if result.active == nil {
		for i := len(lessons) - 1; i >= 0; i-- {
			if lessonWindow(lessons[i], result.now.Unix()) {
				result.active = &result.lessons[i]
				break
			}
		}
	}
	groups, err := v.Groups(ctx)
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		result.known[group["id"].(int64)] = group["name"]
	}
	if _, ok := result.known[*result.group]; !ok {
		result.known[*result.group] = result.name
	}
	result.days[*result.group] = lessons
	jobs := make(chan int64)
	var workers sync.WaitGroup
	var dataMu sync.Mutex
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for gid := range jobs {
				their, fresh := s.config.Calendars.FreshLessons(ctx, gid, result.now)
				dataMu.Lock()
				if fresh {
					result.days[gid] = their
				}
				dataMu.Unlock()
			}
		}()
	}
send:
	for gid := range result.known {
		if gid == *result.group {
			continue
		}
		select {
		case jobs <- gid:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	if result.active != nil {
		result.rows, err = v.MetadataGroups(ctx, result.groups(result.active))
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (c *siteContext) groups(lesson *schedule.Lesson) []int64 {
	out := []int64{}
	if c.group == nil || lesson == nil {
		return out
	}
	out = append(out, *c.group)
	if !c.authorizationFresh {
		return out
	}
	other := []int64{}
	for gid, lessons := range c.days {
		if gid == *c.group {
			continue
		}
		for _, their := range lessons {
			if schedule.SharedLesson(*lesson, their) {
				other = append(other, gid)
				break
			}
		}
	}
	sort.Slice(other, func(i, j int) bool { return other[i] < other[j] })
	return append(out, other...)
}
func containsGroup(groups []int64, value any) bool {
	id, ok := value.(int64)
	if !ok {
		return false
	}
	for _, group := range groups {
		if group == id {
			return true
		}
	}
	return false
}
func (s *Server) siteSession(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if scoped(r) {
		return s.person(w, r)
	}
	if r.URL.Path == "/api/me/today" {
		claims, _ := r.Context().Value(principalKey{}).(map[string]any)
		if claims == nil {
			detail(w, 401, "AA ID sign-in required")
			return nil, false
		}
		group := numberAny(claims["grp"])
		var gid any
		if group > 0 {
			gid = int64(group)
		}
		return map[string]any{"mirea": claims["mirea"], "aaid_id": claims["sub"], "group_id": gid, "group_name": nil}, true
	}
	return s.person(w, r)
}
func (s *Server) journalFor(ctx context.Context, c *siteContext, groups []int64) map[int64]journalDay {
	found := map[int64]journalDay{}
	for _, gid := range groups {
		value, ok := s.site.journals.get(ctx, fmt.Sprintf("%d/%s", gid, c.now.Format("2006-01-02")))
		if ok {
			found[gid] = value
		}
	}
	return found
}
func journalStatus(found map[int64]journalDay, row map[string]any, lesson *schedule.Lesson) any {
	return journalStatusFor(found, row, lesson, false)
}

func journalStatusFor(found map[int64]journalDay, row map[string]any, lesson *schedule.Lesson, strict bool) any {
	gid, _ := row["group_id"].(int64)
	for _, theirs := range found[gid].Lessons {
		delta := theirs.Start - float64(lesson.Start.Unix())
		if strict && (theirs.End != float64(lesson.End.Unix()) || normalizedSubject(theirs.Subject) == "" || normalizedSubject(theirs.Subject) != normalizedSubject(lesson.Subject)) {
			continue
		}
		if (strict && delta == 0) || (!strict && delta >= -600 && delta <= 600) {
			if value, ok := theirs.Status[row["pulse_id"].(string)]; ok {
				return value
			}
		}
	}
	return nil
}

func normalizedSubject(subject string) string {
	return strings.Join(strings.Fields(cases.Fold().String(subject)), " ")
}
