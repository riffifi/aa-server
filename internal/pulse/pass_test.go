package pulse

import (
	"net/http"
	"testing"
)

func passReply(data []byte) ([]byte, http.Header) {
	return append(Frame(data, 0), Frame([]byte("grpc-status: 0\r\n"), 128)...), http.Header{"Content-Type": {"application/grpc-web+proto"}}
}

func TestDigitalPassWire(t *testing.T) {
	body, h := passReply(Field(1, []byte("synthetic.jwt.token")))
	if token, err := PassToken(body, h); err != nil || token != "synthetic.jwt.token" {
		t.Fatal(token, err)
	}
	for _, token := range []string{"", "contains space", "bad\r\nheader"} {
		body, h = passReply(Field(1, []byte(token)))
		if _, err := PassToken(body, h); err == nil {
			t.Fatal("accepted unsafe token")
		}
	}
	request, _, err := Message(append(PassIssueRequest("012345"), Frame([]byte("grpc-status: 0\r\n"), 128)...), h)
	fields, _ := Fields(request)
	device, _ := Fields(fields[2].([]byte))
	if err != nil || string(fields[1].([]byte)) != "012345" || string(device[1].([]byte)) != "AntiAttendance" {
		t.Fatal(fields, err)
	}
	for _, code := range []string{"12345", "1234567", "１２３４５６", "12a456", " 12345"} {
		if ValidPassCode(code) {
			t.Fatal(code)
		}
	}
	if !ValidPassCode("012345") {
		t.Fatal("lost leading zero")
	}
	for _, data := range [][]byte{nil, Field(1, nil), VField(1, 0), VField(1, 1<<63)} {
		if _, err := PassNumber(data); err == nil {
			t.Fatal("accepted invalid number", data)
		}
	}
	if number, err := PassNumber(VField(1, 9223372036854775807)); err != nil || number != "9223372036854775807" {
		t.Fatal(number, err)
	}
	for _, data := range [][]byte{nil, append(Field(1, nil), Field(2, nil)...), append(Field(1, nil), Field(1, nil)...), VField(1, 2), Field(4, nil)} {
		body, h = passReply(data)
		if _, _, err := PassResult(body, h); err == nil {
			t.Fatal("accepted malformed oneof", data)
		}
	}
	if retry, err := PassRetryAt(Field(1, VField(1, 1794300000)), true); err != nil || retry != int64(1794300000) {
		t.Fatal(retry, err)
	}
}
