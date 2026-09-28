package kvstore

import (
	"slices"
	"sync"
)

// Bucket is one keyspace of a Store.
type Bucket struct {
	name string
	mu   sync.RWMutex
	data map[string][]byte
}

func newBucket(name string) *Bucket {
	return &Bucket{name: name, data: make(map[string][]byte)}
}

// Name returns the bucket's name.
func (b *Bucket) Name() string { return b.name }

// Put stores a copy of value under key.
func (b *Bucket) Put(key string, value []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data[key] = slices.Clone(value)
}

// Get returns a copy of the value stored under key.
func (b *Bucket) Get(key string) ([]byte, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.data[key]
	return slices.Clone(v), ok
}

// Delete removes key from the bucket.
func (b *Bucket) Delete(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.data, key)
}

// Keys returns the bucket's keys, sorted.
func (b *Bucket) Keys() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	keys := make([]string, 0, len(b.data))
	for k := range b.data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
