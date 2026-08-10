package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// MemoryCache is the in-process LRU cache (§9: "sync.RWMutex + doubly linked
// list"). Eviction keeps memory bounded — an unbounded cache would grow
// forever (anti-pattern table). The doubly linked list tracks access order:
// every Get/Set moves the entry to the front; the back is evicted when the
// cache exceeds maxEntries.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	max     int
}

// entry is the payload stored in the LRU list.
type entry struct {
	key       string
	resp      *schema.UnifiedResponse
	expiresAt time.Time
}

// NewMemory builds an LRU cache with the given entry cap.
func NewMemory(maxEntries int) *MemoryCache {
	return &MemoryCache{
		entries: make(map[string]*list.Element, maxEntries),
		order:   list.New(),
		max:     maxEntries,
	}
}

// Get retrieves an unexpired response and marks it most-recently-used.
func (c *MemoryCache) Get(ctx context.Context, key string) (*schema.UnifiedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if time.Now().After(e.expiresAt) {
		c.remove(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.resp, true
}

// Set stores or refreshes a response.
func (c *MemoryCache) Set(ctx context.Context, key string, resp *schema.UnifiedResponse, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*entry)
		e.resp = resp
		e.expiresAt = time.Now().Add(ttl)
		c.order.MoveToFront(el)
		return nil
	}
	el := c.order.PushFront(&entry{key: key, resp: resp, expiresAt: time.Now().Add(ttl)})
	c.entries[key] = el
	for c.order.Len() > c.max {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.remove(back)
	}
	return nil
}

// Close releases resources (no-op for the memory cache).
func (c *MemoryCache) Close() error { return nil }

// remove drops an element from both structures.
func (c *MemoryCache) remove(el *list.Element) {
	c.order.Remove(el)
	delete(c.entries, el.Value.(*entry).key)
}
