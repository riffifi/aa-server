package httpserver

import (
	"aa-server/internal/pulse"
	"aa-server/internal/schedule"
	"net/http"
	"sort"
	"strings"
)

func (s *Server) attendance(w http.ResponseWriter, r *http.Request) {
	my := r.URL.Path == "/api/me/attendance"
	days := int64(1)
	if my {
		var ok bool
		days, ok = integer(w, r, "days", 7)
		if !ok {
			return
		}
		if days < 1 || days > 14 {
			detail(w, 400, "Invalid period")
			return
		}
	} else if strings.HasPrefix(r.URL.Path, "/panel/") {
		if _, ok := s.panel(w, r, false); !ok {
			return
		}
	} else {
		allowed, err := s.owner(r, r.PathValue("id"))
		if err != nil {
			s.failure(w, err)
			return
		}
		if !allowed {
			detail(w, 401, "Sign in to this account first")
			return
		}
	}
	var id string
	if my {
		own, ok := s.ownAccount(w, r, true)
		if !ok {
			return
		}
		id = own["id"].(string)
	} else {
		id = r.PathValue("id")
	}
	v := s.config.Vault
	row, err := v.Account(r.Context(), id)
	if err != nil {
		s.failure(w, err)
		return
	}
	if row == nil {
		detail(w, 404, "Account not found")
		return
	}
	secret, err := v.Secret(row)
	if err != nil {
		s.failure(w, err)
		return
	}
	cookie, _ := secret["cookie"].(string)
	if cookie == "" {
		detail(w, 409, "The session was ended; sign in again")
		return
	}
	today := schedule.Day(v.Now()).Unix()
	events := []map[string]any{}
	inside := false
	successful := 0
	for day := int64(0); day < days; day++ {
		start := today - day*86400
		reply, err := s.pulseRPC(r.Context(), row, "acs-timeline", pulse.ACSTimelineRequest(row["pulse_id"].(string), uint64(start), uint64(start+86399)), "")
		if err != nil || reply.Status != 200 {
			continue
		}
		successful++
		entries, here := pulse.ACSEvents(reply.Body, reply.Headers)
		if day == 0 {
			inside = here
		}
		events = append(events, entries...)
	}
	if successful == 0 {
		detail(w, 503, "Pulse is unavailable; try again")
		return
	}
	if !my {
		write(w, 200, map[string]any{"inside": inside, "events": events, "since": today})
		return
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i]["time"].(uint64) > events[j]["time"].(uint64) })
	var lastIn, lastOut any
	for _, event := range events {
		if lastIn == nil && event["direction"] == "in" {
			lastIn = event
		}
		if lastOut == nil && event["direction"] == "out" {
			lastOut = event
		}
	}
	if len(events) > 30 {
		events = events[:30]
	}
	write(w, 200, map[string]any{"account_id": id, "state": row["state"], "inside": inside, "last_in": lastIn, "last_out": lastOut, "events": events})
}
func (s *Server) profileFields(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	row, err := s.config.Vault.Account(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return
	}
	if row == nil {
		detail(w, 404, "Account not found")
		return
	}
	secret, err := s.config.Vault.Secret(row)
	if err != nil {
		s.failure(w, err)
		return
	}
	cookie, _ := secret["cookie"].(string)
	if cookie == "" {
		detail(w, 409, "The session was ended; sign in again")
		return
	}
	reply, err := s.pulseRPC(r.Context(), row, "profile", pulse.ProfileRequest(), "")
	if err != nil {
		detail(w, 503, "Pulse is unavailable")
		return
	}
	payload, status, err := pulse.Message(reply.Body, reply.Headers)
	if err != nil {
		detail(w, 502, "Could not parse the Pulse profile response")
		return
	}
	tree := map[string]any{}
	if payload != nil {
		tree = pulse.DecodeTree(payload)
	}
	write(w, 200, map[string]any{"grpc_status": status, "fields": tree})
}
