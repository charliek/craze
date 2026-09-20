package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// watchdog is the only wall-clock bound these tests use: a generous limit on
// a wait that a correct writer satisfies at once, so a deadlock fails the
// test instead of hanging the run. Nothing here sleeps to let something
// happen.
const watchdog = 10 * time.Second

// testIncarnation is every test writer's incarnation, a UUIDv7's shape.
const testIncarnation = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"

// testBase is the fixed clock's first reading, New's: the header's ts and
// the file's stamp.
var testBase = time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)

// testClock reads testBase, then one millisecond later on every call, so
// each line's ts is predictable from the order of the calls that made it.
type testClock struct{ n atomic.Int64 }

func (c *testClock) now() time.Time {
	return testBase.Add(time.Duration(c.n.Add(1)-1) * time.Millisecond)
}

// idleTimer never fires unless a test fires it: the default, so no test
// depends on the 250 ms schedule.
type idleTimer struct{ c chan time.Time }

func (t *idleTimer) C() <-chan time.Time { return t.c }
func (t *idleTimer) Stop()               {}

func neverTimer(time.Duration) flushTimer { return &idleTimer{c: make(chan time.Time)} }

// testOptions is a journal under a fresh directory with a fixed clock, the
// flush timer idle, and Close's bound stretched to the watchdog, so a test
// that is not about the bound never sees ErrStalled from a slow machine.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Dir:          filepath.Join(t.TempDir(), "journal"),
		Incarnation:  testIncarnation,
		Cwd:          "/work/app",
		CrazeVersion: "v0.0.0-test",
		Provider:     "cursor",
		AgentBinary:  "cursor-agent",
		Mode:         "agent",
		Force:        true,
		Interactive:  true,
		Now:          (&testClock{}).now,
		newTimer:     neverTimer,
		closeWait:    watchdog,
	}
}

// newWriter builds a writer and closes it when the test ends. Gates a test
// creates after this call are opened first (cleanups run last-in first-out),
// so a failing test never leaves the writer stuck behind one.
func newWriter(t *testing.T, opts Options) *Writer {
	t.Helper()
	w, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	return w
}

// event is a record with a small valid body naming its seq.
func event(seq uint64) Record {
	return Record{Seq: seq, At: testBase, EventType: "text", Body: fmt.Sprintf(`{"type":"text","text":"e%d"}`, seq)}
}

func appendEvents(w *Writer, from, to uint64) {
	for seq := from; seq <= to; seq++ {
		w.Append(event(seq))
	}
}

// gate stalls one call until the test opens it. entered is closed when the
// call arrives, so a test waits for the stall instead of guessing at it.
type gate struct {
	entered chan struct{}
	release chan struct{}
	in, out sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) block() {
	g.in.Do(func() { close(g.entered) })
	<-g.release
}

func (g *gate) open() { g.out.Do(func() { close(g.release) }) }

func (g *gate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(watchdog):
		t.Fatal("the stalled call never arrived")
	}
}

// fileHooks is the injected file's control panel: it counts Write, Sync and
// Close calls on a real descriptor and lets a test replace a call's
// behavior. Every call wakes waitFor.
type fileHooks struct {
	mu      sync.Mutex
	writes  int
	syncs   int
	closes  int
	onWrite func(call int, p []byte, f *os.File) (int, error)
	onSync  func(call int, f *os.File) error
	changed chan struct{}
}

func newFileHooks() *fileHooks { return &fileHooks{changed: make(chan struct{})} }

func (h *fileHooks) open(name string, flag int, perm os.FileMode) (file, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &hookedFile{f: f, h: h}, nil
}

// count bumps one counter and returns the call's number and hook.
func (h *fileHooks) count(n *int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	*n++
	close(h.changed)
	h.changed = make(chan struct{})
	return *n
}

func (h *fileHooks) counts() (writes, syncs, closes int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writes, h.syncs, h.closes
}

