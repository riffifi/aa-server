package identityserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnknownRoutesAndUnsupportedMethodsAreNotDependencyFailures(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), config: Config{StaticDir: "."}}
	s.routes()
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"POST", "/unknown", 404, ""},
		{"GET", "/unknown", 404, ""},
		{"POST", "/health", 405, "GET"},
		{"GET", "/v1/auth/refresh", 405, "POST"},
		{"PATCH", "/v1/me", 405, "GET"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: status=%d allow=%q", tc.method, tc.path, w.Code, w.Header().Get("Allow"))
		}
	}
}
