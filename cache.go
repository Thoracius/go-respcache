package respcache

import (
	"container/list"
	"sync"
	"time"
)

// Cache stores finished responses keyed by request. Implementations must be
// safe for concurrent use.
type Cache interface {
	Get(key string) (*Entry, bool)
	Set(key string, e *Entry)
	Delete(key string)
}

// MemoryCache is an in-memory LRU Cache. Expired entries are dropped on
// read, and Set sweeps for expired entries at most once per SweepInterval.
// When MaxEntries or MaxBytes is exceeded the least recently used entries
// are evicted.
type MemoryCache struct {
	// MaxEntries bounds the number of stored entries. Zero means unbounded.
	MaxEntries int
	// MaxBytes bounds the total body bytes (raw plus gzipped) stored.
	// Zero means unbounded. A single entry larger than MaxBytes is never
	// stored.
	MaxBytes int64
	// SweepInterval controls how often a Set scans for expired entries.
	// Zero means once a minute.
	SweepInterval time.Duration

	mu        sync.Mutex
	m         map[string]*list.Element
	lru       *list.List // front = most recently used
	bytes     int64
	lastSweep time.Time
}

type lruItem struct {
	key   string
	entry *Entry
	size  int64
}

// NewMemoryCache returns an empty, unbounded MemoryCache. Set MaxEntries
// and/or MaxBytes on the result to bound it.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{m: make(map[string]*list.Element), lru: list.New()}
}

// Size reports how many bytes an entry occupies for MaxBytes accounting.
func (e *Entry) Size() int64 { return int64(len(e.Body) + len(e.Gzipped)) }

// Get returns the unexpired entry for key and marks it recently used.
func (c *MemoryCache) Get(key string) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*lruItem)
	if it.entry.Expired(time.Now()) {
		c.remove(el)
		return nil, false
	}
	c.lru.MoveToFront(el)
	return it.entry, true
}

// Set stores e under key, evicting as needed.
func (c *MemoryCache) Set(key string, e *Entry) {
	size := e.Size()
	if c.MaxBytes > 0 && size > c.MaxBytes {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.m[key]; ok {
		c.remove(el)
	}
	el := c.lru.PushFront(&lruItem{key: key, entry: e, size: size})
	c.m[key] = el
	c.bytes += size

	interval := c.SweepInterval
	if interval == 0 {
		interval = time.Minute
	}
	if now.Sub(c.lastSweep) > interval {
		for el := c.lru.Back(); el != nil; {
			prev := el.Prev()
			if el.Value.(*lruItem).entry.Expired(now) {
				c.remove(el)
			}
			el = prev
		}
		c.lastSweep = now
	}

	for c.lru.Len() > 1 &&
		((c.MaxEntries > 0 && c.lru.Len() > c.MaxEntries) ||
			(c.MaxBytes > 0 && c.bytes > c.MaxBytes)) {
		c.remove(c.lru.Back())
	}
}

// remove unlinks el. Caller holds c.mu.
func (c *MemoryCache) remove(el *list.Element) {
	it := el.Value.(*lruItem)
	c.lru.Remove(el)
	delete(c.m, it.key)
	c.bytes -= it.size
}

// Delete removes key.
func (c *MemoryCache) Delete(key string) {
	c.mu.Lock()
	if el, ok := c.m[key]; ok {
		c.remove(el)
	}
	c.mu.Unlock()
}

// Flush removes every entry.
func (c *MemoryCache) Flush() {
	c.mu.Lock()
	c.m = make(map[string]*list.Element)
	c.lru.Init()
	c.bytes = 0
	c.mu.Unlock()
}

// Len returns the number of stored entries, expired or not.
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Bytes returns the total body bytes currently stored.
func (c *MemoryCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
