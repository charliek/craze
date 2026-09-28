package lrucache

import "testing"

func TestGetMissing(t *testing.T) {
	c := New[int](2, nil)
	if _, ok := c.Get("a"); ok {
		t.Fatal("empty cache returned a value")
	}
}

func TestPutThenGet(t *testing.T) {
	c := New[int](2, nil)
	c.Put("a", 1)
	c.Put("b", 2)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %d, %v", v, ok)
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("Get(b) = %d, %v", v, ok)
	}
}

func TestPutReplaces(t *testing.T) {
	c := New[string](2, nil)
	c.Put("a", "one")
	c.Put("a", "uno")
	if v, _ := c.Get("a"); v != "uno" {
		t.Fatalf("Get(a) = %q, want uno", v)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

func TestRemove(t *testing.T) {
	c := New[int](2, nil)
	c.Put("a", 1)
	if !c.Remove("a") || c.Remove("a") {
		t.Fatal("Remove should report presence once")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d after Remove", c.Len())
	}
}
