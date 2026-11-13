package httpserver

import (
	"github.com/jackc/pgx/v5"
	"net/http"
)

func (s *Server) setBan(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, true); !ok {
		return
	}
	body, ok := DecodeBody(w, r, []Field{{Name: "mirea_id", Kind: "string", Required: true, Min: 1, Max: 256, Pattern: `^[A-Za-z0-9_.:@-]+$`}, {Name: "blocked", Kind: "bool", Required: true}, {Name: "reason", Kind: "string", Max: 500}})
	if !ok {
		return
	}
	reason, _ := body["reason"].(string)
	if _, err := s.config.Vault.Pool.Exec(r.Context(), "SELECT aa.set_mirea_ban($1,$2,$3)", body["mirea_id"], body["blocked"], reason); err != nil {
		s.failure(w, err)
		return
	}
	s.changed()
	write(w, 200, map[string]any{"ok": true})
}
func (s *Server) banList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	rows, err := s.config.Vault.Pool.Query(r.Context(), "SELECT * FROM aa.mirea_bans ORDER BY updated_at DESC,pulse_uid LIMIT 500")
	if err != nil {
		s.failure(w, err)
		return
	}
	values, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"bans": values})
}
func (s *Server) usageStatistics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	before, ok := integer(w, r, "before", 0)
	if !ok {
		return
	}
	if before < 0 {
		detail(w, 400, "Invalid cursor")
		return
	}
	uid, kind := r.URL.Query().Get("mirea_id"), r.URL.Query().Get("kind")
	if len(uid) > 256 || len(kind) > 64 {
		detail(w, 400, "Invalid filter")
		return
	}
	events, err := s.config.Vault.UsageList(r.Context(), before, uid, kind)
	if err != nil {
		s.failure(w, err)
		return
	}
	var next int64
	if len(events) == 200 {
		next = events[len(events)-1]["id"].(int64)
	}
	rows, err := s.config.Vault.Pool.Query(r.Context(), `SELECT kind,count(*) AS count FROM aa.usage_events WHERE kind IN ('attendance','login','pass_number_read','pass_enrolled','moderation') AND created>=$1 AND ($2='' OR actor_mirea=$2 OR target_mirea=$2) AND ($3='' OR kind=$3) GROUP BY kind ORDER BY kind`, float64(s.config.Vault.Now().Unix()-86400), uid, kind)
	if err != nil {
		s.failure(w, err)
		return
	}
	summary, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"events": events, "next_before": next, "summary_24h": summary})
}
