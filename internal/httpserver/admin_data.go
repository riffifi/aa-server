package httpserver

import (
	"aa-server/internal/cryptocompat"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"regexp"
	"strings"
)

var hiddenColumn = regexp.MustCompile(`(?i)(^|_)(secret|key_hash|csrf|password|token|cookie|capability_hash|client_hash|hash)($|_)`)

func (s *Server) debugTables(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	rows, err := s.config.Vault.Pool.Query(r.Context(), `SELECT table_name,array_agg(column_name ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema='aa' AND table_name NOT LIKE 'telegram%' AND table_name NOT IN('pass_clients','pass_requests','pass_grants') GROUP BY table_name ORDER BY table_name`)
	if err != nil {
		s.failure(w, err)
		return
	}
	type table struct {
		name    string
		columns []string
	}
	tables := []table{}
	for rows.Next() {
		var value table
		if err = rows.Scan(&value.name, &value.columns); err != nil {
			rows.Close()
			s.failure(w, err)
			return
		}
		tables = append(tables, value)
	}
	rows.Close()
	name := r.PathValue("name")
	if name == "" {
		out := []any{}
		for _, table := range tables {
			var count int64
			if err = s.config.Vault.Pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM "+pgx.Identifier{"aa", table.name}.Sanitize()).Scan(&count); err != nil {
				s.failure(w, err)
				return
			}
			out = append(out, map[string]any{"name": table.name, "columns": table.columns, "rows": count})
		}
		write(w, 200, map[string]any{"tables": out})
		return
	}
	var columns []string
	for _, table := range tables {
		if name == table.name {
			columns = table.columns
			break
		}
	}
	if columns == nil {
		detail(w, 404, "No such table")
		return
	}
	offset, ok := integer(w, r, "offset", 0)
	if !ok {
		return
	}
	limit, ok := integer(w, r, "limit", 100)
	if !ok {
		return
	}
	if offset < 0 {
		offset = 0
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	projection := []string{}
	search := []string{}
	for _, column := range columns {
		quoted := pgx.Identifier{column}.Sanitize()
		if hiddenColumn.MatchString(column) || (name == "config" && column == "value") {
			projection = append(projection, "CASE WHEN "+quoted+" IS NULL THEN NULL ELSE '•••' END AS "+quoted)
		} else {
			projection = append(projection, quoted)
			search = append(search, "CAST("+quoted+" AS TEXT) ILIKE $1")
		}
	}
	query := []rune(r.URL.Query().Get("q"))
	if len(query) > 200 {
		query = query[:200]
	}
	where := ""
	args := []any{}
	if len(query) > 0 && len(search) > 0 {
		where = " WHERE " + strings.Join(search, " OR ")
		args = append(args, "%"+string(query)+"%")
	}
	target := pgx.Identifier{"aa", name}.Sanitize()
	var total int64
	if err = s.config.Vault.Pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM "+target+where, args...).Scan(&total); err != nil {
		s.failure(w, err)
		return
	}
	args = append(args, limit, offset)
	data, err := s.config.Vault.Pool.Query(r.Context(), "SELECT "+strings.Join(projection, ",")+" FROM "+target+where+" ORDER BY 1 DESC NULLS LAST LIMIT $"+fmt.Sprint(len(args)-1)+" OFFSET $"+fmt.Sprint(len(args)), args...)
	if err != nil {
		s.failure(w, err)
		return
	}
	defer data.Close()
	out := [][]any{}
	for data.Next() {
		values, err := data.Values()
		if err != nil {
			s.failure(w, err)
			return
		}
		out = append(out, values)
	}
	if err = data.Err(); err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"name": name, "columns": columns, "total": total, "offset": offset, "rows": out})
}
func (s *Server) debugAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	limit, ok := integer(w, r, "limit", 500)
	if !ok {
		return
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 2000 {
		limit = 2000
	}
	q := []rune(r.URL.Query().Get("q"))
	if len(q) > 200 {
		q = q[:200]
	}
	rows, err := s.config.Vault.Pool.Query(r.Context(), `SELECT e.*,a.label FROM aa.audit_events e LEFT JOIN aa.accounts a ON a.id=e.account_id WHERE e.action ILIKE $1 OR e.actor ILIKE $1 OR COALESCE(e.account_id,'') ILIKE $1 ORDER BY e.id DESC LIMIT $2`, "%"+string(q)+"%", limit)
	if err != nil {
		s.failure(w, err)
		return
	}
	events, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"events": events})
}
func (s *Server) unlockPassTesting(w http.ResponseWriter, r *http.Request) {
	body, ok := DecodeBody(w, r, []Field{{Name: "password", Kind: "string", Required: true, Min: 1, Max: 256}})
	if !ok {
		return
	}
	if s.limited(w, "pass-test:"+identity(r), 8, 5*60*1000000000) {
		return
	}
	access := cryptocompat.Daily(cryptocompat.PassTestingKey([]byte(s.config.MasterKey)), s.config.Vault.Now())
	if !cryptocompat.MatchDaily(body["password"].(string), access) {
		detail(w, 401, "Wrong or expired pass testing password")
		return
	}
	if err := s.config.Vault.Audit(r.Context(), "pass_testing_unlocked", actor(r), nil); err != nil {
		s.failure(w, err)
		return
	}
	write(w, 200, map[string]any{"expires_at": access.ExpiresAt, "day": access.Day})
}
