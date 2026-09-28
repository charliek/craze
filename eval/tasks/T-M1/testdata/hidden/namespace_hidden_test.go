package kvstore_test

import (
	"errors"
	"slices"
	"testing"

	"example.com/kvstore"
)

func TestHiddenCreateNamespace(t *testing.T) {
	s := kvstore.New()
	ns, err := s.CreateNamespace("users")
	if err != nil {
		t.Fatal(err)
	}
	var _ *kvstore.Namespace = ns
	if ns.Name() != "users" {
		t.Fatalf("Name = %q", ns.Name())
	}
	if _, err := s.CreateNamespace("users"); !errors.Is(err, kvstore.ErrNamespaceExists) {
		t.Fatalf("second CreateNamespace = %v, want ErrNamespaceExists", err)
	}
}

func TestHiddenNamespaceLookupAndDelete(t *testing.T) {
	s := kvstore.New()
	if _, err := s.Namespace("nope"); !errors.Is(err, kvstore.ErrNoNamespace) {
		t.Fatalf("Namespace(nope) = %v, want ErrNoNamespace", err)
	}
	s.CreateNamespace("tmp")
	if _, err := s.Namespace("tmp"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNamespace("tmp"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNamespace("tmp"); !errors.Is(err, kvstore.ErrNoNamespace) {
		t.Fatalf("second DeleteNamespace = %v, want ErrNoNamespace", err)
	}
}

func TestHiddenNamespacesSortedAndSeparate(t *testing.T) {
	s := kvstore.New()
	b, _ := s.CreateNamespace("b")
	a, _ := s.CreateNamespace("a")
	a.Put("k", []byte("1"))
	b.Put("k", []byte("2"))
	if got := s.Namespaces(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Namespaces = %v", got)
	}
	if v, _ := b.Get("k"); string(v) != "2" {
		t.Fatalf("b.k = %q", v)
	}
	a.Delete("k")
	if got := a.Keys(); len(got) != 0 {
		t.Fatalf("a.Keys = %v", got)
	}
}
