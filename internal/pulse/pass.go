package pulse

import (
	"net/http"
	"strconv"
	"unicode/utf8"
)

// PassIssueRequest follows human_pass.proto shared with the native client.
// The device label is server-owned; clients supply only the verification code.
func PassIssueRequest(code string) []byte {
	return Frame(append(Field(1, []byte(code)), Field(2, Field(1, []byte("AntiAttendance")))...), 0)
}

func PassToken(body []byte, headers http.Header) (string, error) {
	message, status, err := Message(body, headers)
	if err != nil || status != 0 {
		return "", ErrWire
	}
	fields, err := Fields(message)
	value, ok := fields[1].([]byte)
	if err != nil || !ok || len(value) == 0 || len(value) > 16384 {
		return "", ErrWire
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-') {
			return "", ErrWire
		}
	}
	return string(value), nil
}

// PassResult validates the response oneof instead of treating an empty or
// malformed success as an issued credential. Result tags match the protobuf.
func PassResult(body []byte, headers http.Header) (uint64, []byte, error) {
	message, status, err := Message(body, headers)
	if err != nil || status != 0 {
		return 0, nil, ErrWire
	}
	fields, err := fields(message, true)
	if err != nil || len(fields) != 1 {
		return 0, nil, ErrWire
	}
	for tag, value := range fields {
		data, ok := value.([]byte)
		if !ok || tag < 1 || tag > 3 {
			return 0, nil, ErrWire
		}
		return tag, data, nil
	}
	return 0, nil, ErrWire
}

func PassNumber(payload []byte) (string, error) {
	values, err := Fields(payload)
	number, ok := values[1].(uint64)
	if err != nil || !ok || number == 0 || number > 1<<63-1 {
		return "", ErrWire
	}
	return strconv.FormatUint(number, 10), nil
}

func PassRetryAt(payload []byte, nested bool) (any, error) {
	if nested {
		values, err := Fields(payload)
		if err != nil {
			return nil, err
		}
		if values[1] == nil {
			return nil, nil
		}
		var ok bool
		payload, ok = values[1].([]byte)
		if !ok {
			return nil, ErrWire
		}
	}
	values, err := Fields(payload)
	seconds, ok := values[1].(uint64)
	if err != nil || !ok || seconds > 253402300799 {
		return nil, ErrWire
	}
	return int64(seconds), nil
}

func ValidPassCode(code string) bool {
	if !utf8.ValidString(code) || len(code) != 6 {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
