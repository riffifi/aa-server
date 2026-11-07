package httpserver

import "strings"

// RetiredClientPath deliberately returns 404 for every method and credential.
// These capabilities belong to server-owned workflows, not arbitrary clients.
func RetiredClientPath(path string) bool {
	if path == "/api/accounts/import" || path == "/api/debug/pass-testing" {
		return true
	}
	if strings.HasPrefix(path, "/api/accounts/") {
		parts := strings.Split(strings.TrimPrefix(path, "/api/accounts/"), "/")
		return (len(parts) >= 2 && parts[1] == "rpc") || (len(parts) == 2 && parts[1] == "pass")
	}
	return false
}
