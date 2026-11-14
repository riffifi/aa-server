package browser

import (
	"aa-server/internal/pulse"
	"encoding/json"
	"strings"
)

// A valid old Pulse ticket must not count as a completed SSO renewal. Remove
// it only from this disposable context; keep SSO and challenge cookies intact.
// Never mutate the caller's stored credentials, including on failure.
func renewalStorage(state map[string]any) (map[string]any, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	var copy map[string]any
	if err := json.Unmarshal(data, &copy); err != nil {
		return nil, err
	}
	cookies, _ := copy["cookies"].([]any)
	kept := make([]any, 0, len(cookies))
	for _, item := range cookies {
		cookie, _ := item.(map[string]any)
		name, _ := cookie["name"].(string)
		domain, _ := cookie["domain"].(string)
		domain = strings.TrimPrefix(strings.ToLower(domain), ".")
		if (domain == "pulse.mirea.ru" || domain == "mirea.ru") && pulse.IsSessionCookie(name) {
			continue
		}
		kept = append(kept, item)
	}
	copy["cookies"] = kept
	return copy, nil
}
