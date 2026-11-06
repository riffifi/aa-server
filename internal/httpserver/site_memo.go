package httpserver

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type memoItem[V any] struct {
	value V
	at    time.Time
	size  int
}

func (m *siteMemo[K, V]) peek(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[key]
	return item.value, ok
}
func (m *siteMemo[K, V]) stats() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{"cached": len(m.items), "checking": len(m.flights), "bytes": m.bytes, "ttl": 120}
}

type siteMemo[K comparable, V any] struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	items   map[K]memoItem[V]
	flights map[K]chan struct{}
	slots   chan struct{}
	wg      sync.WaitGroup
	closed  bool
	bytes   int
	load    func(context.Context, K) V
}

func newSiteMemo[K comparable, V any](ctx context.Context, load func(context.Context, K) V) *siteMemo[K, V] {
	root, cancel := context.WithCancel(ctx)
	return &siteMemo[K, V]{ctx: root, cancel: cancel, items: map[K]memoItem[V]{}, flights: map[K]chan struct{}{}, slots: make(chan struct{}, 4), load: load}
}
func (m *siteMemo[K, V]) get(ctx context.Context, key K) (V, bool) {
	m.mu.Lock()
	item, ok := m.items[key]
	if m.closed {
		m.mu.Unlock()
		return item.value, ok
	}
	if ok && time.Since(item.at) < 120*time.Second {
		m.mu.Unlock()
		return item.value, true
	}
	flight := m.flights[key]
	if flight == nil {
		select {
		case m.slots <- struct{}{}:
			flight = make(chan struct{})
			m.flights[key] = flight
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				defer func() { <-m.slots }()
				work, cancel := context.WithTimeout(m.ctx, 40*time.Second)
				defer cancel()
				value := m.load(work, key)
				encoded, _ := json.Marshal(value)
				m.mu.Lock()
				defer m.mu.Unlock()
				if !m.closed && len(encoded) <= 2<<20 {
					if old, exists := m.items[key]; exists {
						m.bytes -= old.size
						delete(m.items, key)
					}
					for len(m.items) > 0 && (len(m.items) >= 256 || m.bytes+len(encoded) > 32<<20) {
						var oldest K
						when := time.Now()
						for k, v := range m.items {
							if v.at.Before(when) {
								oldest, when = k, v.at
							}
						}
						m.bytes -= m.items[oldest].size
						delete(m.items, oldest)
					}
					m.items[key] = memoItem[V]{value, time.Now(), len(encoded)}
					m.bytes += len(encoded)
				}
				delete(m.flights, key)
				close(flight)
			}()
		default:
		}
	}
	m.mu.Unlock()
	if ok || flight == nil {
		return item.value, ok
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	case <-flight:
	}
	m.mu.Lock()
	item, ok = m.items[key]
	m.mu.Unlock()
	return item.value, ok
}
func (m *siteMemo[K, V]) close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	clear(m.items)
	m.bytes = 0
	m.mu.Unlock()
}
