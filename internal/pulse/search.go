package pulse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

func (t *Transport) SearchGroups(ctx context.Context, query string) []map[string]any {
	result := []map[string]any{}
	endpoint := "https://schedule-of.mirea.ru/schedule/api/search?" + url.Values{"limit": []string{"15"}, "match": []string{query}}.Encode()
	request, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return result
	}
	request.Header.Set("Accept", "application/json")
	response, err := t.HTTP.Do(request)
	if err != nil {
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return result
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, MaxResponse))
	decoder.UseNumber()
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if decoder.Decode(&payload) != nil {
		return result
	}
	for _, item := range payload.Data {
		target, ok := item["scheduleTarget"].(json.Number)
		if !ok {
			continue
		}
		kind, err := target.Float64()
		if err != nil || kind != 1 {
			continue
		}
		number, ok := item["id"].(json.Number)
		if !ok {
			continue
		}
		id, err := number.Int64()
		if err != nil {
			continue
		}
		name, _ := item["targetTitle"].(string)
		full, _ := item["fullTitle"].(string)
		result = append(result, map[string]any{"id": id, "name": name, "full": full})
		if len(result) == 15 {
			break
		}
	}
	return result
}
