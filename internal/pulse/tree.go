package pulse

import (
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strconv"
	"unicode/utf8"
)

func asText(data []byte) (string, bool) {
	if !utf8.Valid(data) {
		return "", false
	}
	text := string(data)
	for _, c := range text {
		if c < 32 && c != '\t' && c != '\n' && c != '\r' {
			return "", false
		}
	}
	return text, true
}

func DecodeTree(payload []byte) map[string]any { return decodeTree(payload, 0) }

// decodeTree deliberately returns a partial tree on malformed diagnostic data.
func decodeTree(payload []byte, depth int) map[string]any {
	out := map[string]any{}
	for len(payload) > 0 {
		tag, n := binary.Uvarint(payload)
		if n <= 0 || tag>>3 == 0 {
			return out
		}
		payload = payload[n:]
		var value any
		switch tag & 7 {
		case 0:
			number, n := binary.Uvarint(payload)
			if n <= 0 {
				return out
			}
			payload = payload[n:]
			value = number
		case 1, 5:
			size := 4
			if tag&7 == 1 {
				size = 8
			}
			if len(payload) < size {
				return out
			}
			if size == 8 {
				value = binary.LittleEndian.Uint64(payload[:size])
			} else {
				value = uint64(binary.LittleEndian.Uint32(payload[:size]))
			}
			payload = payload[size:]
		case 2:
			size, n := binary.Uvarint(payload)
			if n <= 0 {
				return out
			}
			payload = payload[n:]
			if size > uint64(len(payload)) {
				return out
			}
			data := payload[:int(size)]
			payload = payload[int(size):]
			nested := map[string]any{}
			if depth < 6 && len(data) > 0 {
				nested = decodeTree(data, depth+1)
			}
			text, ok := asText(data)
			switch {
			case ok:
				value = text
			case len(nested) > 0:
				value = nested
			default:
				preview := data
				if len(preview) > 64 {
					preview = preview[:64]
				}
				value = map[string]any{"_hex": hex.EncodeToString(preview), "_len": len(data)}
			}
		default:
			return out
		}
		key := strconv.FormatUint(tag>>3, 10)
		if old, ok := out[key]; ok {
			if many, ok := old.([]any); ok {
				out[key] = append(many, value)
			} else {
				out[key] = []any{old, value}
			}
		} else {
			out[key] = value
		}
	}
	return out
}

func many(value any) []any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		return list
	}
	return []any{value}
}
func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func text(value any) any {
	if value, ok := value.(string); ok {
		return value
	}
	return nil
}
func messageTree(body []byte, headers http.Header) (map[string]any, error) {
	message, status, err := Message(body, headers)
	if err != nil || status != 0 {
		return nil, ErrWire
	}
	return DecodeTree(message), nil
}
