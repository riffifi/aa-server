package pulse

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestPythonJournalAndACSGolden(t *testing.T) {
	var cases []struct {
		Kind, Body string
		Expected   any
	}
	data, err := os.ReadFile("testdata/python_journal.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, item := range cases {
		body, err := base64.StdEncoding.DecodeString(item.Body)
		if err != nil {
			t.Fatal(err)
		}
		var actual any
		switch item.Kind {
		case "acs":
			events, inside := ACSEvents(body, nil)
			actual = []any{events, inside}
		case "logs":
			actual = Logs(body, nil)
		case "lessons":
			actual = Lessons(body, nil)
		case "status":
			actual = LessonStatus(body, nil)
		}
		encoded, err := json.Marshal(actual)
		if err != nil {
			t.Fatal(err)
		}
		var normalized any
		_ = json.Unmarshal(encoded, &normalized)
		if !reflect.DeepEqual(normalized, item.Expected) {
			t.Fatalf("%s mismatch actual=%s expected=%v", item.Kind, encoded, item.Expected)
		}
	}
}
