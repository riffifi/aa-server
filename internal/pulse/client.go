package pulse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var cookieName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
var ErrCookie = errors.New("Invalid session header")
var ErrResponseLimit = errors.New("Upstream response exceeds limit")

func ValidateCookie(header string) error {
	if header == "" || len(header) > 65536 {
		return ErrCookie
	}
	seen := map[string]bool{}
	for _, item := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || seen[name] || !cookieName.MatchString(name) || value == "" {
			return ErrCookie
		}
		for _, c := range value {
			if c < 33 || c > 126 || strings.ContainsRune("\",;\\", c) {
				return ErrCookie
			}
		}
		seen[name] = true
	}
	return nil
}

type Reply struct {
	Status  int
	Headers http.Header
	Body    []byte
}
type Transport struct{ HTTP *http.Client }

func NewTransport(roundTripper http.RoundTripper) *Transport {
	if roundTripper == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 32
		transport.MaxConnsPerHost = 64
		roundTripper = transport
	}
	return &Transport{HTTP: &http.Client{Timeout: 15 * time.Second, Transport: roundTripper, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (t *Transport) Close() { t.HTTP.CloseIdleConnections() }
func (t *Transport) RPC(ctx context.Context, cookie, method string, body []byte, authorization string) (Reply, error) {
	if err := ValidateCookie(cookie); err != nil {
		return Reply{}, err
	}
	path, ok := Methods[method]
	if !ok {
		return Reply{}, errors.New("RPC not allowed")
	}
	request, err := http.NewRequestWithContext(ctx, "POST", Host+path, bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	request.Header.Set("Content-Type", "application/grpc-web+proto")
	request.Header.Set("Accept", "application/grpc-web+proto")
	request.Header.Set("X-Grpc-Web", "1")
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.Header.Set("Origin", Host)
	request.Header.Set("Referer", Host+"/services")
	request.Header.Set("Cookie", cookie)
	if authorization != "" {
		request.Header.Set("Authorization", "Bearer "+authorization)
	}
	response, err := t.HTTP.Do(request)
	if err != nil {
		return Reply{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxResponse+1))
	if err != nil {
		return Reply{}, err
	}
	if len(data) > MaxResponse {
		return Reply{}, ErrResponseLimit
	}
	return Reply{response.StatusCode, response.Header.Clone(), data}, nil
}
func (t *Transport) Identity(ctx context.Context, cookie string) (string, string) {
	reply, err := t.RPC(ctx, cookie, "profile", ProfileRequest(), "")
	if err != nil {
		return "unavailable", ""
	}
	return AuthenticationState(reply.Status, reply.Headers, reply.Body)
}

// Profile preserves response headers for server-side sliding session renewal.
func (t *Transport) Profile(ctx context.Context, cookie string) (Reply, error) {
	return t.RPC(ctx, cookie, "profile", ProfileRequest(), "")
}
func (t *Transport) Logout(ctx context.Context, cookie string) error {
	if err := ValidateCookie(cookie); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "GET", Host+"/api/auth/logout?redirectUri=https%3A%2F%2Fpulse.mirea.ru", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Cookie", cookie)
	response, err := t.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, MaxResponse))
	return err
}
