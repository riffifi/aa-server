package httpserver

import (
	"aa-server/internal/postgres"
	"net/http"
	"sort"
)

func (s *Server) visibleAccounts(w http.ResponseWriter, r *http.Request) {
	gid, ok := group(w, r)
	if !ok {
		return
	}
	v := s.config.Vault
	scope, allowed := s.rosterScope(w, r)
	if !allowed {
		return
	}
	rows, err := v.VisibleMetadata(r.Context(), identity(r))
	if err != nil {
		s.failure(w, err)
		return
	}
	// The existing app rejects a snapshot containing a different selected group.
	if gid != nil {
		filtered := []map[string]any{}
		for _, row := range rows {
			if row["group_id"] == *gid {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	accounts := []any{}
	for _, row := range rows {
		if !scope.allows(row) {
			continue
		}
		data := postgres.Public(row)
		own := row["pulse_id"] == identity(r)
		data["own"] = own
		access := "approved"
		if row["pass_allowed"] == int64(0) {
			access = "disabled"
		}
		data["pass_access"] = access
		if !own {
			for _, key := range []string{"expires_at", "checked_at", "updated_at"} {
				delete(data, key)
			}
		}
		accounts = append(accounts, data)
	}
	cursor, err := v.Cursor(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"accounts": accounts, "cursor": cursor, "protocol": 2, "sync": "automatic", "group_filter": gid})
}
func (s *Server) visibleGroups(w http.ResponseWriter, r *http.Request) {
	scope, allowed := s.rosterScope(w, r)
	if !allowed {
		return
	}
	rows, err := s.config.Vault.VisibleMetadata(r.Context(), identity(r))
	if err != nil {
		s.failure(w, err)
		return
	}
	known := map[int64]map[string]any{}
	for _, row := range rows {
		if !scope.allows(row) {
			continue
		}
		group, ok := row["group_id"].(int64)
		if !ok {
			continue
		}
		entry := known[group]
		if entry == nil {
			entry = map[string]any{"id": group, "name": row["group_name"], "accounts": 0}
			known[group] = entry
		}
		entry["accounts"] = entry["accounts"].(int) + 1
	}
	groups := []map[string]any{}
	for _, value := range known {
		groups = append(groups, value)
	}
	sort.Slice(groups, func(i, j int) bool {
		left, _ := groups[i]["name"].(string)
		right, _ := groups[j]["name"].(string)
		if left == right {
			return groups[i]["id"].(int64) < groups[j]["id"].(int64)
		}
		return left < right
	})
	write(w, 200, map[string]any{"groups": groups})
}
