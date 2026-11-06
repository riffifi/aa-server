package identityserver

import (
	"aa-server/internal/schedule"
	"net/http"
)

func (s *Server) webSchedule(w http.ResponseWriter, r *http.Request) {
	_, user, ok := s.webSession(w, r)
	if !ok {
		return
	}
	group, ok := user["group_id"].(int64)
	if !ok || group == 0 {
		write(w, 200, map[string]any{"group_id": nil, "url": schedule.Host, "current": nil, "next": nil})
		return
	}
	source, _ := s.config.Calendars.SourceWait(r.Context(), group)
	if source == "" {
		detail(w, 503, "Расписание МИРЭА недоступно")
		return
	}
	now := s.config.Store.Now().In(schedule.Moscow)
	current, next := schedule.CurrentAndNext(schedule.On(source, now), now)
	shown := func(lesson *schedule.Lesson) any {
		item := schedule.Shown(lesson)
		if fields, ok := item.(map[string]any); ok {
			delete(fields, "teacher")
		}
		return item
	}
	write(w, 200, map[string]any{"group_id": group, "url": schedule.PageURL(group), "current": shown(current), "next": shown(next)})
}
