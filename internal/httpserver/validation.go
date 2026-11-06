package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Field struct {
	Name, Kind         string
	Required, Nullable bool
	GT                 bool
	Min, Max           int64
	Pattern            string
}
type field = Field

var integerText = regexp.MustCompile(`^[+-]?[0-9]+(?:_[0-9]+)*(?:\.0+)?$`)

func parseIntegerText(raw string) (int64, bool, bool) {
	text := strings.TrimSpace(raw)
	if !integerText.MatchString(text) {
		return 0, false, false
	}
	text = strings.ReplaceAll(text, "_", "")
	if point := strings.IndexByte(text, '.'); point >= 0 {
		text = text[:point]
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return 0, false, false
	}
	if !value.IsInt64() {
		if value.Sign() < 0 {
			return math.MinInt64, true, true
		}
		return math.MaxInt64, true, true
	}
	return value.Int64(), false, true
}
func validation(kind string, loc []any, message string, input any, ctx map[string]any) map[string]any {
	out := map[string]any{"type": kind, "loc": loc, "msg": message, "input": input}
	if ctx != nil {
		out["ctx"] = ctx
	}
	return out
}
func model(r *http.Request) []field {
	switch {
	case r.URL.Path == "/api/accounts/import":
		return []field{{Name: "cookie", Kind: "string", Required: true, Min: 1, Max: 65536}, {Name: "label", Kind: "string", Required: true, Min: 1, Max: 60}, {Name: "account_id", Kind: "string", Nullable: true}, {Name: "expected_version", Kind: "int", Nullable: true}, {Name: "expected_session_version", Kind: "int", Nullable: true}, {Name: "group_id", Kind: "int", Nullable: true, Min: 1, Max: math.MaxInt64, GT: true}, {Name: "group_name", Kind: "string", Nullable: true, Max: 100}}
	case r.URL.Path == "/internal/aaid/link":
		return []field{{Name: "cookie", Kind: "string", Required: true, Min: 1, Max: 65536}, {Name: "storage_state", Kind: "dict", Nullable: true}, {Name: "expires_at", Kind: "float", Nullable: true}, {Name: "renewed_at", Kind: "float", Nullable: true}, {Name: "group_id", Kind: "int", Nullable: true, Min: 1, Max: math.MaxInt64, GT: true}, {Name: "group_name", Kind: "string", Nullable: true, Max: 100}}
	case r.URL.Path == "/api/me/account":
		return []field{{Name: "label", Kind: "string", Nullable: true, Min: 1, Max: 60}, {Name: "pass_allowed", Kind: "bool", Nullable: true}}
	case r.URL.Path == "/api/me/pass/people" || r.URL.Path == "/api/me/pass/requests":
		return []field{{Name: "account_id", Kind: "string", Required: true, Min: 1, Max: 64}}
	case r.URL.Path == "/panel/api/login":
		return []field{{Name: "password", Kind: "string", Required: true, Min: 1, Max: 256}}
	case strings.HasSuffix(r.URL.Path, "/label"):
		return []field{{Name: "label", Kind: "string", Required: true, Min: 1, Max: 60}, {Name: "expected_version", Kind: "int", Nullable: true, Min: 0}}
	case strings.HasSuffix(r.URL.Path, "/pass-allowed"):
		return []field{{Name: "allowed", Kind: "bool", Required: true}, {Name: "expected_version", Kind: "int", Nullable: true, Min: 0}}
	case strings.HasSuffix(r.URL.Path, "/group"):
		return []field{{Name: "group_id", Kind: "int", Nullable: true, Min: 1, Max: math.MaxInt64, GT: true}, {Name: "group_name", Kind: "string", Nullable: true, Max: 100}, {Name: "expected_version", Kind: "int", Required: true}}
	case strings.HasSuffix(r.URL.Path, "/pass"):
		return []field{{Name: "number", Kind: "string", Required: true, Min: 1, Max: 19}, {Name: "expected_version", Kind: "int", Required: true}}
	}
	return nil
}
func input(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	return DecodeBody(w, r, model(r))
}
func DecodeBody(w http.ResponseWriter, r *http.Request, specs []Field) (map[string]any, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		detail(w, 400, "There was an error parsing the body")
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	err = decoder.Decode(&value)
	if err == io.EOF || err == nil && value == nil {
		write(w, 422, map[string]any{"detail": []any{validation("missing", []any{"body"}, "Field required", nil, nil)}})
		return nil, false
	}
	if err != nil {
		pos := len(raw)
		message := "Expecting value"
		var syntax *json.SyntaxError
		if errorsAsSyntax(err, &syntax) {
			pos = int(syntax.Offset) - 1
			if strings.Contains(syntax.Error(), "object key string") {
				message = "Expecting property name enclosed in double quotes"
			}
		}
		if pos < 0 {
			pos = 0
		}
		if pos > len(raw) {
			pos = len(raw)
		}
		write(w, 422, map[string]any{"detail": []any{validation("json_invalid", []any{"body", utf8.RuneCount(raw[:pos])}, "JSON decode error", map[string]any{}, map[string]any{"error": message})}})
		return nil, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		detail(w, 422, "Invalid request body")
		return nil, false
	}
	body, ok := value.(map[string]any)
	if !ok {
		write(w, 422, map[string]any{"detail": []any{validation("model_attributes_type", []any{"body"}, "Input should be a valid dictionary or object to extract fields from", value, nil)}})
		return nil, false
	}
	problems := []any{}
	original := map[string]any{}
	for key, value := range body {
		original[key] = value
	}
	for _, spec := range specs {
		raw, exists := body[spec.Name]
		loc := []any{"body", spec.Name}
		if !exists {
			if spec.Required {
				problems = append(problems, validation("missing", loc, "Field required", original, nil))
			}
			continue
		}
		if raw == nil && spec.Nullable {
			continue
		}
		switch spec.Kind {
		case "strings":
			list, ok := raw.([]any)
			if !ok {
				problems = append(problems, validation("list_type", loc, "Input should be a valid list", raw, nil))
				continue
			}
			if int64(len(list)) < spec.Min || int64(len(list)) > spec.Max {
				kind, word, bound, key := "too_short", "at least", spec.Min, "min_length"
				if int64(len(list)) > spec.Max {
					kind, word, bound, key = "too_long", "at most", spec.Max, "max_length"
				}
				problems = append(problems, validation(kind, loc, fmt.Sprintf("List should have %s %d items after validation, not %d", word, bound, len(list)), raw, map[string]any{"field_type": "List", key: bound, "actual_length": len(list)}))
				continue
			}
			outList := []string{}
			for index, value := range list {
				text, ok := value.(string)
				if !ok {
					problems = append(problems, validation("string_type", append(append([]any{}, loc...), index), "Input should be a valid string", value, nil))
				} else {
					outList = append(outList, text)
				}
			}
			body[spec.Name] = outList
		case "dict":
			if _, ok := raw.(map[string]any); !ok {
				problems = append(problems, validation("dict_type", loc, "Input should be a valid dictionary", raw, nil))
			}
		case "float":
			var value float64
			valid := false
			switch item := raw.(type) {
			case string:
				var err error
				value, err = strconv.ParseFloat(item, 64)
				valid = err == nil
			case json.Number:
				var err error
				value, err = item.Float64()
				valid = err == nil
			case bool:
				valid = true
				if item {
					value = 1
				}
			}
			if !valid {
				kind, msg := "float_type", "Input should be a valid number"
				if _, ok := raw.(string); ok {
					kind, msg = "float_parsing", "Input should be a valid number, unable to parse string as a number"
				}
				problems = append(problems, validation(kind, loc, msg, raw, nil))
			} else {
				body[spec.Name] = value
			}
		case "string":
			text, ok := raw.(string)
			if !ok {
				problems = append(problems, validation("string_type", loc, "Input should be a valid string", raw, nil))
				continue
			}
			length := int64(utf8.RuneCountInString(text))
			if length < spec.Min {
				suffix := "characters"
				if spec.Min == 1 {
					suffix = "character"
				}
				problems = append(problems, validation("string_too_short", loc, fmt.Sprintf("String should have at least %d %s", spec.Min, suffix), raw, map[string]any{"min_length": spec.Min}))
			} else if spec.Max > 0 && length > spec.Max {
				problems = append(problems, validation("string_too_long", loc, fmt.Sprintf("String should have at most %d characters", spec.Max), raw, map[string]any{"max_length": spec.Max}))
			} else if spec.Pattern != "" {
				matched, err := regexp.MatchString(spec.Pattern, text)
				if err != nil || !matched {
					problems = append(problems, validation("string_pattern_mismatch", loc, "String should match pattern '"+spec.Pattern+"'", raw, map[string]any{"pattern": spec.Pattern}))
				}
			}
		case "int":
			var number int64
			var valid bool
			var overflow bool
			switch item := raw.(type) {
			case string:
				number, overflow, valid = parseIntegerText(item)
			case json.Number:
				ratio, ok := new(big.Rat).SetString(string(item))
				if ok && ratio.IsInt() {
					valid = true
					if ratio.Num().IsInt64() {
						number = ratio.Num().Int64()
					} else if ratio.Sign() < 0 {
						overflow = true
						number = math.MinInt64
					} else {
						overflow = true
						number = math.MaxInt64
					}
				}
			case bool:
				valid = true
				if item {
					number = 1
				}
			}
			if !valid {
				kind, msg := "int_type", "Input should be a valid integer"
				if _, ok := raw.(string); ok {
					kind, msg = "int_parsing", "Input should be a valid integer, unable to parse string as an integer"
				}
				if _, ok := raw.(json.Number); ok {
					kind, msg = "int_from_float", "Input should be a valid integer, got a number with a fractional part"
				}
				problems = append(problems, validation(kind, loc, msg, raw, nil))
				continue
			}
			if number < spec.Min {
				if spec.GT {
					problems = append(problems, validation("greater_than", loc, fmt.Sprintf("Input should be greater than %d", spec.Min-1), raw, map[string]any{"gt": spec.Min - 1}))
				} else {
					problems = append(problems, validation("greater_than_equal", loc, fmt.Sprintf("Input should be greater than or equal to %d", spec.Min), raw, map[string]any{"ge": spec.Min}))
				}
				continue
			}
			if spec.Max > 0 && (number > spec.Max || overflow) {
				problems = append(problems, validation("less_than_equal", loc, fmt.Sprintf("Input should be less than or equal to %d", spec.Max), raw, map[string]any{"le": spec.Max}))
				continue
			}
			body[spec.Name] = number
		case "bool":
			flag, ok := raw.(bool)
			if !ok {
				switch item := raw.(type) {
				case string:
					switch strings.ToLower(item) {
					case "0", "off", "f", "false", "n", "no":
						flag, ok = false, true
					case "1", "on", "t", "true", "y", "yes":
						flag, ok = true, true
					}
				case json.Number:
					if item == "0" || item == "0.0" {
						flag, ok = false, true
					} else if item == "1" || item == "1.0" {
						flag, ok = true, true
					}
				}
			}
			if !ok {
				kind, msg := "bool_parsing", "Input should be a valid boolean, unable to interpret input"
				if raw == nil {
					kind, msg = "bool_type", "Input should be a valid boolean"
				}
				problems = append(problems, validation(kind, loc, msg, raw, nil))
				continue
			}
			body[spec.Name] = flag
		}
	}
	if len(problems) > 0 {
		write(w, 422, map[string]any{"detail": problems})
		return nil, false
	}
	return body, true
}
func errorsAsSyntax(err error, out **json.SyntaxError) bool {
	value, ok := err.(*json.SyntaxError)
	if ok {
		*out = value
	}
	return ok
}
func stringField(w http.ResponseWriter, body map[string]any, key string, min, max int) (string, bool) {
	value, ok := body[key].(string)
	if !ok {
		detail(w, 422, "Invalid "+key)
	}
	return value, ok
}
func expected(w http.ResponseWriter, body map[string]any) (*int64, bool) {
	raw := body["expected_version"]
	if raw == nil {
		return nil, true
	}
	value, ok := raw.(int64)
	if !ok {
		detail(w, 422, "Invalid expected_version")
		return nil, false
	}
	return &value, true
}
