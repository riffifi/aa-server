package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticRootDoesNotHideUnsupportedAPIMethods(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), config: Config{StaticDir: "."}}
	s.routes()
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/unknown", 404, ""},
		{"POST", "/unknown", 404, ""},
		{"GET", "/internal/aaid/link", 405, "POST"},
		{"POST", "/health", 405, "GET"},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || w.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: status=%d allow=%q", tc.method, tc.path, w.Code, w.Header().Get("Allow"))
		}
	}
}
