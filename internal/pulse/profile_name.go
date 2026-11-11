package pulse

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProfileName reads only the authenticated user's name. Wire fields are from
// the official PWA's GetMeInfoResponse_Authorized_User: 1=id, 2=first name,
// 3=last name, 4=google.protobuf.StringValue middle name.
func ProfileName(body []byte, headers http.Header, expectedUID string) string {
	state, uid, err := ProfileIdentity(body, headers)
	if err != nil || state != "valid" || uid != expectedUID || uid == "" {
		return ""
	}
	message, _, _ := Message(body, headers)
	outer, err := fields(message, true)
	if err != nil {
		return ""
	}
	authorized, err := fields(bytesField(outer, 1), true)
	if err != nil {
		return ""
	}
	user, err := fields(bytesField(authorized, 1), true)
	if err != nil {
		return ""
	}
	text := func(value any) (string, bool) {
		if value == nil {
			return "", true
		}
		data, ok := value.([]byte)
		if !ok || !utf8.Valid(data) || utf8.RuneCount(data) > 150 {
			return "", false
		}
		for _, r := range string(data) {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return "", false
			}
		}
		return strings.Join(strings.Fields(string(data)), " "), true
	}
	first, ok := text(user[2])
	if !ok {
		return ""
	}
	last, ok := text(user[3])
	if !ok {
		return ""
	}
	middle := ""
	if raw, exists := user[4]; exists {
		data, ok := raw.([]byte)
		if !ok {
			return ""
		}
		wrapper, err := fields(data, true)
		if err != nil {
			return ""
		}
		middle, ok = text(wrapper[1])
		if !ok {
			return ""
		}
	}
	base := strings.TrimSpace(last + " " + first)
	if base == "" {
		return ""
	}
	name := strings.TrimSpace(base + " " + middle)
	if utf8.RuneCountInString(name) > 60 {
		name = base
	}
	if utf8.RuneCountInString(name) > 60 {
		return ""
	}
	return name
}
