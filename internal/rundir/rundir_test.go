package rundir

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewHostIDIsTwelveLowercaseHexDigits(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		id := NewHostID()
		if !ValidHostID(id) {
			t.Fatalf("NewHostID() = %q, not a valid host id", id)
		}
		if seen[id] {
			t.Fatalf("NewHostID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestValidHostID(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"0123456789ab":   true,
		"0123456789AB":   false, // uppercase
		"0123456789a":    false, // 11
		"0123456789abc":  false, // 13
		"0123456789ag":   false,
		"../456789abcd":  false,
		"":               false,
		"0123456789ab\n": false,
	} {
		if got := ValidHostID(id); got != want {
			t.Errorf("ValidHostID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestTheNamespaceIsStablePerCrazeDir(t *testing.T) {
	t.Parallel()
	a, err := Namespace("/home/u/.craze")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Namespace("/home/u/.craze/")
	sum := sha256.Sum256([]byte("/home/u/.craze"))
	if want := hex.EncodeToString(sum[:])[:8]; a != want || again != a {
		t.Fatalf("Namespace = %q and %q (with a trailing slash), want %q for both", a, again, want)
	}
	b, _ := Namespace("/home/u/other-craze")
	if b == a || len(b) != 8 {
		t.Fatalf("two craze directories give %q and %q; want two different 8-digit namespaces", a, b)
	}
}

func TestARelativeCrazeDirResolvesToItsAbsolutePath(t *testing.T) {
	t.Parallel()
	rel, err := Namespace("rel/craze")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs("rel/craze")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := Namespace(abs); rel != want {
		t.Fatalf("Namespace(rel/craze) = %q, want Namespace(%s) = %q", rel, abs, want)
	}
}

func TestAnEmptyCrazeDirIsRefusedBeforeAnythingIsBuilt(t *testing.T) {
	t.Parallel()
	if _, err := Namespace(""); err == nil {
		t.Fatal("Namespace(\"\") succeeded")
	}
	env := testEnv(t)
	env.CrazeDir = ""
	bindErr(t, env, "no craze directory")
	if exists(t, filepath.Join(env.Home, cacheName)) {
		t.Error("the cache tree was built for a refused bind")
	}
	if names, _ := os.ReadDir(env.CrazeRuntimeDir); len(names) != 0 {
		t.Errorf("the runtime dir holds %d entries after a refused bind", len(names))
	}
}

func TestTwoCrazeDirsBindInTwoNamespaces(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	a := bind(t, env)
	env.CrazeDir = filepath.Join(env.Home, "other")
	b := bind(t, env)
	if a.Namespace() == b.Namespace() || filepath.Dir(a.Socket()) == filepath.Dir(b.Socket()) {
		t.Fatalf("two craze directories share a namespace: %s and %s", a.Socket(), b.Socket())
	}
	if !strings.HasSuffix(filepath.Dir(a.Socket()), "/"+a.Namespace()) {
		t.Fatalf("socket %s is not in <base>/%s", a.Socket(), a.Namespace())
	}
}
