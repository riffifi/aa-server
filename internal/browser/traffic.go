package browser

import (
	"context"
	"net/url"
	"sync"
	"time"

	pw "github.com/mxschmitt/playwright-go"
)

type TrafficBucket struct {
	Requests uint64 `json:"requests"`
	Bytes    uint64 `json:"encoded_response_bytes"`
	Failures uint64 `json:"failures"`
}
type Traffic struct {
	mu                 sync.Mutex
	started            time.Time
	buckets            map[string]TrafficBucket
	dropped            uint64
	apiBytes, apiCalls uint64
}

func newTraffic() *Traffic { return &Traffic{started: time.Now(), buckets: map[string]TrafficBucket{}} }
func (t *Traffic) add(key string, bytes int, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	bucket := t.buckets[key]
	bucket.Requests++
	if bytes > 0 {
		bucket.Bytes += uint64(bytes)
	}
	if failed {
		bucket.Failures++
	}
	t.buckets[key] = bucket
}
func (t *Traffic) api(bytes int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.apiCalls++
	if bytes > 0 {
		t.apiBytes += uint64(bytes)
	}
}
func (t *Traffic) Snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	copy := map[string]TrafficBucket{}
	for key, value := range t.buckets {
		copy[key] = value
	}
	return map[string]any{"started": t.started.UTC().Format(time.RFC3339), "page_network": copy, "dropped_measurements": t.dropped, "api_requests": t.apiCalls, "api_decoded_bytes": t.apiBytes, "note": "Page bytes use browser request sizes; static fetch/cache bytes are reported separately. API bytes are decoded payloads. These are response measurements, not host/interface accounting; no URLs, tokens or bodies are retained."}
}

func (e *Engine) Stats() map[string]any {
	e.mu.Lock()
	contexts := 0
	if e.browser != nil {
		contexts = len(e.browser.Contexts())
	}
	completed := e.completed
	running := e.browser != nil && e.browser.IsConnected()
	e.mu.Unlock()
	return map[string]any{"contexts": contexts, "running": running, "login_slots_free": cap(e.logins) - len(e.logins), "renew_slots_free": cap(e.renewals) - len(e.renewals), "completed_since_restart": completed, "asset_cache": e.assets.Stats(), "traffic": e.traffic.Snapshot()}
}
func trafficKey(r pw.Request) string {
	u, _ := url.Parse(r.URL())
	host := "other"
	if u != nil {
		switch u.Hostname() {
		case "pulse.mirea.ru":
			host = "pulse"
		case "sso.mirea.ru":
			host = "sso"
		default:
			host = "ddos_guard"
		}
	}
	kind := r.ResourceType()
	switch kind {
	case "document", "script", "stylesheet", "font", "image", "media", "fetch", "xhr":
	default:
		kind = "other"
	}
	return host + "/" + kind
}

// One bounded queue and one reader per live context; never one goroutine or a
// retained response body per network request.
func (t *Traffic) track(ctx context.Context, c pw.BrowserContext) (func(pw.Request), func()) {
	type completed struct {
		request pw.Request
		cached  bool
		failed  bool
	}
	queue := make(chan completed, 64)
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var cacheMu sync.Mutex
	cached := map[pw.Request]bool{}
	enqueue := func(request pw.Request, failed bool) {
		cacheMu.Lock()
		hit := cached[request]
		delete(cached, request)
		cacheMu.Unlock()
		select {
		case queue <- completed{request, hit, failed}:
		default:
			t.mu.Lock()
			t.dropped++
			t.mu.Unlock()
		}
	}
	finished := func(r pw.Request) { enqueue(r, false) }
	failed := func(r pw.Request) { enqueue(r, true) }
	c.OnRequestFinished(finished)
	c.OnRequestFailed(failed)
	go func() {
		defer close(done)
		for {
			select {
			case <-work.Done():
				return
			case item := <-queue:
				if item.cached {
					continue
				}
				size := 0
				if !item.failed {
					if sizes, err := item.request.Sizes(); err == nil {
						size = sizes.ResponseBodySize + sizes.ResponseHeadersSize
					}
				}
				t.add(trafficKey(item.request), size, item.failed)
			}
		}
	}()
	stop := func() {
		c.RemoveListener("requestfinished", finished)
		c.RemoveListener("requestfailed", failed)
		cancel()
		<-done
		cacheMu.Lock()
		clear(cached)
		cacheMu.Unlock()
	}
	return func(r pw.Request) { cacheMu.Lock(); cached[r] = true; cacheMu.Unlock() }, stop
}
