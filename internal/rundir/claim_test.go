package rundir

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// claimErr is ClaimSession expected to be refused as held.
func claimErr(t *testing.T, env Env, crazeID string) *HeldError {
	t.Helper()
	c, err := ClaimSession(env, crazeID, NewHostID())
	if err == nil {
		_ = c.Release()
		t.Fatalf("ClaimSession(%s) succeeded; want it held", crazeID)
	}
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("ClaimSession(%s) = %v; want a *HeldError", crazeID, err)
	}
	return held
}

func TestASessionClaimIsExclusive(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	c, err := ClaimSession(env, "s-1", NewHostID())
	if err != nil {
		t.Fatal(err)
	}
	claimErr(t, env, "s-1")
	other, err := ClaimSession(env, "s-2", NewHostID())
	if err != nil {
		t.Fatalf("another session's claim: %v", err)
	}
	_ = other.Release()
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := ClaimSession(env, "s-1", NewHostID())
	if err != nil {
		t.Fatalf("a released session could not be claimed again: %v", err)
	}
	_ = again.Release()
}

func TestAHeldClaimNamesItsHolder(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	c, err := ClaimSession(env, "s-1", "0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Release() }()
	held := claimErr(t, env, "s-1")
	if held.Holder != (Holder{PID: os.Getpid(), HostID: "0123456789ab"}) || held.CrazeID != "s-1" {
		t.Fatalf("HeldError = %+v", held)
	}
	if msg := held.Error(); !strings.Contains(msg, "pid "+strconv.Itoa(os.Getpid())) ||
		!strings.Contains(msg, "0123456789ab") {
		t.Fatalf("the refusal %q does not name its holder", msg)
	}
}

func TestAHolderThatHasNotWrittenReadsPIDZero(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"empty":        "",
		"half-written": strconv.Itoa(os.Getpid()) + " 0123",
		"no newline":   strconv.Itoa(os.Getpid()) + " 0123456789ab",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			dir, err := env.cacheSubdir(locksName, true)
			if err != nil {
				t.Fatal(err)
			}
			// Another holder, the instant after its flock.
			f, err := os.OpenFile(filepath.Join(dir, "s-1.lock"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if taken, err := tryLock(f); err != nil || !taken {
				t.Fatalf("tryLock = %v, %v", taken, err)
			}
			if _, err := f.WriteString(content); err != nil {
				t.Fatal(err)
			}
			held := claimErr(t, env, "s-1")
			if held.Holder != (Holder{}) || !strings.Contains(held.Error(), "pid ?") {
				t.Fatalf("HeldError = %+v (%q); want the unknown holder", held.Holder, held.Error())
			}
		})
	}
}

func TestAReleasedClaimLeavesItsFile(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	c, err := ClaimSession(env, "s-1", NewHostID())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(t, c.Path()) {
		t.Fatal("releasing a claim unlinked its lock file")
	}
	if b, err := os.ReadFile(c.Path()); err != nil || len(b) != 0 {
		t.Fatalf("a released claim's file holds %q (%v); want its holder line cleared", b, err)
	}
	if c.Path() != filepath.Join(mustCanonical(t, env.Home), cacheName, crazeName, locksName, "s-1.lock") {
		t.Fatalf("the claim is at %s", c.Path())
	}
}

func TestTheSessionLockIgnoresTheRuntimeBase(t *testing.T) {
	t.Parallel()
	one := testEnv(t)
	one.XDGRuntimeDir = shortDir(t)
	two := one
	two.CrazeDir = filepath.Join(one.Home, "another-craze-home")
	two.CrazeRuntimeDir = shortDir(t)
	two.XDGRuntimeDir = ""
	two.TmpRoot = shortDir(t)
	c, err := ClaimSession(one, "s-1", NewHostID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Release() }()
	claimErr(t, two, "s-1")
	// Nor does it need any of them: only the home directory.
	claimErr(t, Env{Home: one.Home, EUID: one.EUID}, "s-1")
}

func TestABadSessionIDIsRefusedBeforeAnyPathIsBuilt(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	for _, id := range []string{"", ".", "..", "../x", "a/b", "x\x00", "é", "a b", strings.Repeat("a", 129)} {
		c, err := ClaimSession(env, id, NewHostID())
		if err == nil {
			_ = c.Release()
			t.Fatalf("ClaimSession(%q) succeeded", id)
		}
	}
	if exists(t, filepath.Join(env.Home, cacheName)) {
		t.Fatal("a refused session id built the cache tree")
	}
	for _, id := range []string{"a", "0190ab12-cd34-7ef0-8123-456789abcdef", "A.b_c-d", strings.Repeat("a", 128), "..."} {
		if !ValidToken(id) {
			t.Errorf("ValidToken(%q) = false", id)
		}
	}
}

func TestParseHolder(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Holder{
		"42 0123456789ab\n":    {PID: 42, HostID: "0123456789ab"},
		"42 0123456789ab":      {},
		"42\n":                 {},
		"0 0123456789ab\n":     {},
		"-1 0123456789ab\n":    {},
		"42 0123456789AB\n":    {},
		"42  0123456789ab\n":   {},
		"x 0123456789ab\n":     {},
		"42 0123456789ab\nx\n": {},
	} {
		if got := parseHolder(in); got != want {
			t.Errorf("parseHolder(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestASymlinkedSessionLockIsRefused(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	dir, err := env.cacheSubdir(locksName, true)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(shortDir(t), "elsewhere")
	writeFile(t, target, "keep")
	symlink(t, target, filepath.Join(dir, "s-1.lock"))
	if c, err := ClaimSession(env, "s-1", NewHostID()); err == nil {
		_ = c.Release()
		t.Fatal("ClaimSession followed a symlinked lock file")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the symlink's target was written: %q", b)
	}
}
