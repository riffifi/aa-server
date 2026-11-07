package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"aa-server/internal/buildinfo"
)

func TestPublicVersionWorksWithoutDatabaseOrAuthentication(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), config: Config{EnforceAAID: true,
		Verify: func(context.Context, string) (map[string]any, error) {
			return nil, errors.New("invalid token")
		}}}
	s.routes()
	for _, token := range []string{"", "Bearer invalid"} {
		r := httptest.NewRequest("GET", "/api/version", nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var got buildinfo.Info
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got != buildinfo.Current("sessions") {
			t.Fatalf("version response: %d %s (%v)", w.Code, w.Body, err)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("version responses must not be cached across deployments")
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/api/version", 405}, {"HEAD", "/api/version", 405},
		{"GET", "/api/version/", 401}, {"GET", "/api/version-private", 401},
		{"GET", "/api/accounts", 401},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Authorization", "Bearer invalid")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}
