package pulse

import (
	"context"
	"math"
	"net/http"
	"unicode/utf8"
)

// identityUID matches AA ID's separate profile decoder, including unauthorized
// responses, while the session server keeps its stricter identity-state decoder.
func identityUID(status int, headers http.Header, body []byte) (string, error) {
	if status != 200 {
		return "", nil
	}
	payload, code, err := message(body, headers, false)
	if err != nil {
		return "", err
	}
	if code != 0 || payload == nil {
		return "", nil
	}
	outer, err := fields(payload, true)
	if err != nil {
		return "", err
	}
	if _, denied := outer[2]; denied {
		return "", nil
	}
	authorized, ok := outer[1].([]byte)
	if !ok {
		return "", nil
	}
	auth, err := fields(authorized, true)
	if err != nil {
		return "", err
	}
	user, err := fields(bytesField(auth, 1), true)
	if err != nil {
		return "", err
	}
	uid, ok := user[1].([]byte)
	if !ok {
		return "", nil
	}
	if !utf8.Valid(uid) {
		return "", ErrWire
	}
	if len(uid) == 0 || utf8.RuneCount(uid) > 256 {
		return "", nil
	}
	return string(uid), nil
}
func identityGroup(headers http.Header, body []byte) (*int64, *string) {
	payload, code, err := message(body, headers, false)
	if err != nil || code != 0 || payload == nil {
		return nil, nil
	}
	outer, err := fields(payload, true)
	if err != nil {
		return nil, nil
	}
	var bestID *int64
	var bestName *string
	for _, raw := range many(outer[1]) {
		data, ok := raw.([]byte)
		if !ok {
			return nil, nil
		}
		entry, err := fields(data, true)
		if err != nil {
			return nil, nil
		}
		target, err := fields(bytesField(entry, 1), true)
		if err != nil {
			return nil, nil
		}
		id, ok := target[2].(uint64)
		if target[1] != uint64(1) || !ok || id == 0 || id > math.MaxInt64 {
			continue
		}
		value := int64(id)
		var name *string
		if text, ok := entry[2].([]byte); ok {
			if !utf8.Valid(text) {
				return nil, nil
			}
			label := string(text)
			name = &label
		}
		if entry[4] == uint64(1) {
			return &value, name
		}
		if bestID == nil {
			bestID, bestName = &value, name
		}
	}
	return bestID, bestName
}

// DecodeIdentity preserves the AA ID profile parser, separate from the stricter
// session-server AuthenticationState contract.
func DecodeIdentity(status int, headers http.Header, body []byte) (string, error) {
	return identityUID(status, headers, body)
}
func DecodeGroup(headers http.Header, body []byte) (*int64, *string) {
	return identityGroup(headers, body)
}
func (t *Transport) WhoAmI(ctx context.Context, cookie string) (string, *int64, *string, error) {
	reply, err := t.RPC(ctx, cookie, "profile", ProfileRequest(), "")
	if err != nil {
		return "", nil, nil, err
	}
	uid, err := identityUID(reply.Status, reply.Headers, reply.Body)
	if err != nil || uid == "" {
		return uid, nil, nil, err
	}
	schedules, err := t.RPC(ctx, cookie, "schedules", Frame(nil, 0), "")
	if err != nil {
		return uid, nil, nil, nil
	}
	id, name := identityGroup(schedules.Headers, schedules.Body)
	return uid, id, name, nil
}
