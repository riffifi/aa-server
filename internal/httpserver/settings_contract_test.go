package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aa-server/internal/postgres"
)

func TestCapturedPythonSharingConversation(t *testing.T) {
	var fixture struct {
		Now   int64
		Calls []struct {
			As           string `json:"as"`
			Method, Path string
			Body         any
			Status       int
			Expected     map[string]any
		}
	}
	data, err := os.ReadFile("testdata/python_settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("conversation-%d-", time.Now().UnixNano())
	s, v := setup(t, func(c *Config) {
		c.Vault.Now = func() time.Time { return time.Unix(fixture.Now, 0) }
		c.Verify = func(_ context.Context, token string) (map[string]any, error) {
			return map[string]any{"sub": token, "mirea": prefix + token, "usr": token}, nil
		}
	})
	aliases := map[string]string{}
	group := int64(5330)
	for _, person := range []string{"owner", "guest", "other"} {
		row, err := v.Upsert(context.Background(), postgres.Upsert{PulseID: prefix + person, Label: strings.ToUpper(person[:1]) + person[1:], GroupID: &group, Credentials: map[string]any{"cookie": "session=" + person}})
		if err != nil {
			t.Fatal(err)
		}
		id := row["id"].(string)
		aliases[id] = person
		t.Cleanup(func() { _ = v.DeleteAccount(context.Background(), id) })
	}
	var replace func(any) any
	replace = func(value any) any {
		switch item := value.(type) {
		case string:
			for actual, alias := range aliases {
				item = strings.ReplaceAll(item, "$"+alias, actual)
			}
			return item
		case map[string]any:
			out := map[string]any{}
			for key, value := range item {
				out[key] = replace(value)
			}
			return out
		default:
			return value
		}
	}
	var normalize func(any, string) any
	normalize = func(value any, key string) any {
		switch item := value.(type) {
		case map[string]any:
			out := map[string]any{}
			for name, value := range item {
				out[name] = normalize(value, name)
			}
			return out
		case []any:
			out := []any{}
			for _, value := range item {
				out = append(out, normalize(value, ""))
			}
			return out
		case string:
			if key == "id" {
				if _, known := aliases[item]; !known {
					n := 1
					for _, alias := range aliases {
						if strings.HasPrefix(alias, "request") {
							n++
						}
					}
					aliases[item] = fmt.Sprintf("request%d", n)
				}
			}
			if alias, known := aliases[item]; known {
				return "$" + alias
			}
			return item
		default:
			return value
		}
	}
	for i, call := range fixture.Calls {
		t.Run(fmt.Sprintf("%02d_%s_%s", i, call.As, call.Path), func(t *testing.T) {
			body := ""
			if call.Body != nil {
				raw, err := json.Marshal(replace(call.Body))
				if err != nil {
					t.Fatal(err)
				}
				body = string(raw)
			}
			w, actual := request(t, s, call.Method, replace(call.Path).(string), body, map[string]string{"Authorization": "Bearer " + call.As})
			normalized := normalize(actual, "")
			if w.Code != call.Status || !reflect.DeepEqual(normalized, call.Expected) {
				t.Fatalf("Python sharing parity mismatch: status=%d expected=%d actual=%v expected=%v", w.Code, call.Status, normalized, call.Expected)
			}
		})
	}
}
