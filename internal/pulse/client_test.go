package pulse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closedBody struct {
	io.Reader
	closed bool
}

func (b *closedBody) Close() error { b.closed = true; return nil }

func TestRPCUsesOnlyAllowedEndpointAndClosesResponse(t *testing.T) {
	body := &closedBody{Reader: strings.NewReader("response")}
	calls := 0
	transport := NewTransport(roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != Host+Methods["profile"] || r.Method != "POST" || r.Header.Get("Cookie") != "session=synthetic" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("upstream request changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/grpc-web+proto"}}, Body: body}, nil
	}))
	reply, err := transport.RPC(context.Background(), "session=synthetic", "profile", Frame(nil, 0), "token")
	if err != nil || string(reply.Body) != "response" || !body.closed {
		t.Fatalf("RPC read/close failed: %v", err)
	}
	if _, err = transport.RPC(context.Background(), "session=synthetic", "https://untrusted.invalid", nil, ""); err == nil {
		t.Fatal("arbitrary endpoint accepted")
	}
	if _, err = transport.RPC(context.Background(), "session=bad\r\nheader=x", "profile", nil, ""); err == nil {
		t.Fatal("cookie injection accepted")
	}
	if calls != 1 {
		t.Fatal("invalid input reached network")
	}
}
func TestRPCBoundsBodyAndRetainsNoReader(t *testing.T) {
	body := &closedBody{Reader: strings.NewReader(strings.Repeat("x", MaxResponse+1))}
	transport := NewTransport(roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
	}))
	_, err := transport.RPC(context.Background(), "session=synthetic", "profile", nil, "")
	if !errors.Is(err, ErrResponseLimit) || !body.closed {
		t.Fatal("oversized response not rejected/closed")
	}
}
func TestCookieValidation(t *testing.T) {
	for _, header := range []string{"a=b", "a=b; c=d==", ".AspNetCore.Cookies=token"} {
		if err := ValidateCookie(header); err != nil {
			t.Fatalf("valid cookie rejected: %q", header)
		}
	}
	for _, header := range []string{"", "a=b; a=c", "a=b;", "a=", "a=b,c", "a=b\nc=d", "a=тест", "bad name=x"} {
		if err := ValidateCookie(header); err == nil {
			t.Fatalf("invalid cookie accepted: %q", header)
		}
	}
}
