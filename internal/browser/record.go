package browser

import (
	"aa-server/internal/pulse"
	"context"
	"encoding/base64"
	pw "github.com/mxschmitt/playwright-go"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type diagnosticKey struct{}

func diagnosticURL(ctx context.Context, raw string) bool {
	allowed, _ := ctx.Value(diagnosticKey{}).(bool)
	u, err := url.Parse(raw)
	return allowed && err == nil && u.Scheme == "https" && u.User == nil && u.Hostname() == "attendance-app.mirea.ru" && (u.Port() == "" || u.Port() == "443")
}

var rpcMethod = regexp.MustCompile(`/(?:rtu[_a-z]*|rtu_tc)\.[A-Za-z0-9_.]+/[A-Z][A-Za-z0-9]+`)
var attendanceLink = regexp.MustCompile(`(?i)посещ|attend|журнал|visit|lesson|занят|отмет`)

func blockedDiagnostic(ctx context.Context, raw string) bool {
	enabled, _ := ctx.Value(diagnosticKey{}).(bool)
	if !enabled {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	p := strings.ToLower(u.Path)
	return strings.Contains(p, "logout") || strings.Contains(p, "selfapprove") || u.Path == pulse.Methods["attendance"] || u.Path == pulse.Methods["pass-code"] || u.Path == pulse.Methods["pass-issue"]
}

func (e *Engine) Record(parent context.Context, state map[string]any) (map[string]any, error) {
	if len(state) == 0 {
		return nil, &UserError{"У аккаунта нет сохранённого входа браузера"}
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, ErrClosed
	}
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()
	ctx, cancel := context.WithTimeout(context.WithValue(parent, diagnosticKey{}, true), 150*time.Second)
	defer cancel()
	stopRoot := context.AfterFunc(e.root, cancel)
	defer stopRoot()
	select {
	case e.renewals <- struct{}{}:
		defer func() { <-e.renewals }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c, err := e.newContext(state, ctx)
	if err != nil {
		return nil, err
	}
	var closeOnce sync.Once
	closeContext := func() { closeOnce.Do(func() { _ = c.Close() }) }
	stop := context.AfterFunc(ctx, closeContext)
	defer func() { stop(); closeContext() }()
	page, err := c.NewPage()
	if err != nil {
		return nil, err
	}
	queue := make(chan pw.Response, 64)
	var mu sync.Mutex
	calls := []any{}
	retainedBytes := 0
	methods := map[string]bool{}
	visited := []string{}
	work, stopWorker := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-work.Done():
				return
			case response := <-queue:
				headers := lowerHeaders(response.Headers())
				kind := headers["content-type"]
				if !strings.Contains(kind, "javascript") && !strings.Contains(kind, "grpc-web") {
					continue
				}
				body, err := response.Body()
				if err != nil {
					continue
				}
				if strings.Contains(kind, "javascript") {
					if len(body) > 8<<20 {
						body = body[:8<<20]
					}
					mu.Lock()
					for _, method := range rpcMethod.FindAllString(string(body), -1) {
						if len(methods) < 2000 {
							methods[method] = true
						}
					}
					mu.Unlock()
					continue
				}
				if strings.Contains(kind, "grpc-web-text") {
					body, err = base64.StdEncoding.DecodeString(string(body))
					if err != nil {
						continue
					}
				}
				if len(body) > pulse.MaxResponse {
					continue
				}
				h := http.Header{}
				for key, value := range headers {
					h.Set(key, value)
				}
				h.Set("content-type", "application/grpc-web+proto")
				message, status, err := pulse.Message(body, h)
				if err != nil {
					continue
				}
				request := response.Request()
				sent, _ := request.PostDataBuffer()
				if strings.Contains(request.Headers()["content-type"], "grpc-web-text") {
					sent, _ = base64.StdEncoding.DecodeString(string(sent))
				}
				sentTree := map[string]any{}
				if len(sent) >= 5 {
					sentTree = pulse.DecodeTree(sent[5:])
				}
				u, _ := url.Parse(response.URL())
				if u == nil {
					continue
				}
				mu.Lock()
				if len(calls) < 500 && retainedBytes+len(message)+len(sent) <= 8<<20 {
					retainedBytes += len(message) + len(sent)
					calls = append(calls, map[string]any{"host": u.Hostname(), "method": u.Path, "status": response.Status(), "grpc_status": status, "request": sentTree, "response": pulse.DecodeTree(message), "response_bytes": len(message)})
				}
				mu.Unlock()
			}
		}
	}()
	collect := func(response pw.Response) {
		select {
		case queue <- response:
		default:
		}
	}
	page.OnResponse(collect)
	defer func() { page.RemoveListener("response", collect); stopWorker(); closeContext(); <-done }()
	pending := []string{pulse.Host + "/services", "https://attendance-app.mirea.ru/"}
	seen := map[string]bool{}
	for len(pending) > 0 && len(visited) < 8 && ctx.Err() == nil {
		target := pending[0]
		pending = pending[1:]
		if seen[target] {
			continue
		}
		seen[target] = true
		visited = append(visited, target)
		if _, err = page.Goto(target, pw.PageGotoOptions{WaitUntil: pw.WaitUntilStateDomcontentloaded, Timeout: pw.Float(30000)}); err != nil {
			continue
		}
		if !sleep(ctx, 8*time.Second) {
			break
		}
		raw, err := page.Evaluate(`Array.from(document.querySelectorAll('a[href]'), n => [n.href,n.textContent||''])`)
		if err != nil {
			continue
		}
		links, _ := raw.([]any)
		for _, raw := range links {
			pair, ok := raw.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			href, _ := pair[0].(string)
			text, _ := pair[1].(string)
			u, err := url.Parse(href)
			if err == nil && (u.Hostname() == "pulse.mirea.ru" || u.Hostname() == "attendance-app.mirea.ru") && attendanceLink.MatchString(href+" "+text) && !seen[href] && len(pending) < 64 {
				pending = append(pending, href)
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	page.RemoveListener("response", collect)
	stopWorker()
	closeContext()
	<-done
	mu.Lock()
	defer mu.Unlock()
	names := []string{}
	for name := range methods {
		names = append(names, name)
	}
	sort.Strings(names)
	redacted := []string{}
	for _, raw := range visited {
		if u, err := url.Parse(raw); err == nil {
			redacted = append(redacted, u.Scheme+"://"+u.Host+u.EscapedPath())
		}
	}
	return map[string]any{"visited": redacted, "calls": calls, "methods": names, "known_mutations_blocked": true}, nil
}
