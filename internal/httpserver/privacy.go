package httpserver

import (
	"aa-server/internal/schedule"
	"context"
	"net/http"
)

type rosterScope struct {
	owner  string
	groups []int64
}
type rosterScopeKey struct{}

func (p *rosterScope) allows(row map[string]any) bool {
	return row != nil && (row["pulse_id"] == p.owner || containsGroup(p.groups, row["group_id"]))
}

// Names are visible for the day only when the groups have a shared lesson today.
// Permission grants are intersected with this scope, never used to widen it.
func (s *Server) rosterScope(w http.ResponseWriter, r *http.Request) (*rosterScope, bool) {
	if cached, ok := r.Context().Value(rosterScopeKey{}).(*rosterScope); ok {
		return cached, true
	}
	claims, _ := r.Context().Value(principalKey{}).(map[string]any)
	var person map[string]any
	var owner string
	if claims != nil {
		owner, _ = claims["mirea"].(string)
		var gid any
		if n := numberAny(claims["grp"]); n > 0 {
			gid = int64(n)
		}
		person = map[string]any{"mirea": owner, "group_id": gid, "group_name": claims["group_name"], "group_authoritative": s.config.RequireIdentityState}
	} else {
		var ok bool
		person, ok = s.person(w, r)
		if !ok {
			return nil, false
		}
		owner, _ = person["mirea"].(string)
	}
	c, err := s.siteContext(r.Context(), person)
	if err != nil {
		s.failure(w, err)
		return nil, false
	}
	p := &rosterScope{owner: owner, groups: []int64{}}
	if c.group != nil {
		p.groups = append(p.groups, *c.group)
	}
	for _, lesson := range c.lessons {
		for _, gid := range c.groups(&lesson) {
			if !containsGroup(p.groups, gid) {
				p.groups = append(p.groups, gid)
			}
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), rosterScopeKey{}, p))
	return p, true
}

func lessonWindow(lesson schedule.Lesson, now int64) bool {
	return now >= lesson.Start.Unix()-1800 && now <= lesson.End.Unix()+1800
}

func chooseLesson(c *siteContext, requested int64) bool {
	if requested == 0 {
		return true
	}
	for i := range c.lessons {
		lesson := &c.lessons[i]
		if lesson.Start.Unix() == requested && lessonWindow(*lesson, c.now.Unix()) {
			c.active = lesson
			return true
		}
	}
	return false
}

func markBelongs(mark map[string]any, lesson schedule.Lesson, foreign bool) bool {
	if start := mark["lesson_start"]; start != nil {
		return numberAny(start) == float64(lesson.Start.Unix())
	}
	// A legacy timestamp alone does not prove which class produced a foreign
	// mark. Hide it rather than accidentally disclose a different class.
	if foreign {
		return false
	}
	begin := lesson.Start.Unix()
	if !foreign {
		begin -= 900
	}
	created := numberAny(mark["created"])
	return created >= float64(begin) && created < float64(lesson.End.Unix())
}
