package identityserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aa-server/internal/buildinfo"
)

func TestVersionNeedsNoIdentityDatabase(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.routes()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/version", nil))
	var got buildinfo.Info
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got != buildinfo.Current("aa-id") {
		t.Fatalf("version response: %d %s (%v)", w.Code, w.Body, err)
	}
	for _, method := range []string{"POST", "HEAD"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, "/api/version", nil))
		if w.Code != 405 || w.Header().Get("Allow") != "GET" {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
}
