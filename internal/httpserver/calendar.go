package httpserver

import (
	"net/http"
	"strings"
	"time"

	"aa-server/internal/schedule"
)

func (s *Server) person(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if scoped(r) {
		claims, _ := r.Context().Value(principalKey{}).(map[string]any)
		group := numberAny(claims["grp"])
		var gid any
		if group > 0 {
			gid = int64(group)
		}
		return map[string]any{"mirea": identity(r), "aaid_id": claims["sub"], "group_id": gid, "group_name": claims["group_name"]}, true
	}
	session, err := s.config.Vault.Session(r.Context(), "site", cookie(r, "aa_site"))
	if err != nil {
		s.failure(w, err)
		return nil, false
	}
	if session == nil {
		detail(w, 401, "Войдите через AA ID")
		return nil, false
	}
	return session, true
}
func idValue(raw any) *int64 {
	if value, ok := raw.(int64); ok && value != 0 {
		return &value
	}
	return nil
}
func sameID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func (s *Server) weekSchedule(w http.ResponseWriter, r *http.Request) {
	requested, ok := integer(w, r, "group", 0)
	if !ok {
		return
	}
	week, ok := integer(w, r, "week", 0)
	if !ok {
		return
	}
	session, ok := s.person(w, r)
	if !ok {
		return
	}
	v := s.config.Vault
	own, err := v.MetadataByPulse(r.Context(), session["mirea"].(string))
	if err != nil {
		s.failure(w, err)
		return
	}
	ownID := idValue(session["group_id"])
	ownName := session["group_name"]
	if ownID == nil && own != nil {
		ownID = idValue(own["group_id"])
	}
	if (ownName == nil || ownName == "") && own != nil {
		ownName = own["group_name"]
	}
	groupID := ownID
	if requested > 0 {
		groupID = &requested
	}
	known := map[int64]any{}
	groups, err := v.Groups(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	for _, group := range groups {
		known[group["id"].(int64)] = group["name"]
	}
	if ownID != nil {
		if _, exists := known[*ownID]; !exists {
			known[*ownID] = ownName
		}
	}
	now := v.Now().In(schedule.Moscow)
	day := schedule.Day(now)
	weekday := (int(day.Weekday()) + 6) % 7
	offset := week
	if offset < -30 {
		offset = -30
	}
	if offset > 30 {
		offset = 30
	}
	monday := day.AddDate(0, 0, -weekday+int(offset)*7)
	var name any
	if groupID != nil {
		name = known[*groupID]
	}
	if name == nil || name == "" {
		query := []rune(strings.TrimSpace(r.URL.Query().Get("name")))
		if len(query) > 40 {
			query = query[:40]
		}
		if len(query) > 0 {
			name = string(query)
		} else {
			name = nil
		}
	}
	result := map[string]any{"group_id": groupID, "group_name": name, "own": sameID(groupID, ownID), "own_group_id": ownID, "own_group_name": ownName, "monday": monday.Format("2006-01-02"), "week": week, "days": []any{}, "schedule_error": nil}
	if groupID == nil {
		write(w, 200, result)
		return
	}
	days, available := s.config.Calendars.Week(r.Context(), *groupID, monday)
	if !available {
		result["schedule_error"] = "Расписание МИРЭА недоступно"
		write(w, 200, result)
		return
	}
	output := []any{}
	for index := 0; index < 7; index++ {
		date := monday.AddDate(0, 0, index)
		lessons := []any{}
		for _, lesson := range days[date.Format("2006-01-02")] {
			item := schedule.Shown(&lesson).(map[string]any)
			state := "future"
			if !lesson.End.After(now) {
				state = "past"
			} else if !lesson.Start.After(now) {
				state = "now"
			}
			item["state"] = state
			lessons = append(lessons, item)
		}
		output = append(output, map[string]any{"date": date.Format("2006-01-02"), "weekday": index, "today": date.Equal(day), "lessons": lessons})
	}
	result["days"] = output
	write(w, 200, result)
}
func (s *Server) searchGroups(w http.ResponseWriter, r *http.Request) {
	query := []rune(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(query) < 2 {
		write(w, 200, map[string]any{"groups": []any{}})
		return
	}
	if len(query) > 50 {
		query = query[:50]
	}
	write(w, 200, map[string]any{"groups": s.config.SearchGroups(r.Context(), string(query))})
}
func (s *Server) searchSchedule(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.person(w, r); !ok {
		return
	}
	query := []rune(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(query) < 2 {
		write(w, 200, map[string]any{"groups": []any{}})
		return
	}
	if s.limited(w, "site:schedule-search:"+remoteHost(r), 60, time.Minute) {
		return
	}
	s.searchGroups(w, r)
}
