package kvstore

import (
	"errors"
	"slices"
	"testing"
)

func TestCreateBucket(t *testing.T) {
	s := New()
	b, err := s.CreateBucket("users")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "users" {
		t.Fatalf("Name = %q", b.Name())
	}
	if _, err := s.CreateBucket("users"); !errors.Is(err, ErrBucketExists) {
		t.Fatalf("second CreateBucket = %v, want ErrBucketExists", err)
	}
}

func TestMissingBucket(t *testing.T) {
	s := New()
	if _, err := s.Bucket("nope"); !errors.Is(err, ErrNoBucket) {
		t.Fatalf("Bucket(nope) = %v, want ErrNoBucket", err)
	}
	if err := s.DeleteBucket("nope"); !errors.Is(err, ErrNoBucket) {
		t.Fatalf("DeleteBucket(nope) = %v, want ErrNoBucket", err)
	}
}

func TestBucketsAreSeparate(t *testing.T) {
	s := New()
	a, _ := s.CreateBucket("a")
	b, _ := s.CreateBucket("b")
	a.Put("k", []byte("1"))
	b.Put("k", []byte("2"))
	if v, _ := a.Get("k"); string(v) != "1" {
		t.Fatalf("a.k = %q", v)
	}
	if v, _ := b.Get("k"); string(v) != "2" {
		t.Fatalf("b.k = %q", v)
	}
}

func TestDeleteBucket(t *testing.T) {
	s := New()
	s.CreateBucket("tmp")
	if err := s.DeleteBucket("tmp"); err != nil {
		t.Fatal(err)
	}
	if got := s.Buckets(); len(got) != 0 {
		t.Fatalf("Buckets = %v after delete", got)
	}
}

func TestBucketsSorted(t *testing.T) {
	s := New()
	for _, n := range []string{"c", "a", "b"} {
		s.CreateBucket(n)
	}
	if got := s.Buckets(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("Buckets = %v", got)
	}
}

func TestBucketKeysAndDelete(t *testing.T) {
	s := New()
	b, _ := s.CreateBucket("x")
	b.Put("z", nil)
	b.Put("y", []byte("v"))
	b.Delete("z")
	if got := b.Keys(); !slices.Equal(got, []string{"y"}) {
		t.Fatalf("Keys = %v", got)
	}
}
