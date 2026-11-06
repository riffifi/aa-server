package pulse

import (
	"net/http"
	"sort"
)

func ACSEvents(body []byte, headers http.Header) ([]map[string]any, bool) {
	events := []map[string]any{}
	payload, status, err := Message(body, headers)
	if err != nil || status != 0 || payload == nil {
		return events, false
	}
	var walk func(any)
	walk = func(node any) {
		switch node := node.(type) {
		case map[string]any:
			stamp, _ := node["2"].(map[string]any)
			when, valid := stamp["1"].(uint64)
			code, isCode := node["5"].(uint64)
			zone, isZone := node["3"].(map[string]any)
			if valid && isCode && isZone && (code == 1 || code == 2) {
				direction := "in"
				if code == 2 {
					direction = "out"
				}
				var place any
				if text, ok := zone["2"].(string); ok {
					place = text
				}
				events = append(events, map[string]any{"time": when, "direction": direction, "zone": place})
			}
			for _, value := range node {
				walk(value)
			}
		case []any:
			for _, value := range node {
				walk(value)
			}
		}
	}
	walk(DecodeTree(payload))
	sort.SliceStable(events, func(i, j int) bool { return events[i]["time"].(uint64) < events[j]["time"].(uint64) })
	return events, len(events) > 0 && events[len(events)-1]["direction"] == "in"
}
