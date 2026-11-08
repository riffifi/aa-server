package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The application only reads a sanitized host-collector snapshot. It never
// receives the Docker socket, daemon credentials, container env or host paths.
func (s *Server) debugMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.panel(w, r, false); !ok {
		return
	}
	path := s.config.MetricsFile
	if path == "" {
		path = "/metrics/snapshot.json"
	}
	file, err := os.Open(path)
	if err != nil {
		code := "io"
		if errors.Is(err, os.ErrNotExist) {
			code = "missing"
		} else if errors.Is(err, os.ErrPermission) {
			code = "permission"
		}
		write(w, 200, map[string]any{"available": false, "reason": "Resource collector is not available", "reason_code": code})
		return
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	var data map[string]any
	if err != nil || len(body) > 8<<20 || json.Unmarshal(body, &data) != nil || numberAny(data["version"]) != 1 {
		write(w, 200, map[string]any{"available": false, "reason": "Resource collector snapshot is invalid", "reason_code": "invalid"})
		return
	}
	data["available"] = true
	data["history"] = []any{}
	if history, err := os.Open(filepath.Join(filepath.Dir(path), "history.json")); err == nil {
		defer history.Close()
		body, err := io.ReadAll(io.LimitReader(history, (2<<20)+1))
		var points []map[string]any
		if err == nil && len(body) <= 2<<20 && json.Unmarshal(body, &points) == nil && len(points) <= 1440 {
			data["history"] = points
		}
	}
	age := float64(time.Now().Unix()) - numberAny(data["collected_at"])
	data["stale"] = age > 60 || age < -60
	write(w, 200, data)
}