// waitFor waits until ok holds over the counts (writes, syncs, closes).
func (h *fileHooks) waitFor(t *testing.T, what string, ok func(writes, syncs, closes int) bool) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		h.mu.Lock()
		done, changed := ok(h.writes, h.syncs, h.closes), h.changed
		h.mu.Unlock()
		if done {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

type hookedFile struct {
	f *os.File
	h *fileHooks
}

func (hf *hookedFile) Write(p []byte) (int, error) {
	call := hf.h.count(&hf.h.writes)
	hf.h.mu.Lock()
	hook := hf.h.onWrite
	hf.h.mu.Unlock()
	if hook != nil {
		return hook(call, p, hf.f)
	}
	return hf.f.Write(p)
}

func (hf *hookedFile) Sync() error {
	call := hf.h.count(&hf.h.syncs)
	hf.h.mu.Lock()
	hook := hf.h.onSync
	hf.h.mu.Unlock()
	if hook != nil {
		return hook(call, hf.f)
	}
	return hf.f.Sync()
}

func (hf *hookedFile) Close() error {
	hf.h.count(&hf.h.closes)
	return hf.f.Close()
}

// diagSink is Options.Diag: goroutine-safe, and optionally stalled by a
// gate, which proves the notice is written where a stall hurts nobody.
type diagSink struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	gate *gate
}

func (d *diagSink) Write(p []byte) (int, error) {
	if d.gate != nil {
		d.gate.block()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

func (d *diagSink) lines() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := strings.TrimSuffix(d.buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func waitExited(t *testing.T, w *Writer) {
	t.Helper()
	select {
	case <-w.exited:
	case <-time.After(watchdog):
		t.Fatal("the writer goroutine did not exit")
	}
}

func waitFlushed(t *testing.T, w *Writer, seq uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, seq); err != nil {
		t.Fatalf("WaitFlushed(%d) = %v", seq, err)
	}
}

// waitBoundary waits for the boundary to reach seq without asking for a
// flush, so a test sees a flush something else caused (a threshold, the
// timer) rather than one it caused itself.
func waitBoundary(t *testing.T, w *Writer, seq uint64) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		w.mu.Lock()
		reached, advanced := w.flushed.seq >= seq, w.advanced
		w.mu.Unlock()
		if reached {
			return
		}
		select {
		case <-advanced:
		case <-deadline:
			t.Fatalf("the boundary never reached seq %d", seq)
		}
	}
}

func closeWriter(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	waitExited(t, w)
}

// rawLines is the file's lines as written, newlines dropped.
func rawLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSuffix(string(raw), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// parsedLines is the file's lines decoded, numbers as json.Number.
func parsedLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, line := range rawLines(t, path) {
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("line %d is not JSON: %v: %q", i+1, err, line)
		}
		out = append(out, m)
	}
	return out
}

// shape is a line's type plus the fields a test pins, "seq" for an event
// and "from..to" for a gap, so a file's layout compares as one string.
func shape(lines []map[string]any) string {
	var parts []string
	for _, l := range lines {
		switch l["type"] {
		case typeEvent:
			parts = append(parts, fmt.Sprintf("%v", l["seq"]))
		case typeGap:
			parts = append(parts, fmt.Sprintf("gap{%v..%v e%v n%v}", l["fromSeq"], l["toSeq"], l["droppedEvents"], l["droppedNotes"]))
		default:
			parts = append(parts, fmt.Sprintf("%v", l["type"]))
		}
	}
	return strings.Join(parts, " ")
}

// collect reads a range into a slice of records.
func collect(read func(fn func(Record) error) error) ([]Record, error) {
	var recs []Record
	err := read(func(r Record) error {
		recs = append(recs, r)
		return nil
	})
	return recs, err
}

func seqs(recs []Record) []uint64 {
	out := make([]uint64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}

func seqRange(from, to uint64) []uint64 {
	var out []uint64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}
