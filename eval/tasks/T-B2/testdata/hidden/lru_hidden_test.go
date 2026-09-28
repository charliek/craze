package lrucache_test

import (
	"slices"
	"testing"

	"example.com/lrucache"
)

func TestHiddenNewKeySurvivesEviction(t *testing.T) {
	c := lrucache.New[int](2, nil)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("Get(c) right after Put(c) = %d, %v; want 3, true", v, ok)
	}
}

func TestHiddenEvictsLeastRecentlyUsed(t *testing.T) {
	c := lrucache.New[int](2, nil)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Get("a") // a is now the most recently used
	c.Put("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b was the least recently used and should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was used after b and should still be cached")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c was just added and should be cached")
	}
}

func TestHiddenLenNeverExceedsCapacity(t *testing.T) {
	c := lrucache.New[int](3, nil)
	for i, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		c.Put(k, i)
		if c.Len() > 3 {
			t.Fatalf("after %d puts Len = %d, want at most 3", i+1, c.Len())
		}
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	if got, want := c.Keys(), []string{"g", "f", "e"}; !slices.Equal(got, want) {
		t.Fatalf("Keys = %v, want %v", got, want)
	}
}

func TestHiddenOnEvictGetsTheEvictedEntry(t *testing.T) {
	var evicted []string
	c := lrucache.New[int](1, func(key string, _ int) { evicted = append(evicted, key) })
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	if want := []string{"a", "b"}; !slices.Equal(evicted, want) {
		t.Fatalf("evicted %v, want %v", evicted, want)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("an evicted key is still returned")
	}
}

func TestHiddenReplaceDoesNotEvict(t *testing.T) {
	c := lrucache.New[int](2, nil)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("a", 10)
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	if v, _ := c.Get("b"); v != 2 {
		t.Fatalf("Get(b) = %d, want 2", v)
	}
}

func TestHiddenRemoveAfterEviction(t *testing.T) {
	c := lrucache.New[int](1, nil)
	c.Put("a", 1)
	c.Put("b", 2)
	if c.Remove("a") {
		t.Fatal("Remove(a) reported an evicted key as present")
	}
	if !c.Remove("b") {
		t.Fatal("Remove(b) did not find the cached key")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}
