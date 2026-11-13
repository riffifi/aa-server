package httpserver

import (
	"aa-server/internal/pulse"
	"aa-server/internal/schedule"
	"golang.org/x/text/cases"
	"net/http"
	"sort"
	"sync"
	"time"
)

func (s *Server) siteToday(w http.ResponseWriter, r *http.Request) {
	session, ok := s.siteSession(w, r)
	if !ok {
		return
	}
	if s.limited(w, "today:"+session["mirea"].(string), 30, time.Minute) {
		return
	}
	c, err := s.siteContext(r.Context(), session)
	if err != nil {
		s.failure(w, err)
		return
	}
	result := map[string]any{"date": c.now.Format("02.01.2006"), "group_id": c.group, "group_name": c.name, "lessons": []any{}, "people": []any{}, "schedule_error": c.error}
	if c.group == nil || c.error != nil {
		write(w, 200, result)
		return
	}
	present := []int64{*c.group}
	columns := [][]int64{}
	for _, lesson := range c.lessons {
		groups := c.groups(&lesson)
		columns = append(columns, groups)
		for _, gid := range groups {
			if !containsGroup(present, gid) {
				present = append(present, gid)
			}
		}
	}
	rows := []map[string]any{}
	ids := []string{}
	dayRows, err := s.config.Vault.MetadataGroups(r.Context(), present)
	if err != nil {
		s.failure(w, err)
		return
	}
	for _, row := range dayRows {
		if containsGroup(present, row["group_id"]) {
			rows = append(rows, row)
			ids = append(ids, row["id"].(string))
		}
	}
	marks, err := s.config.Vault.MarksSince(r.Context(), ids, float64(schedule.Day(c.now).Unix()-1800))
	if err != nil {
		s.failure(w, err)
		return
	}
	journals := s.journalFor(r.Context(), c, present)
	meta := map[int64]any{}
	for gid, entry := range journals {
		meta[gid] = map[string]any{"fetched": entry.Fetched, "error": entry.Error}
	}
	result["mirea"] = meta
	lessons := []any{}
	foreign := map[string]bool{}
	for _, row := range rows {
		foreign[row["id"].(string)] = c.group != nil && row["group_id"] != *c.group
	}
	for i, lesson := range c.lessons {
		cells := map[string]any{}
		for _, mark := range marks {
			if markBelongs(mark, lesson, foreign[mark["account_id"].(string)]) {
				id := mark["account_id"].(string)
				previous, _ := cells[id].(map[string]any)
				if previous["state"] != "approved" {
					cells[id] = map[string]any{"state": mark["state"], "message": mark["message"], "created": mark["created"], "source": mark["source"]}
				}
			}
		}
		studentMarks, journalMarks := map[string]any{}, map[string]any{}
		for _, row := range rows {
			if containsGroup(columns[i], row["group_id"]) {
				id := row["id"].(string)
				studentMarks[id] = cells[id]
				journalMarks[id] = journalStatusFor(journals, row, &lesson, foreign[id])
			}
		}
		shown := schedule.Shown(&lesson).(map[string]any)
		shown["state"] = lessonState(lesson, c.now)
		groups := []any{}
		for _, gid := range columns[i] {
			groups = append(groups, map[string]any{"id": gid, "name": c.known[gid]})
		}
		shown["groups"] = groups
		shown["marks"] = studentMarks
		shown["mirea"] = journalMarks
		lessons = append(lessons, shown)
	}
	result["lessons"] = lessons
	people := []map[string]any{}
	for _, row := range rows {
		person := sitePerson(c, row)
		if foreign[row["id"].(string)] {
			delete(person, "session")
		}
		people = append(people, person)
	}
	sortPeople(people, c.group, false)
	result["people"] = people
	write(w, 200, result)
}
func lessonState(lesson schedule.Lesson, now time.Time) string {
	if !lesson.End.After(now) {
		return "past"
	}
	if !lesson.Start.After(now) {
		return "now"
	}
	return "future"
}
func sitePerson(c *siteContext, row map[string]any) map[string]any {
	name := row["group_name"]
	if name == nil || name == "" {
		gid, _ := row["group_id"].(int64)
		name = c.known[gid]
	}
	return map[string]any{"id": row["id"], "label": row["label"], "username": row["username"], "group_id": row["group_id"], "group_name": name, "session": row["state"], "own": c.own != nil && row["id"] == c.own["id"]}
}
func sortPeople(people []map[string]any, group *int64, ownFirst bool) {
	fold := cases.Fold()
	sort.SliceStable(people, func(i, j int) bool {
		a, b := people[i], people[j]
		if ownFirst && a["own"] != b["own"] {
			return a["own"] == true
		}
		ownA, ownB := group != nil && a["group_id"] == *group, group != nil && b["group_id"] == *group
		if ownA != ownB {
			return ownA
		}
		if !ownFirst {
			left, _ := a["group_name"].(string)
			right, _ := b["group_name"].(string)
			if left != right {
				return left < right
			}
		}
		return fold.String(a["label"].(string)) < fold.String(b["label"].(string))
	})
}
func (s *Server) siteState(w http.ResponseWriter, r *http.Request) {
	session, ok := s.siteSession(w, r)
	if !ok {
		return
	}
	c, err := s.siteContext(r.Context(), session)
	if err != nil {
		s.failure(w, err)
		return
	}
	requested, valid := integer(w, r, "lesson_start", 0)
	if !valid {
		return
	}
	if !chooseLesson(c, requested) {
		detail(w, 409, "Lesson is outside the permitted 30 minute window")
		return
	}
	if c.active != nil {
		c.rows, err = s.config.Vault.MetadataGroups(r.Context(), c.groups(c.active))
		if err != nil {
			s.failure(w, err)
			return
		}
	}
	people := []map[string]any{}
	groups := []any{}
	activeGroups := c.groups(c.active)
	ids := []string{}
	rows := []map[string]any{}
	for _, gid := range activeGroups {
		groups = append(groups, map[string]any{"id": gid, "name": c.known[gid], "own": c.group != nil && gid == *c.group})
	}
	for _, row := range c.rows {
		if containsGroup(activeGroups, row["group_id"]) {
			rows = append(rows, row)
			ids = append(ids, row["id"].(string))
		}
	}
	if c.active != nil {
		allMarks, err := s.config.Vault.MarksSince(r.Context(), ids, float64(c.active.Start.Unix()-1800))
		if err != nil {
			s.failure(w, err)
			return
		}
		foreign := map[string]bool{}
		for _, row := range rows {
			foreign[row["id"].(string)] = c.group != nil && row["group_id"] != *c.group
		}
		marks := map[string]map[string]any{}
		for _, mark := range allMarks {
			id := mark["account_id"].(string)
			if markBelongs(mark, *c.active, foreign[id]) && marks[id]["state"] != "approved" {
				marks[id] = mark
			}
		}
		journals := s.journalFor(r.Context(), c, activeGroups)
		for _, row := range rows {
			person := sitePerson(c, row)
			person["inside"] = nil
			if !foreign[row["id"].(string)] {
				here, _ := s.site.presence.get(r.Context(), row["id"].(string))
				person["inside"] = here
			}
			var mark any
			if value := marks[row["id"].(string)]; value != nil {
				mark = map[string]any{"state": value["state"], "message": value["message"], "created": value["created"]}
			}
			person["mark"] = mark
			person["mirea"] = journalStatusFor(journals, row, c.active, c.group != nil && row["group_id"] != *c.group)
			people = append(people, person)
		}
	}
	sortPeople(people, c.group, true)
	var accountID, label, active any
	connected := false
	if c.own != nil {
		accountID, label = c.own["id"], c.own["label"]
		connected = c.own["state"] == "valid"
	}
	if c.active != nil {
		active = "soon"
		if !c.active.Start.After(c.now) {
			active = "now"
		}
		if !c.active.End.After(c.now) {
			active = "recent"
		}
	}
	had := false
	for _, lesson := range c.lessons {
		if !lesson.End.After(c.now) {
			had = true
		}
	}
	size := 0
	for _, row := range c.rows {
		if c.group != nil && row["group_id"] == *c.group && row["state"] == "valid" {
			size++
		}
	}
	var aaid any
	if s.config.AAIDURL != "" {
		aaid = s.config.AAIDURL
	}
	available := []any{}
	for _, lesson := range c.lessons {
		if lessonWindow(lesson, c.now.Unix()) {
			item := schedule.Shown(&lesson).(map[string]any)
			item["lesson_start"] = lesson.Start.Unix()
			available = append(available, item)
		}
	}
	var chosen any
	if c.active != nil {
		item := schedule.Shown(c.active).(map[string]any)
		item["lesson_start"] = c.active.Start.Unix()
		item["starts_at"] = c.active.Start.Unix()
		item["ends_at"] = c.active.End.Unix()
		chosen = item
	}
	write(w, 200, map[string]any{"me": map[string]any{"group_id": c.group, "group_name": c.name, "account_id": accountID, "label": label, "connected": connected}, "lesson": schedule.Shown(c.current), "next": schedule.Shown(c.next), "selected_lesson": chosen, "available_lessons": available, "active": active, "had_lessons": had, "own_group_size": size, "schedule_url": "/schedule", "schedule_error": c.error, "groups": groups, "people": people, "aaid_url": aaid})
}
func (s *Server) siteMark(w http.ResponseWriter, r *http.Request) {
	body, ok := DecodeBody(w, r, []Field{{Name: "qr", Kind: "string", Required: true, Min: 1, Max: 8192}, {Name: "account_ids", Kind: "strings", Nullable: true, Min: 1, Max: 200}, {Name: "own_group", Kind: "bool"}, {Name: "lesson_start", Kind: "int", Min: 0}})
	if !ok {
		return
	}
	session, ok := s.siteSession(w, r)
	if !ok {
		return
	}
	if s.limited(w, "mark:"+session["mirea"].(string), 60, time.Minute) {
		return
	}
	token, err := pulse.AttendanceToken(body["qr"].(string))
	if err != nil {
		detail(w, 400, "Это не QR-код посещаемости Пульса")
		return
	}
	c, err := s.siteContext(r.Context(), session)
	if err != nil {
		s.failure(w, err)
		return
	}
	own, _ := body["own_group"].(bool)
	requested, _ := body["lesson_start"].(int64)
	if !chooseLesson(c, requested) {
		detail(w, 409, "Lesson is outside the permitted 30 minute window")
		return
	}
	if c.active != nil {
		c.rows, err = s.config.Vault.MetadataGroups(r.Context(), c.groups(c.active))
		if err != nil {
			s.failure(w, err)
			return
		}
	}
	groups := c.groups(c.active)
	if own {
		if c.group == nil {
			detail(w, 409, "Группа не определена")
			return
		}
		groups = []int64{*c.group}
	} else if c.active == nil {
		detail(w, 409, "Сейчас нет пары")
		return
	}
	allowed := map[string]map[string]any{}
	for _, row := range c.rows {
		if containsGroup(groups, row["group_id"]) {
			allowed[row["id"].(string)] = row
		}
	}
	chosen, _ := body["account_ids"].([]string)
	if len(chosen) == 0 {
		if !own {
			detail(w, 422, "Выберите людей")
			return
		}
		for _, row := range c.rows {
			if allowed[row["id"].(string)] != nil && row["state"] == "valid" {
				chosen = append(chosen, row["id"].(string))
			}
		}
		if len(chosen) == 0 {
			detail(w, 409, "В вашей группе нет подключённых людей")
			return
		}
	}
	unique := []string{}
	seen := map[string]bool{}
	for _, id := range chosen {
		if row := allowed[id]; row != nil {
			if err := s.config.Vault.RequireUnblocked(r.Context(), row["pulse_id"].(string)); err != nil {
				s.failure(w, err)
				return
			}
		}
		if allowed[id] == nil {
			message := "Отмечать можно только людей на вашей паре"
			if own {
				message = "Отмечать можно только свою группу"
			}
			detail(w, 403, message)
			return
		}
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	results := make([]map[string]any, len(unique))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				id := unique[index]
				state, message := "expired", "Сессия МИРЭА завершена"
				row, err := s.config.Vault.Account(r.Context(), id)
				if err == nil && row != nil {
					secret, err := s.config.Vault.Secret(row)
					if err == nil {
						cookie, _ := secret["cookie"].(string)
						if cookie != "" {
							reply, err := s.pulseRPC(r.Context(), row, "attendance", pulse.Frame(pulse.Field(1, []byte(token)), 0), "")
							if err != nil {
								state, message = "error", "Пульс недоступен"
							} else {
								state, message = pulse.Approval(reply.Status, reply.Headers, reply.Body)
							}
						}
					}
				}
				results[index] = map[string]any{"id": id, "label": allowed[id]["label"], "state": state, "message": message}
			}
		}()
	}
	for index := range unique {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	actor, _ := session["aaid_id"].(string)
	for _, result := range results {
		target := allowed[result["id"].(string)]
		details := map[string]any{"result": result["state"], "lesson_start": nil, "lesson_end": nil, "subject": nil}
		if c.active != nil {
			details["lesson_start"] = c.active.Start.Unix()
			details["lesson_end"] = c.active.End.Unix()
			details["subject"] = c.active.Subject
		}
		if err := s.config.Vault.Usage(r.Context(), "attendance", session["mirea"].(string), target["pulse_id"].(string), result["id"].(string), details); err != nil {
			s.failure(w, err)
			return
		}
		var lessonStart *float64
		if c.active != nil {
			start := float64(c.active.Start.Unix())
			lessonStart = &start
		}
		if err := s.config.Vault.RecordLessonMark(r.Context(), result["id"].(string), result["state"].(string), result["message"].(string), "site:"+actor, lessonStart); err != nil {
			s.failure(w, err)
			return
		}
	}
	write(w, 200, map[string]any{"results": results})
}
