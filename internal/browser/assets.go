package browser

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	pw "github.com/mxschmitt/playwright-go"
)

type asset struct {
	body    []byte
	headers map[string]string
	expires time.Time
	used    uint64
}
type AssetStats struct {
	Entries            int    `json:"entries"`
	Bytes              int    `json:"bytes"`
	Hits               uint64 `json:"hits"`
	Misses             uint64 `json:"misses"`
	Revalidations      uint64 `json:"revalidations"`
	SavedBytes         uint64 `json:"saved_decoded_bytes"`
	DownloadedBytes    uint64 `json:"downloaded_decoded_bytes"`
	Fetches            uint64 `json:"fetches"`
	ContentLengthBytes uint64 `json:"content_length_bytes"`
	UnknownLengths     uint64 `json:"unknown_content_lengths"`
}
type Assets struct {
	slots                       chan struct{}
	mu                          sync.Mutex
	entries                     map[string]asset
	flights                     map[string]chan struct{}
	size                        int
	tick                        uint64
	stats                       AssetStats
	Now                         func() time.Time
	Budget, MaxItem, MaxEntries int
}

func NewAssets() *Assets {
	return &Assets{entries: map[string]asset{}, flights: map[string]chan struct{}{}, slots: make(chan struct{}, 4), Now: time.Now, Budget: 64 << 20, MaxItem: 32 << 20, MaxEntries: 128}
}
func (c *Assets) Stats() AssetStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	stats := c.stats
	stats.Entries = len(c.entries)
	stats.Bytes = c.size
	return stats
}
func (c *Assets) Clear() { c.mu.Lock(); defer c.mu.Unlock(); clear(c.entries); c.size = 0 }
func assetURL(raw, method, kind string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() != "pulse.mirea.ru" || u.User != nil || method != "GET" {
		return false
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch kind {
	case "script", "stylesheet", "font":
		switch ext {
		case ".js", ".css", ".woff", ".woff2", ".ttf":
			return true
		}
	}
	// Blazor/WebAssembly runtimes fetch large public binaries outside script tags.
	return strings.HasPrefix(u.Path, "/_framework/") && (ext == ".wasm" || ext == ".dll" || ext == ".dat" || ext == ".bin" || ext == ".json")
}
func lowerHeaders(raw map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range raw {
		out[strings.ToLower(key)] = value
	}
	return out
}
func assetTTL(headers map[string]string) time.Duration {
	control := headers["cache-control"]
	directives := map[string]string{}
	for _, part := range strings.Split(strings.ToLower(control), ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		directives[key] = strings.Trim(value, "\"")
	}
	if _, ok := directives["public"]; !ok {
		return 0
	}
	for _, key := range []string{"private", "no-store", "no-cache"} {
		if _, ok := directives[key]; ok {
			return 0
		}
	}
	if _, ok := headers["set-cookie"]; ok {
		return 0
	}
	vary := strings.ToLower(strings.TrimSpace(headers["vary"]))
	if vary != "" && vary != "accept-encoding" {
		return 0
	}
	seconds, err := strconv.ParseInt(directives["max-age"], 10, 64)
	if err != nil || seconds < 0 {
		return 0
	}
	age := int64(0)
	if text := headers["age"]; text != "" {
		age, err = strconv.ParseInt(text, 10, 64)
		if err != nil || age < 0 {
			return 0
		}
	}
	if age >= seconds {
		return 0
	}
	seconds -= age
	if seconds > 86400 {
		seconds = 86400
	}
	return time.Duration(seconds) * time.Second
}
func replayHeaders(raw map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range raw {
		switch key {
		case "content-encoding", "content-length", "transfer-encoding", "connection":
		default:
			out[key] = value
		}
	}
	return out
}
func (c *Assets) remove(key string) {
	if previous, ok := c.entries[key]; ok {
		c.size -= len(previous.body)
		delete(c.entries, key)
	}
}
func (c *Assets) put(key string, value asset) {
	c.remove(key)
	if len(value.body) > c.Budget || len(value.body) > c.MaxItem || c.MaxEntries < 1 {
		return
	}
	for len(c.entries) > 0 && (c.size+len(value.body) > c.Budget || len(c.entries) >= c.MaxEntries) {
		var oldest string
		age := ^uint64(0)
		for key, item := range c.entries {
			if item.used < age {
				oldest, age = key, item.used
			}
		}
		c.remove(oldest)
	}
	c.tick++
	value.used = c.tick
	c.entries[key] = value
	c.size += len(value.body)
}

// Route only shares explicitly public static assets. A per-URL flight prevents
// simultaneous logins from downloading the same bundle repeatedly.
func (c *Assets) Route(ctx context.Context, route pw.Route, hit func(pw.Request)) error {
	request := route.Request()
	if !assetURL(request.URL(), request.Method(), request.ResourceType()) {
		return route.Continue()
	}
	key := request.URL()
	var previous asset
	for {
		c.mu.Lock()
		previous = c.entries[key]
		if previous.body != nil && c.Now().Before(previous.expires) {
			c.tick++
			previous.used = c.tick
			c.entries[key] = previous
			c.stats.Hits++
			c.stats.SavedBytes += uint64(len(previous.body))
			c.mu.Unlock()
			if hit != nil {
				hit(request)
			}
			return route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), Headers: previous.headers, Body: previous.body})
		}
		if flight := c.flights[key]; flight != nil {
			c.mu.Unlock()
			select {
			case <-flight:
				continue
			case <-ctx.Done():
				return route.Abort()
			}
		}
		c.flights[key] = make(chan struct{})
		c.stats.Misses++
		c.mu.Unlock()
		break
	}
	defer func() { c.mu.Lock(); close(c.flights[key]); delete(c.flights, key); c.mu.Unlock() }()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return route.Abort()
	}
	headers, err := request.AllHeaders()
	if err != nil {
		return route.Abort()
	}
	headers = lowerHeaders(headers)
	conditional := false
	if previous.body != nil {
		if value := previous.headers["etag"]; value != "" {
			headers["if-none-match"] = value
			conditional = true
		} else if value := previous.headers["last-modified"]; value != "" {
			headers["if-modified-since"] = value
			conditional = true
		}
	}
	response, err := route.Fetch(pw.RouteFetchOptions{Headers: headers, MaxRedirects: pw.Int(0), Timeout: pw.Float(15000)})
	if err != nil {
		return route.Abort()
	}
	c.recordTransfer(response)
	if conditional && response.Status() == http.StatusNotModified {
		merged := map[string]string{}
		for key, value := range previous.headers {
			merged[key] = value
		}
		for key, value := range lowerHeaders(response.Headers()) {
			merged[key] = value
		}
		if _, ok := response.Headers()["age"]; !ok {
			delete(merged, "age")
		}
		ttl := assetTTL(merged)
		if ttl > 0 {
			_ = response.Dispose()
			previous.expires = c.Now().Add(ttl)
			previous.headers = replayHeaders(merged)
			c.mu.Lock()
			c.put(key, previous)
			c.stats.Revalidations++
			c.stats.SavedBytes += uint64(len(previous.body))
			c.mu.Unlock()
			if hit != nil {
				hit(request)
			}
			return route.Fulfill(pw.RouteFulfillOptions{Status: pw.Int(200), Headers: previous.headers, Body: previous.body})
		}
		_ = response.Dispose()
		delete(headers, "if-none-match")
		delete(headers, "if-modified-since")
		response, err = route.Fetch(pw.RouteFetchOptions{Headers: headers, MaxRedirects: pw.Int(0), Timeout: pw.Float(15000)})
		if err != nil {
			return route.Abort()
		}
		c.recordTransfer(response)
	}
	defer response.Dispose()
	responseHeaders := lowerHeaders(response.Headers())
	ttl := assetTTL(responseHeaders)
	if response.Status() == 200 && ttl > 0 {
		length, _ := strconv.ParseInt(responseHeaders["content-length"], 10, 64)
		if length <= int64(c.MaxItem) {
			body, err := response.Body()
			if err != nil {
				return route.Abort()
			}
			c.mu.Lock()
			c.stats.DownloadedBytes += uint64(len(body))
			if len(body) <= c.MaxItem {
				c.put(key, asset{body: body, headers: replayHeaders(responseHeaders), expires: c.Now().Add(ttl)})
			}
			c.mu.Unlock()
		}
	} else {
		c.mu.Lock()
		c.remove(key)
		c.mu.Unlock()
	}
	if hit != nil {
		hit(request)
	}
	return route.Fulfill(pw.RouteFulfillOptions{Response: response})
}

func (c *Assets) recordTransfer(response pw.APIResponse) {
	headers := lowerHeaders(response.Headers())
	size, err := strconv.ParseUint(headers["content-length"], 10, 64)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Fetches++
	if err == nil {
		c.stats.ContentLengthBytes += size
	} else if response.Status() != 304 {
		c.stats.UnknownLengths++
	}
}
