package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetiredClientWorkflowsNeverReadCredentialsOrCallPulse(t *testing.T) {
	upstream := &fakePulse{}
	s := &Server{mux: http.NewServeMux(), config: Config{Pulse: upstream,
		Verify: func(context.Context, string) (map[string]any, error) {
			t.Fatal("retired route attempted authentication")
			return nil, nil
		}}}
	s.routes()
	for _, path := range []string{"/api/accounts/import", "/api/accounts/id/pass", "/api/accounts/id/rpc/profile", "/api/accounts/id/rpc/pass-issue", "/api/accounts/id/rpc/acs-timeline", "/api/debug/pass-testing"} {
		for _, method := range []string{"GET", "POST", "PUT", "HEAD"} {
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("Authorization", "Bearer old-app-token")
			r.Header.Set("Cookie", "aa_site=old-cookie")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 404 {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	if upstream.calls != 0 {
		t.Fatal("removed workflow called Pulse")
	}
}
