package httpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetricsRequirePrivatePanelAndExposeStaleness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	s, v := setup(t, func(c *Config) { c.MetricsFile = path; c.PanelLocalOnly = true })
	token, _, err := v.AdminLogin(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	for _, headers := range []map[string]string{nil, {"Authorization": "Bearer invalid"}, {"Cookie": "panel_session=" + token}} {
		w, _ := secureRequest(t, s, "GET", "/panel/api/debug/metrics", "", headers)
		if w.Code == 200 {
			t.Fatal("metrics exposed publicly")
		}
	}
	headers := map[string]string{"Cookie": "panel_session=" + token, "X-AA-Panel": "1"}
	w, data := secureRequest(t, s, "GET", "/panel/api/debug/metrics", "", headers)
	if w.Code != 200 || data["available"] != false {
		t.Fatal(w.Body.String())
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"version":1,"collected_at":%d,"total":{"rx_bytes":123}}`, time.Now().Unix()-120)), 0600); err != nil {
		t.Fatal(err)
	}
	w, data = secureRequest(t, s, "GET", "/panel/api/debug/metrics", "", headers)
	if w.Code != 200 || data["available"] != true || data["stale"] != true {
		t.Fatal(w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "history.json"), []byte(`[{"time":1,"cpu_percent":2}]`), 0600); err != nil {
		t.Fatal(err)
	}
	w, data = secureRequest(t, s, "GET", "/panel/api/debug/metrics", "", headers)
	if w.Code != 200 || len(data["history"].([]any)) != 1 {
		t.Fatal("minute history missing", w.Body.String())
	}
}
