package browser

import (
	"context"
	pw "github.com/mxschmitt/playwright-go"
	"sync"
	"testing"
	"time"
)

type assetRequest struct {
	pw.Request
	url string
}

func TestConcurrentContextsShareOneAssetDownload(t *testing.T) {
	c := NewAssets()
	routes := make([]*assetRoute, 20)
	var workers sync.WaitGroup
	for i := range routes {
		routes[i] = &assetRoute{request: assetRequest{url: "https://pulse.mirea.ru/shared.js"}, response: &assetResponse{status: 200, body: make([]byte, 512<<10), headers: map[string]string{"cache-control": "public, max-age=3600"}}}
		workers.Add(1)
		go func(route *assetRoute) {
			defer workers.Done()
			if err := c.Route(context.Background(), route, nil); err != nil {
				t.Error(err)
			}
		}(routes[i])
	}
	workers.Wait()
	fetches := 0
	for _, route := range routes {
		fetches += route.fetches
	}
	if fetches != 1 || c.Stats().Entries != 1 {
		t.Fatal("duplicate asset downloads", fetches)
	}
}

func (r assetRequest) URL() string                            { return r.url }
func (r assetRequest) Method() string                         { return "GET" }
func (r assetRequest) ResourceType() string                   { return "script" }
func (r assetRequest) AllHeaders() (map[string]string, error) { return map[string]string{}, nil }

type assetResponse struct {
	pw.APIResponse
	body             []byte
	headers          map[string]string
	status, disposed int
}

func (r *assetResponse) Body() ([]byte, error)      { return r.body, nil }
func (r *assetResponse) Headers() map[string]string { return r.headers }
func (r *assetResponse) Status() int                { return r.status }
func (r *assetResponse) Dispose() error             { r.disposed++; return nil }

type assetRoute struct {
	pw.Route
	request   assetRequest
	response  *assetResponse
	fetches   int
	options   pw.RouteFetchOptions
	fulfilled pw.RouteFulfillOptions
}

func (r *assetRoute) Request() pw.Request { return r.request }
func (r *assetRoute) Fetch(options ...pw.RouteFetchOptions) (pw.APIResponse, error) {
	r.fetches++
	r.options = options[0]
	return r.response, nil
}
func (r *assetRoute) Fulfill(options ...pw.RouteFulfillOptions) error {
	r.fulfilled = options[0]
	return nil
}
func (r *assetRoute) Abort(...string) error { return nil }
func TestAssetsReusePastRenewalIntervalAndRevalidate(t *testing.T) {
	c := NewAssets()
	now := time.Now()
	c.Now = func() time.Time { return now }
	response := &assetResponse{status: 200, body: []byte("bundle"), headers: map[string]string{"cache-control": "public, max-age=3600", "etag": "bundle-v1", "content-encoding": "gzip"}}
	first := &assetRoute{request: assetRequest{url: "https://pulse.mirea.ru/app.js"}, response: response}
	if err := c.Route(context.Background(), first, nil); err != nil {
		t.Fatal(err)
	}
	if response.disposed != 1 {
		t.Fatal("response retained")
	}
	now = now.Add(901 * time.Second)
	hit := &assetRoute{request: first.request}
	if err := c.Route(context.Background(), hit, nil); err != nil {
		t.Fatal(err)
	}
	if hit.fetches != 0 || string(hit.fulfilled.Body.([]byte)) != "bundle" {
		t.Fatal("bundle downloaded again after renewal interval")
	}
	if _, ok := hit.fulfilled.Headers["content-encoding"]; ok {
		t.Fatal("decoded bytes replayed as gzip")
	}
	now = now.Add(time.Hour)
	revalidated := &assetRoute{request: first.request, response: &assetResponse{status: 304, headers: map[string]string{}}}
	if err := c.Route(context.Background(), revalidated, nil); err != nil {
		t.Fatal(err)
	}
	if revalidated.options.Headers["if-none-match"] != "bundle-v1" || revalidated.response.disposed != 1 || c.Stats().Revalidations != 1 {
		t.Fatal("conditional revalidation failed")
	}
	c.Clear()
	if c.Stats().Bytes != 0 {
		t.Fatal("cache retained bytes after clear")
	}
}
func TestAssetsRejectPrivateAndBoundMemory(t *testing.T) {
	for _, headers := range []map[string]string{{"cache-control": "private, max-age=3600"}, {"cache-control": "public, max-age=3600", "set-cookie": "secret=x"}, {"cache-control": "public, max-age=3600", "vary": "Cookie"}} {
		if assetTTL(headers) != 0 {
			t.Fatal("private content became shared")
		}
	}
	if assetURL("https://sso.mirea.ru/app.js", "GET", "script") || assetURL("https://pulse.mirea.ru/api/profile", "GET", "fetch") {
		t.Fatal("authenticated endpoint eligible")
	}
	c := NewAssets()
	c.Budget = 10
	c.MaxItem = 8
	c.MaxEntries = 2
	for _, key := range []string{"a", "b", "c"} {
		c.put(key, asset{body: make([]byte, 8)})
	}
	if c.size > 10 || len(c.entries) > 2 {
		t.Fatal("cache exceeded bounds")
	}
}

func TestLargePublicFrameworkAssetIsReused(t *testing.T) {
	c := NewAssets()
	first := &assetRoute{request: assetRequest{url: "https://pulse.mirea.ru/_framework/dotnet.native.wasm"}, response: &assetResponse{status: 200, body: make([]byte, 12<<20), headers: map[string]string{"cache-control": "public, max-age=86400"}}}
	if err := c.Route(context.Background(), first, nil); err != nil {
		t.Fatal(err)
	}
	second := &assetRoute{request: first.request}
	if err := c.Route(context.Background(), second, nil); err != nil {
		t.Fatal(err)
	}
	if second.fetches != 0 || c.Stats().Bytes != 12<<20 || first.response.disposed != 1 {
		t.Fatal("large framework asset was re-downloaded or retained as a response")
	}
}
