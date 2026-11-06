package httpserver

import (
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type requestRecord struct {
	Time   float64 `json:"time"`
	Method string  `json:"method"`
	Route  string  `json:"route"`
	Path   string  `json:"path"`
	Status int     `json:"status"`
	MS     float64 `json:"ms"`
}
type requestTotals struct {
	Route   string  `json:"route"`
	Count   int64   `json:"count"`
	Errors  int64   `json:"errors"`
	Total   float64 `json:"total_ms"`
	Max     float64 `json:"max_ms"`
	Last    float64 `json:"last"`
	Average float64 `json:"avg_ms"`
}
type diagnosticState struct {
	mu      sync.Mutex
	since   time.Time
	recent  []requestRecord
	routes  map[string]requestTotals
	logs    []serverLog
	nextLog int64
}
type serverLog struct {
	ID      int64   `json:"id"`
	Time    float64 `json:"time"`
	Level   string  `json:"level"`
	Logger  string  `json:"logger"`
	Message string  `json:"message"`
	Detail  string  `json:"detail"`
}
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(body)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	wrapped := &statusWriter{ResponseWriter: w}
	defer func() {
		_, route := s.mux.Handler(r)
		if route == "" || (route == "GET /" && r.URL.Path != "/") {
			route = "unknown"
		}
		if _, path, ok := strings.Cut(route, " "); ok {
			route = path
		}
		method := r.Method
		switch method {
		case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		default:
			method = "OTHER"
		}
		status := wrapped.status
		if status == 0 {
			status = 200
		}
		record := requestRecord{float64(time.Now().UnixMilli()) / 1000, method, route, route, status, float64(time.Since(started).Microseconds()) / 1000}
		d := &s.diagnostics
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.since.IsZero() {
			d.since = started
			d.routes = map[string]requestTotals{}
		}
		if len(d.recent) >= 1000 {
			copy(d.recent, d.recent[1:])
			d.recent = d.recent[:999]
		}
		d.recent = append(d.recent, record)
		key := method + " " + route
		totals := d.routes[key]
		totals.Route = key
		totals.Count++
		if status >= 500 {
			totals.Errors++
		}
		totals.Total += record.MS
		if record.MS > totals.Max {
			totals.Max = record.MS
		}
		totals.Last = record.Time
		totals.Average = totals.Total / float64(totals.Count)
		d.routes[key] = totals
		if status >= 500 {
			d.nextLog++
			if len(d.logs) >= 3000 {
				copy(d.logs, d.logs[1:])
				d.logs = d.logs[:2999]
			}
			d.logs = append(d.logs, serverLog{ID: d.nextLog, Time: record.Time, Level: "error", Logger: "http", Message: record.Method + " " + record.Route})
		}
	}()
	s.serveAuthorized(wrapped, r)
}
func (s *Server) debugRequests(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	d := &s.diagnostics
	d.mu.Lock()
	recent := append([]requestRecord{}, d.recent...)
	routes := []requestTotals{}
	for _, value := range d.routes {
		routes = append(routes, value)
	}
	since := d.since
	d.mu.Unlock()
	q := strings.ToLower(r.URL.Query().Get("q"))
	errors := r.URL.Query().Get("errors") == "true"
	if raw := strings.ToLower(r.URL.Query().Get("errors")); raw != "" {
		switch raw {
		case "1", "true", "t", "on", "yes", "y":
			errors = true
		case "0", "false", "f", "off", "no", "n":
			errors = false
		default:
			write(w, 422, map[string]any{"detail": []any{map[string]any{"type": "bool_parsing", "loc": []string{"query", "errors"}, "msg": "Input should be a valid boolean, unable to interpret input", "input": raw}}})
			return
		}
	}
	slow := float64(0)
	if raw := r.URL.Query().Get("slow"); raw != "" {
		var err error
		slow, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			write(w, 422, map[string]any{"detail": []any{map[string]any{"type": "float_parsing", "loc": []string{"query", "slow"}, "msg": "Input should be a valid number, unable to parse string as a number", "input": raw}}})
			return
		}
	}
	out := []requestRecord{}
	for index := len(recent) - 1; index >= 0 && len(out) < 300; index-- {
		item := recent[index]
		if strings.Contains(strings.ToLower(item.Path), q) && (!errors || item.Status >= 400) && item.MS >= slow {
			out = append(out, item)
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Total > routes[j].Total })
	write(w, 200, map[string]any{"since": float64(since.UnixMilli()) / 1000, "recent": out, "routes": routes})
}
func (s *Server) debugLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	d := &s.diagnostics
	d.mu.Lock()
	recent := append([]serverLog{}, d.logs...)
	d.mu.Unlock()
	out := []any{}
	q := strings.ToLower(r.URL.Query().Get("q"))
	after, ok := integer(w, r, "after", 0)
	if !ok {
		return
	}
	floor := map[string]int{"debug": 10, "info": 20, "warning": 30, "error": 40}[r.URL.Query().Get("level")]
	for i := len(recent) - 1; i >= 0 && len(out) < 500; i-- {
		item := recent[i]
		if item.ID > after && map[string]int{"debug": 10, "info": 20, "warning": 30, "error": 40}[item.Level] >= floor && strings.Contains(strings.ToLower(item.Logger+" "+item.Message+" "+item.Detail), q) {
			out = append(out, item)
		}
	}
	write(w, 200, map[string]any{"logs": out})
}
func (s *Server) debugSystem(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	var browser any
	if provider, ok := s.config.Browser.(interface{ Stats() map[string]any }); ok {
		browser = provider.Stats()
	}
	if browser == nil {
		browser = map[string]any{}
	}
	data := browser.(map[string]any)
	loop := "off"
	if s.config.RefreshEvery > 0 {
		loop = "running"
	}
	d := &s.diagnostics
	d.mu.Lock()
	started := d.since
	d.mu.Unlock()
	process := map[string]any{"python": runtime.Version(), "go": runtime.Version(), "pid": os.Getpid(), "platform": runtime.GOOS, "heap_kb": memory.HeapAlloc / 1024}
	if raw, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						process["memory_kb"] = kb
					}
				}
			}
		}
	}
	files := map[string]any{}
	var bytes int64
	if err := s.config.Vault.Pool.QueryRow(r.Context(), "SELECT pg_database_size(current_database())").Scan(&bytes); err == nil {
		files["postgresql_bytes"] = bytes
	}
	calendars := s.config.Calendars.Stats()
	groups, _ := s.config.Vault.Groups(r.Context())
	names := map[int64]any{}
	for _, group := range groups {
		names[group["id"].(int64)] = group["name"]
	}
	for _, entry := range calendars {
		entry["name"] = names[entry["group_id"].(int64)]
	}
	sort.Slice(calendars, func(i, j int) bool { return calendars[i]["group_id"].(int64) < calendars[j]["group_id"].(int64) })
	write(w, 200, map[string]any{"now": float64(time.Now().UnixMilli()) / 1000, "started": float64(started.UnixMilli()) / 1000, "process": process, "files": files, "browser": data, "background": map[string]any{"refresh_loop": loop}, "presence": s.site.presence.stats(), "calendars": calendars, "telegram": nil})
}
