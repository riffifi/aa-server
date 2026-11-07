package schedule

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Fetch func(context.Context, string) (string, error)
type cacheEntry struct {
	Text            string
	Fetched, Failed time.Time
	Used            uint64
}
type flight struct {
	Done  chan struct{}
	Entry cacheEntry
}
type Cache struct {
	Fetch                Fetch
	Now                  func() time.Time
	TTL, Retry           time.Duration
	MaxBytes, MaxEntries int
	mu                   sync.Mutex
	entries              map[int64]cacheEntry
	flights              map[int64]*flight
	size                 int
	tick                 uint64
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	closed               bool
	client               *http.Client
	slots                chan struct{}
}

func NewCache(ctx context.Context, fetch Fetch) *Cache {
	root, cancel := context.WithCancel(ctx)
	cache := &Cache{Fetch: fetch, Now: time.Now, TTL: 15 * time.Minute, Retry: time.Minute, MaxBytes: 32 << 20, MaxEntries: 512, entries: map[int64]cacheEntry{}, flights: map[int64]*flight{}, ctx: root, cancel: cancel, slots: make(chan struct{}, 16)}
	if fetch == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxConnsPerHost = 16
		transport.MaxIdleConnsPerHost = 16
		cache.client = &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		cache.Fetch = func(ctx context.Context, endpoint string) (string, error) {
			request, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
			if err != nil {
				return "", err
			}
			response, err := cache.client.Do(request)
			if err != nil {
				return "", err
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				return "", nil
			}
			body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
			if err != nil || len(body) > 2<<20 {
				return "", err
			}
			return strings.ToValidUTF8(string(body), "\uFFFD"), nil
		}
	}
	return cache
}
func (c *Cache) Close() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
	c.mu.Lock()
	clear(c.entries)
	c.size = 0
	c.mu.Unlock()
	if c.client != nil {
		c.client.CloseIdleConnections()
	}
}

func (c *Cache) Stats() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []map[string]any{}
	seen := map[int64]bool{}
	for id, entry := range c.entries {
		var fetched, failed any
		if !entry.Fetched.IsZero() {
			fetched = float64(entry.Fetched.UnixMilli()) / 1000
		}
		if !entry.Failed.IsZero() {
			failed = float64(entry.Failed.UnixMilli()) / 1000
		}
		out = append(out, map[string]any{"group_id": id, "fetched": fetched, "failed": failed, "size": len(entry.Text), "parsed": 0, "weeks": 0, "checking": c.flights[id] != nil})
		seen[id] = true
	}
	for id := range c.flights {
		if !seen[id] {
			out = append(out, map[string]any{"group_id": id, "fetched": nil, "failed": nil, "size": 0, "parsed": 0, "weeks": 0, "checking": true})
		}
	}
	return out
}
func (c *Cache) put(id int64, entry cacheEntry) {
	if previous, ok := c.entries[id]; ok {
		c.size -= len(previous.Text)
		delete(c.entries, id)
	}
	if len(entry.Text) > c.MaxBytes || c.MaxEntries < 1 {
		return
	}
	for len(c.entries) > 0 && (c.size+len(entry.Text) > c.MaxBytes || len(c.entries) >= c.MaxEntries) {
		var oldest int64
		var age uint64 = ^uint64(0)
		for key, item := range c.entries {
			if item.Used < age {
				oldest, age = key, item.Used
			}
		}
		c.size -= len(c.entries[oldest].Text)
		delete(c.entries, oldest)
	}
	c.tick++
	entry.Used = c.tick
	c.entries[id] = entry
	c.size += len(entry.Text)
}
func (c *Cache) load(id int64, work *flight, previous cacheEntry) {
	defer c.wg.Done()
	defer func() { <-c.slots }()
	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	text, err := c.Fetch(ctx, ICALURL(id))
	now := c.Now()
	entry := previous
	if err != nil || text == "" || len(text) > 2<<20 || !strings.Contains(text, "BEGIN:VCALENDAR") {
		entry.Failed = now
	} else {
		entry = cacheEntry{Text: text, Fetched: now}
	}
	c.mu.Lock()
	if !c.closed {
		c.put(id, entry)
	}
	work.Entry = entry
	delete(c.flights, id)
	close(work.Done)
	c.mu.Unlock()
}

// Source serves a saved calendar immediately while exactly one download renews it.
// A cancelled first reader never cancels a download shared by other readers.
func (c *Cache) Source(ctx context.Context, id int64) (string, time.Time) {
	return c.source(ctx, id, false)
}
func (c *Cache) SourceWait(ctx context.Context, id int64) (string, time.Time) {
	return c.source(ctx, id, true)
}
func (c *Cache) source(ctx context.Context, id int64, wait bool) (string, time.Time) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", time.Time{}
	}
	now := c.Now()
	entry, exists := c.entries[id]
	age := now.Sub(entry.Fetched)
	fresh := exists && (!entry.Fetched.IsZero() && (age < c.TTL || wait && age == c.TTL) || !entry.Failed.IsZero() && now.Sub(entry.Failed) < c.Retry)
	if fresh {
		c.tick++
		entry.Used = c.tick
		c.entries[id] = entry
		c.mu.Unlock()
		return entry.Text, entry.Fetched
	}
	work := c.flights[id]
	if work == nil {
		select {
		case c.slots <- struct{}{}:
		default:
			c.mu.Unlock()
			return entry.Text, entry.Fetched
		}
		work = &flight{Done: make(chan struct{})}
		c.flights[id] = work
		c.wg.Add(1)
		go c.load(id, work, entry)
	}
	if entry.Text != "" && !wait {
		c.mu.Unlock()
		return entry.Text, entry.Fetched
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", time.Time{}
	case <-work.Done:
		return work.Entry.Text, work.Entry.Fetched
	}
}
func (c *Cache) Lessons(ctx context.Context, id int64, day time.Time) ([]Lesson, bool) {
	source, _ := c.Source(ctx, id)
	if source == "" {
		return nil, false
	}
	return On(source, day), true
}

// Authorization never falls back to an expired timetable during an outage.
func (c *Cache) FreshLessons(ctx context.Context, id int64, day time.Time) ([]Lesson, bool) {
	text, at := c.SourceWait(ctx, id)
	if text == "" || at.IsZero() || c.Now().Sub(at) > c.TTL {
		return nil, false
	}
	return On(text, day), true
}
func (c *Cache) Week(ctx context.Context, id int64, monday time.Time) (map[string][]Lesson, bool) {
	source, _ := c.Source(ctx, id)
	if source == "" {
		return nil, false
	}
	return Between(source, monday, 7), true
}
