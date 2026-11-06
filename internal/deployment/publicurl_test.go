package deployment

import "testing"

func TestPublicOriginsAreRestrictedWithoutBreakingLocalVerification(t *testing.T) {
	for _, origin := range []string{SiteURL, IdentityURL, "https://test.ytc-srv.org:443/", "http://127.0.0.1:18080", "http://localhost:18091", "http://[::1]:18080"} {
		if _, err := PublicURL(origin, ""); err != nil {
			t.Errorf("allowed origin %q rejected", origin)
		}
	}
	for _, origin := range []string{"https://outside.invalid", "https://ytc-srv.org.attacker.invalid", "https://aa.ytc-srv.org@outside.invalid", "https://outside.invalid/aa.ytc-srv.org", "https://aa.ytc-srv.org/path", "https://aa.ytc-srv.org?next=x", "https://aa.ytc-srv.org#x", "https://aa.ytc-srv.org:", "https://aa.ytc-srv.org:65536", "http://192.0.2.1", "https://aa.ytc-srv.org?"} {
		if _, err := PublicURL(origin, ""); err == nil {
			t.Errorf("foreign or invalid origin %q accepted", origin)
		}
	}
	if got, err := PublicURL("", SiteURL); err != nil || got != SiteURL {
		t.Fatal("canonical default missing")
	}
	if got, err := PublicURL("", ""); err != nil || got != "" {
		t.Fatal("optional address cannot remain disabled")
	}
}
