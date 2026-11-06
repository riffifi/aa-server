// Package pulse implements the existing server's bounded gRPC-Web wire protocol.
package pulse

import (
	"encoding/binary"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

var ErrWire = errors.New("invalid gRPC-Web response")

const Host = "https://pulse.mirea.ru"
const MaxResponse = 1024 * 1024

var Methods = map[string]string{
	"profile":           "/rtu_tc.rtu_attend.app.UserService/GetMeInfo",
	"attendance":        "/rtu_tc.attendance.api.AttendanceService/SelfApproveAttendanceThroughQRCode",
	"pass-token":        "/rtu.pulse_app.LongTimeTokenService/GetAccessTokenForDigitalPass",
	"pass-code":         "/rtu_tc.rtu_attend.humanpass.HumanPassService/SendVerificationCode",
	"pass-issue":        "/rtu_tc.rtu_attend.humanpass.HumanPassService/GetDigitalPass",
	"schedules":         "/rtu.schedule.api.PersonalScheduleService/GetSubscribedSchedules",
	"acs-timeline":      "/rtu_tc.rtu_attend.humanpass.HumanPassService/GetHumanAcsTimeline",
	"visiting-logs":     "/rtu_tc.attendance.api.VisitingLogService/GetAvailableVisitingLogsOfStudent",
	"day-lessons":       "/rtu_tc.attendance.api.LessonService/GetAvailableLessonsOfVisitingLogs",
	"lesson-attendance": "/rtu_tc.attendance.api.AttendanceService/GetAttendanceForLesson",
}

func Varint(value uint64) []byte { return binary.AppendUvarint(nil, value) }
func Field(number uint64, value []byte) []byte {
	out := Varint(number<<3 | 2)
	out = append(out, Varint(uint64(len(value)))...)
	return append(out, value...)
}
func VField(number, value uint64) []byte { return append(Varint(number<<3), Varint(value)...) }
func Frame(payload []byte, flag byte) []byte {
	out := make([]byte, 5, len(payload)+5)
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}
func ProfileRequest() []byte { return Frame(append(Field(1, []byte(Host)), 0x18, 0x01), 0) }

func Fields(payload []byte) (map[uint64]any, error) { return fields(payload, false) }
func fields(payload []byte, repeated bool) (map[uint64]any, error) {
	out := map[uint64]any{}
	assign := func(number uint64, value any) {
		if old, ok := out[number]; ok && repeated {
			if many, ok := old.([]any); ok {
				out[number] = append(many, value)
			} else {
				out[number] = []any{old, value}
			}
		} else {
			out[number] = value
		}
	}
	for len(payload) > 0 {
		tag, n := binary.Uvarint(payload)
		if n <= 0 || tag>>3 == 0 {
			return nil, ErrWire
		}
		payload = payload[n:]
		number, wire := tag>>3, tag&7
		if wire == 0 {
			value, n := binary.Uvarint(payload)
			if n <= 0 {
				return nil, ErrWire
			}
			assign(number, value)
			payload = payload[n:]
			continue
		}
		var size uint64
		switch wire {
		case 1:
			size = 8
		case 5:
			size = 4
		case 2:
			value, n := binary.Uvarint(payload)
			if n <= 0 {
				return nil, ErrWire
			}
			size = value
			payload = payload[n:]
		default:
			return nil, ErrWire
		}
		if size > uint64(len(payload)) {
			return nil, ErrWire
		}
		if wire == 2 {
			assign(number, payload[:int(size)])
		}
		payload = payload[int(size):]
	}
	return out, nil
}

func Message(body []byte, headers http.Header) ([]byte, int, error) {
	return message(body, headers, true)
}
func message(body []byte, headers http.Header, strict bool) ([]byte, int, error) {
	if !strings.HasPrefix(strings.ToLower(headers.Get("Content-Type")), "application/grpc-web") {
		return nil, 0, ErrWire
	}
	status := headers.Get("Grpc-Status")
	_, hasStatus := headers[http.CanonicalHeaderKey("Grpc-Status")]
	var message []byte
	seen := false
	for len(body) > 0 {
		if len(body) < 5 {
			return nil, 0, ErrWire
		}
		flag, size := body[0], binary.BigEndian.Uint32(body[1:5])
		body = body[5:]
		if uint64(size) > uint64(len(body)) {
			return nil, 0, ErrWire
		}
		payload := body[:int(size)]
		body = body[int(size):]
		switch flag {
		case 0:
			if seen {
				return nil, 0, ErrWire
			}
			seen = true
			message = payload
		case 128:
			for _, b := range payload {
				if b > 127 {
					return nil, 0, ErrWire
				}
			}
			for _, line := range strings.Split(string(payload), "\r\n") {
				key, value, ok := strings.Cut(line, ":")
				if ok && strings.EqualFold(key, "grpc-status") {
					value = strings.TrimSpace(value)
					if strict && hasStatus && status != value {
						return nil, 0, ErrWire
					}
					status = value
					hasStatus = true
				}
			}
		default:
			return nil, 0, ErrWire
		}
	}
	if !hasStatus || status == "" {
		return nil, 0, ErrWire
	}
	for _, c := range status {
		if c < '0' || c > '9' {
			return nil, 0, ErrWire
		}
	}
	code, err := strconv.Atoi(status)
	if err != nil {
		return nil, 0, ErrWire
	}
	return message, code, nil
}
