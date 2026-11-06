package pulse

import (
	"net/http"
	"strings"
)

func treeMessage(body []byte, h http.Header) map[string]any {
	data, status, err := Message(body, h)
	if err != nil || status != 0 {
		return nil
	}
	if len(data) == 0 {
		return map[string]any{}
	}
	return DecodeTree(data)
}
func dict(v any) map[string]any { out, _ := v.(map[string]any); return out }
func entries(v any) []any {
	if v == nil {
		return nil
	}
	if many, ok := v.([]any); ok {
		return many
	}
	return []any{v}
}
func Logs(body []byte, h http.Header) []map[string]any {
	tree := treeMessage(body, h)
	if tree == nil {
		return nil
	}
	out := []map[string]any{}
	for _, item := range entries(tree["2"]) {
		journal := dict(dict(item)["2"])
		id, ok := journal["1"].(string)
		if !ok {
			continue
		}
		semester := dict(journal["6"])
		var name any
		if value, ok := journal["2"].(string); ok {
			name = value
		}
		out = append(out, map[string]any{"id": id, "group": name, "start": dict(semester["3"])["1"], "end": dict(semester["4"])["1"]})
	}
	return out
}
func Lessons(body []byte, h http.Header) []map[string]any {
	tree := treeMessage(body, h)
	if tree == nil {
		return nil
	}
	out := []map[string]any{}
	for _, item := range entries(tree["2"]) {
		lesson := dict(dict(item)["3"])
		id, ok := lesson["1"].(string)
		if !ok {
			continue
		}
		start, a := dict(lesson["2"])["1"].(uint64)
		end, b := dict(lesson["3"])["1"].(uint64)
		if !a || !b {
			continue
		}
		subject, _ := dict(lesson["4"])["2"].(string)
		out = append(out, map[string]any{"id": id, "start": start, "end": end, "subject": subject})
	}
	return out
}
func LessonStatus(body []byte, h http.Header) map[string]string {
	tree := treeMessage(body, h)
	if tree == nil {
		return nil
	}
	out := map[string]string{}
	for _, item := range entries(tree["1"]) {
		student := dict(item)
		uid, ok := student["9"].(string)
		if !ok {
			continue
		}
		label, _ := dict(student["7"])["1"].(string)
		parts := strings.Split(label, "|")
		if len(parts) < 2 {
			continue
		}
		word := parts[len(parts)-1]
		state := ""
		switch word {
		case "Present":
			state = "present"
		case "Absent":
			state = "absent"
		case "ExcusedAbsence":
			state = "excused"
		default:
			runes := []rune(strings.ToLower(word))
			if len(runes) > 32 {
				runes = runes[:32]
			}
			state = string(runes)
		}
		out[uid] = state
	}
	return out
}
