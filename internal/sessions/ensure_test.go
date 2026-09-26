package sessions

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// EnsureCrazeID (plan 027 §3.9, SQ16): a legacy row gets a durable id under
// the index's own lock before it is claimed.

// legacyIndex writes a file with a legacy row (no crazeId), a row that already
// has one, and an unknown key on each, as an older and a newer craze would
// have left them.
func legacyIndex(t *testing.T) string {
	t.Helper()
	path := setIndex(t)
	writeRaw(t, path,
		`{"sessionId":"old","provider":"cursor","cwd":"/ws","title":"legacy","pinned":false,`+
			`"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-02T00:00:00Z","futureField":"kept"}`+"\n"+
			`{"sessionId":"new","provider":"grok","cwd":"/ws","title":"has one","pinned":true,"crazeId":"018f-held",`+
			`"createdAt":"2024-02-01T00:00:00Z","updatedAt":"2024-02-02T00:00:00Z","futureNum":9007199254740993}`+"\n")
	return path
}

// TestEnsureCrazeIDMintsOnceAndPersists: a legacy row is given a UUIDv7, in
// the file, before the id is returned; a second call reads it back rather than
// minting another. The other row, both unknown keys and the legacy row's own
// fields and timestamps are exactly as they were.
func TestEnsureCrazeIDMintsOnceAndPersists(t *testing.T) {
	path := legacyIndex(t)
	var s Store
	id, err := s.EnsureCrazeID(Row{SessionID: "old", Provider: "cursor"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[14] != '7' {
		t.Fatalf("minted %q, want a UUIDv7", id)
	}
	again, err := s.EnsureCrazeID(Row{SessionID: "old", Provider: "cursor"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("a second call minted %q after %q", again, id)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{
		`"crazeId":"` + id + `"`,
		`"futureField":"kept"`,
		`"futureNum":9007199254740993`,
		`"crazeId":"018f-held"`,
		`"updatedAt":"2024-01-02T00:00:00Z"`,
		`"createdAt":"2024-01-01T00:00:00Z"`,
		`"title":"legacy"`,
		`"title":"has one"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the index lost %s:\n%s", want, text)
		}
	}
	rows, err := s.Recent("/ws", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SessionID != "new" || rows[1].CrazeID != id {
		t.Fatalf("rows after the mint: %+v", rows)
	}
}

// TestEnsureCrazeIDLeavesARowWithAnIDUntouched: the id is answered and the
// file is not rewritten at all — same bytes, same inode.
func TestEnsureCrazeIDLeavesARowWithAnIDUntouched(t *testing.T) {
	path := legacyIndex(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var s Store
	// The caller's row carries a different id: the file's wins, as Upsert's
	// first-one-wins rule has it.
	id, err := s.EnsureCrazeID(Row{SessionID: "new", Provider: "grok", CrazeID: "018f-stale"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if id != "018f-held" {
		t.Fatalf("EnsureCrazeID = %q, want the stored id", id)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !os.SameFile(fi, fi2) {
		t.Fatalf("a row with an id was rewritten:\n%s", after)
	}
}

// TestEnsureCrazeIDOfARowNoLongerInTheIndex: the row's own id is all there
// is; with none, there is nowhere durable to mint one.
func TestEnsureCrazeIDOfARowNoLongerInTheIndex(t *testing.T) {
	legacyIndex(t)
	var s Store
	id, err := s.EnsureCrazeID(Row{SessionID: "gone", Provider: "cursor", CrazeID: "018f-own"}, time.Second)
	if err != nil || id != "018f-own" {
		t.Fatalf("EnsureCrazeID = %q, %v; want the row's own id", id, err)
	}
	if id, err := s.EnsureCrazeID(Row{SessionID: "gone", Provider: "cursor"}, time.Second); !errors.Is(err, ErrNotInIndex) {
		t.Fatalf("EnsureCrazeID of a vanished legacy row = %q, %v; want ErrNotInIndex", id, err)
	}
	// An index that cannot be read at all is not the missing row: that is the
	// caller's warning, not its refusal.
	path := setIndex(t)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureCrazeID(Row{SessionID: "old", Provider: "cursor"}, time.Second); err == nil || errors.Is(err, ErrNotInIndex) {
		t.Fatalf("EnsureCrazeID of an unreadable index = %v; want an error that is not ErrNotInIndex", err)
	}
}

// TestEnsureCrazeIDIsBusyWithinItsBound: the index lock is Upsert's; a holder
// that keeps it past the bound is atomicfile.ErrLockBusy, answered at the
// bound, and nothing is written.
func TestEnsureCrazeIDIsBusyWithinItsBound(t *testing.T) {
	path := legacyIndex(t)
	unlock, err := atomicfile.Lock(path + indexLockSuffix)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var s Store
	start := time.Now()
	id, err := s.EnsureCrazeID(Row{SessionID: "old", Provider: "cursor"}, 100*time.Millisecond)
	if !errors.Is(err, atomicfile.ErrLockBusy) {
		t.Fatalf("EnsureCrazeID under a held lock = %q, %v; want ErrLockBusy", id, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("answered busy after %s", took)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), `"crazeId"`) != 1 {
		t.Fatalf("a busy EnsureCrazeID wrote the file:\n%s", b)
	}
}

// TestConcurrentEnsureCrazeIDAgrees: loaders of one legacy row serialise on
// the index lock, and every one of them reads the id the first minted.
func TestConcurrentEnsureCrazeIDAgrees(t *testing.T) {
	legacyIndex(t)
	var s Store
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			ids[i], errs[i] = s.EnsureCrazeID(Row{SessionID: "old", Provider: "cursor"}, 10*time.Second)
		}()
	}
	waitDone(t, &wg)
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("loaders disagree: %q", ids)
		}
	}
}
