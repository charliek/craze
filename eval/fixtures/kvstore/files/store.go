// Package kvstore is an in-memory key-value store. Keys live in buckets: two
// buckets may hold the same key with different values.
package kvstore

import (
	"errors"
	"slices"
	"sync"
)

var (
	// ErrNoBucket is returned for a bucket that was never created or was deleted.
	ErrNoBucket = errors.New("kvstore: no such bucket")
	// ErrBucketExists is returned by CreateBucket for a name already in use.
	ErrBucketExists = errors.New("kvstore: bucket already exists")
)

// Store holds the buckets. It is safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	buckets map[string]*Bucket
}

// New returns an empty store.
func New() *Store {
	return &Store{buckets: make(map[string]*Bucket)}
}

// CreateBucket makes a new, empty bucket.
func (s *Store) CreateBucket(name string) (*Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; ok {
		return nil, ErrBucketExists
	}
	b := newBucket(name)
	s.buckets[name] = b
	return b, nil
}

// Bucket returns the bucket called name.
func (s *Store) Bucket(name string) (*Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[name]
	if !ok {
		return nil, ErrNoBucket
	}
	return b, nil
}

// DeleteBucket removes a bucket and everything in it.
func (s *Store) DeleteBucket(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; !ok {
		return ErrNoBucket
	}
	delete(s.buckets, name)
	return nil
}

// Buckets returns the bucket names, sorted.
func (s *Store) Buckets() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.buckets))
	for name := range s.buckets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
