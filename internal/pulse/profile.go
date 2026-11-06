package pulse

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

func bytesField(fields map[uint64]any, key uint64) []byte {
	data, _ := fields[key].([]byte)
	return data
}

func ProfileIdentity(body []byte, headers http.Header) (string, string, error) {
	message, status, err := Message(body, headers)
	if err != nil {
		return "", "", err
	}
	if status == 16 {
		return "expired", "", nil
	}
	if status == 7 {
		return "forbidden", "", nil
	}
	if status != 0 || message == nil {
		return "unavailable", "", nil
	}
	outer, err := Fields(message)
	if err != nil {
		return "", "", err
	}
	_, one := outer[1]
	_, two := outer[2]
	if one && two {
		return "", "", ErrWire
	}
	if two {
		return "expired", "", nil
	}
	authorized, err := Fields(bytesField(outer, 1))
	if err != nil {
		return "", "", err
	}
	user, err := Fields(bytesField(authorized, 1))
	if err != nil {
		return "", "", err
	}
	uid := bytesField(user, 1)
	if !utf8.Valid(uid) || len(uid) == 0 || utf8.RuneCount(uid) > 256 {
		return "", "", ErrWire
	}
	return "valid", string(uid), nil
}

func AuthenticationState(status int, headers http.Header, body []byte) (string, string) {
	switch status {
	case 401:
		return "expired", ""
	case 403:
		return "forbidden", ""
	case 301, 302, 303, 307, 308:
		location, err := url.Parse(headers.Get("Location"))
		if err == nil && (strings.Contains(location.Path, "/auth/login") || location.Hostname() == "sso.mirea.ru") {
			return "expired", ""
		}
		return "unavailable", ""
	case 200:
		state, uid, err := ProfileIdentity(body, headers)
		if err != nil {
			return "invalidResponse", ""
		}
		return state, uid
	default:
		return "unavailable", ""
	}
}

func AttendanceToken(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || (parsed.Hostname() != "pulse.mirea.ru" && parsed.Hostname() != "attendance-app.mirea.ru") {
		return "", ErrWire
	}
	tokens := parsed.Query()["token"]
	nonempty := []string{}
	for _, token := range tokens {
		if token != "" {
			nonempty = append(nonempty, token)
		}
	}
	if len(nonempty) != 1 || utf8.RuneCountInString(nonempty[0]) > 4096 {
		return "", ErrWire
	}
	return nonempty[0], nil
}

func TokenExpiry(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, ErrWire
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0, ErrWire
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return 0, ErrWire
	}
	var expiry int64
	if json.Unmarshal(claims["exp"], &expiry) != nil || expiry <= 0 {
		return 0, ErrWire
	}
	return expiry, nil
}

func Timestamp(seconds uint64, nanos uint64) []byte {
	out := VField(1, seconds)
	if nanos != 0 {
		out = append(out, VField(2, nanos)...)
	}
	return out
}

func ACSTimelineRequest(uid string, start, end uint64) []byte {
	window := append(Field(1, Timestamp(start, 0)), Field(2, Timestamp(end, 999000000))...)
	out := append(Field(1, []byte(uid)), Field(2, window)...)
	out = append(out, VField(3, 1)...)
	out = append(out, VField(4, 2)...)
	return Frame(out, 0)
}

func DayLessonsRequest(year, month, day uint64) []byte {
	out := VField(1, year)
	out = append(out, VField(2, month)...)
	out = append(out, VField(3, day)...)
	return Frame(Field(2, out), 0)
}
func LessonAttendanceRequest(lessonID, logID string) []byte {
	return Frame(append(Field(1, []byte(lessonID)), Field(2, []byte(logID))...), 0)
}
