// Package lrucache is a fixed-size, least-recently-used cache used by the
// thumbnail service to keep decoded images in memory.
package lrucache

import (
	"container/list"
	"sync"
)

// Cache holds at most Cap entries; adding one more evicts the entry that was
// used least recently. Get and Put both count as a use. It is safe for
// concurrent use.
type Cache[V any] struct {
	mu      sync.Mutex
	cap     int
	ll      *list.List // front = most recently used
	items   map[string]*list.Element
	onEvict func(key string, value V)
}

type entry[V any] struct {
	key   string
	value V
}

// New returns an empty cache holding at most capacity entries. onEvict, when
// not nil, is called with each evicted entry (under the cache's lock).
func New[V any](capacity int, onEvict func(key string, value V)) *Cache[V] {
	if capacity < 1 {
		panic("lrucache: capacity must be at least 1")
	}
	return &Cache[V]{cap: capacity, ll: list.New(), items: make(map[string]*list.Element), onEvict: onEvict}
}

// Get returns the value for key and marks it as the most recently used.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*entry[V]).value, true
	}
	var zero V
	return zero, false
}

// Put adds or replaces the value for key and marks it as the most recently
// used, evicting the least recently used entry when the cache is over
// capacity.
func (c *Cache[V]) Put(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.Value.(*entry[V]).value = value
		c.ll.MoveToFront(e)
		return
	}
	c.items[key] = c.ll.PushFront(&entry[V]{key: key, value: value})
	if c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, key)
		if c.onEvict != nil {
			ev := oldest.Value.(*entry[V])
			c.onEvict(ev.key, ev.value)
		}
	}
}

// Remove deletes key, reporting whether it was present.
func (c *Cache[V]) Remove(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return false
	}
	c.ll.Remove(e)
	delete(c.items, key)
	return true
}

// Len returns the number of entries in the cache.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Keys returns the keys from most to least recently used.
func (c *Cache[V]) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, c.ll.Len())
	for e := c.ll.Front(); e != nil; e = e.Next() {
		keys = append(keys, e.Value.(*entry[V]).key)
	}
	return keys
}
