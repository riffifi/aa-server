package httpserver

import (
	"net/http"
	"runtime"
)

// Browser diagnostics never return URLs, cookies, browser storage or account IDs.
func (s *Server) browserDebug(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	report := map[string]any{"go_heap_bytes": memory.HeapAlloc, "go_heap_objects": memory.HeapObjects, "goroutines": runtime.NumGoroutine(), "telegram_enabled": false, "browser": nil}
	if source, ok := s.config.Browser.(interface{ Stats() map[string]any }); ok {
		report["browser"] = source.Stats()
	}
	write(w, 200, report)
}
