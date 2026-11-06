package identityserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/postgres"
	"github.com/jackc/pgx/v5"
)

func setup(t *testing.T, configure ...func(*Config)) (*Server, *postgres.Identity, string) {
	t.Helper()
	databaseURL := os.Getenv("TEST_IDENTITY_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_IDENTITY_DATABASE_URL is required")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, databaseURL, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	var name string
	if err = store.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "aa_test_") {
		t.Fatal("isolated test database required")
	}
	if err = store.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	identity := postgres.NewIdentity(store)
	pulseID := fmt.Sprintf("issuer-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		user, err := store.Pool.Query(ctx, "SELECT id FROM aaid.users WHERE id IN(SELECT user_id FROM aaid.mirea_links WHERE pulse_uid=$1)", pulseID)
		if err != nil {
			t.Error(err)
			return
		}
		ids, err := pgx.CollectRows(user, pgx.RowTo[string])
		if err != nil {
			t.Error(err)
			return
		}
		for _, id := range ids {
			for _, sql := range []string{"DELETE FROM aaid.web_tokens WHERE session_id IN(SELECT id FROM aaid.sessions WHERE user_id=$1)", "DELETE FROM aaid.refresh_tokens WHERE session_id IN(SELECT id FROM aaid.sessions WHERE user_id=$1)", "DELETE FROM aaid.sessions WHERE user_id=$1", "DELETE FROM aaid.mirea_links WHERE user_id=$1", "DELETE FROM aaid.users WHERE id=$1", "DELETE FROM aaid.audit WHERE user_id=$1"} {
				if _, err = store.Pool.Exec(ctx, sql, id); err != nil {
					t.Error(err)
				}
			}
		}
	})
	config := Config{Store: identity, Seed: make([]byte, 32), AdminToken: strings.Repeat("a", 32), WhoAmI: func(context.Context, string) (string, *int64, *string, error) { return pulseID, nil, nil, nil }}
	for _, change := range configure {
		change(&config)
	}
	server, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server, identity, pulseID
}
func request(t *testing.T, s *Server, method, path, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, "http://id.example"+path, strings.NewReader(body))
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(w.Body.String())
	}
	return w, result
}
func TestIssuerGrantsCompatibleTokensAndRefreshReuseRevokesImmediately(t *testing.T) {
	s, _, _ := setup(t)
	w, grant := request(t, s, "POST", "/v1/auth/mirea", `{"pulse_cookie":"session=synthetic","device":" Test device "}`, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	token := grant["access_token"].(string)
	claims, err := cryptocompat.Verify(token, map[string]ed25519.PublicKey{s.kid: s.public}, "aa-id", "aa", time.Now(), true)
	if err != nil || claims["sid"] != grant["session_id"] || claims["sub"] != grant["user"].(map[string]any)["id"] {
		t.Fatal("issued token contract changed")
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	w, me := request(t, s, "GET", "/v1/me", "", headers)
	if w.Code != 200 || me["session_id"] != grant["session_id"] {
		t.Fatal(w.Body.String())
	}
	body := `{"refresh_token":"` + grant["refresh_token"].(string) + `"}`
	w, renewed := request(t, s, "POST", "/v1/auth/refresh", body, nil)
	if w.Code != 200 || renewed["refresh_token"] == grant["refresh_token"] {
		t.Fatal("refresh was not rotated")
	}
	w, _ = request(t, s, "POST", "/v1/auth/refresh", body, nil)
	if w.Code != 401 {
		t.Fatal("refresh reuse accepted")
	}
	w, _ = request(t, s, "GET", "/v1/me", "", headers)
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("revocation did not invalidate issuer's active-session check")
	}
}
func TestIssuerAdminBoundaryAndSessionOwnership(t *testing.T) {
	s, _, _ := setup(t)
	w, grant := request(t, s, "POST", "/v1/auth/mirea", `{"pulse_cookie":"session=synthetic"}`, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := grant["user"].(map[string]any)["id"].(string)
	headers := map[string]string{"Authorization": "Bearer " + grant["access_token"].(string)}
	w, _ = request(t, s, "DELETE", "/v1/me/sessions/not-owned", "", headers)
	if w.Code != 404 {
		t.Fatal("unowned session could be revoked")
	}
	w, _ = request(t, s, "PUT", "/v1/admin/users/"+id+"/status", `{"status":"blocked"}`, nil)
	if w.Code != 403 {
		t.Fatal("unauthenticated moderation succeeded")
	}
	w, _ = request(t, s, "PUT", "/v1/admin/users/"+id+"/status", `{"status":"blocked"}`, map[string]string{"X-Admin-Token": strings.Repeat("a", 32)})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w, _ = request(t, s, "GET", "/v1/me", "", headers)
	if w.Code != 401 {
		t.Fatal("blocked user's session remained active")
	}
}
