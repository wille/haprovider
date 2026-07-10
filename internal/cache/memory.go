package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

const (
	// MaxValueBytes is the largest response that will be cached. Larger responses
	// (e.g. a full block with all transactions on a high-throughput chain) are
	// passed through uncached rather than dominating the cache.
	MaxValueBytes = 5 * 1024 * 1024 // 5 MB

	// DefaultMaxBytes is the default total byte budget for a memory cache.
	DefaultMaxBytes = 128 * 1024 * 1024 // 128 MB

	// DefaultMaxEntries is the default maximum number of cached entries.
	DefaultMaxEntries = 50_000

	// janitorInterval is how often expired entries are proactively swept. The
	// byte/entry bounds cap memory regardless; the janitor just reclaims space
	// held by expired entries sooner.
	janitorInterval = time.Minute
)

type entry struct {
	key     string
	value   []byte
	expires time.Time
}

// size is the accounted byte cost of the entry (key + value). Map/list overhead
// per entry is bounded separately by the entry-count limit.
func (e *entry) size() int64 {
	return int64(len(e.key) + len(e.value))
}

// Memory is an in-memory Storage with LRU eviction bounded by both a total byte
// budget and an entry count. Responses larger than MaxValueBytes are never
// cached. All operations are O(1) under a single mutex.
type Memory struct {
	mu         sync.Mutex
	maxBytes   int64
	maxEntries int
	curBytes   int64
	ll         *list.List // front = most recently used; element values are *entry
	items      map[string]*list.Element
}

// NewMemory returns a bounded LRU cache and starts its janitor. Non-positive
// maxBytes/maxEntries fall back to DefaultMaxBytes/DefaultMaxEntries.
func NewMemory(maxBytes int64, maxEntries int) *Memory {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	m := &Memory{
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
		ll:         list.New(),
		items:      make(map[string]*list.Element),
	}
	go m.janitor()
	return m
}

// Len returns the current number of cached entries.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ll.Len()
}

// Bytes returns the current accounted byte usage.
func (m *Memory) Bytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.curBytes
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	el, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}

	e := el.Value.(*entry)
	if time.Now().After(e.expires) {
		m.removeElement(el)
		return nil, false, nil
	}

	m.ll.MoveToFront(el)
	return e.value, true, nil
}

func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	// Skip disabled TTLs and responses too large to cache.
	if ttl <= 0 || len(value) > MaxValueBytes {
		return nil
	}

	// An entry that alone would blow the byte budget is not worth caching (it
	// would evict everything else to make room for itself).
	if int64(len(key)+len(value)) > m.maxBytes {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	expires := time.Now().Add(ttl)

	if el, ok := m.items[key]; ok {
		e := el.Value.(*entry)
		m.curBytes += int64(len(value) - len(e.value))
		e.value = value
		e.expires = expires
		m.ll.MoveToFront(el)
	} else {
		e := &entry{key: key, value: value, expires: expires}
		m.items[key] = m.ll.PushFront(e)
		m.curBytes += e.size()
	}

	m.evict()
	return nil
}

// evict removes least-recently-used entries until both the byte and entry
// bounds are satisfied. Caller must hold m.mu.
func (m *Memory) evict() {
	for m.curBytes > m.maxBytes || m.ll.Len() > m.maxEntries {
		el := m.ll.Back()
		if el == nil {
			return
		}
		m.removeElement(el)
	}
}

// removeElement removes el from the list and index and updates the byte total.
// Caller must hold m.mu.
func (m *Memory) removeElement(el *list.Element) {
	e := el.Value.(*entry)
	m.ll.Remove(el)
	delete(m.items, e.key)
	m.curBytes -= e.size()
}

// janitor periodically removes expired entries.
func (m *Memory) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		for el := m.ll.Back(); el != nil; {
			prev := el.Prev()
			if now.After(el.Value.(*entry).expires) {
				m.removeElement(el)
			}
			el = prev
		}
		m.mu.Unlock()
	}
}
