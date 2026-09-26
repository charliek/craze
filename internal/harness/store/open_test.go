package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// These tests cover reopening a transcript (plan 028 §3.2, A1's store part
// and A2–A4): Open's lock, its repair of the tail, the resume entry, and the
// lock New takes before its first write is published.

// lockedElsewhere reports whether another descriptor holds the session lock
// on path: it tries the lock on a descriptor of its own, and gives it back.
func lockedElsewhere(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	switch err := flockNB(f.Fd()); {
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true
	case err != nil:
		t.Fatalf("flock %s: %v", path, err)
	}
	return false
}

// steps is an fsStep seam that records each step's name and runs it, or,
// with skipSyncs, skips the fsyncs (for a test that repairs a file thousands
// of times and is about what is cut, not what is durable). at, when set, runs
// before the step it names.
type steps struct {
	names     []string
	skipSyncs bool
	at        map[string]func() error
}

func (r *steps) seam(op string, do func() error) error {
	r.names = append(r.names, op)
	if f := r.at[op]; f != nil {
		if err := f(); err != nil {
			return err
		}
	}
	if r.skipSyncs && strings.HasSuffix(op, "sync") {
		return nil
	}
	return do()
}

// reopen is Open that fails the test on error and closes the store with it.
func reopen(t *testing.T, opts Options, path string) *Store {
	t.Helper()
	s, err := Open(opts, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// resumedOptions are opts as a later incarnation opens the session with: a
// newer craze, another system prompt, and a tool contract.
func resumedOptions(opts Options) Options {
	opts.CrazeVersion = "v0.0.1-resumed"
	opts.SystemPrompt = "You are craze, resumed."
	opts.ToolProfile = "opencode"
	opts.Tools = []byte(`[{"name":"read"}]`)
	return opts
}

// TestStoreReopensAndAppends (A1, the store's part): a session of two turns,
// closed and reopened, takes a third in the same file: one header, the
// header as it was, and the resume entry once, ahead of the third turn's
// entries, recording the new incarnation's contract. The turn and todos
// fields round-trip, and a reopening that writes nothing leaves nothing.
func TestStoreReopensAndAppends(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	if err := s.AppendUser(withTurn(user("q1", kimi), 1)); err != nil {
		t.Fatal(err)
	}
	list := []Todo{{ID: "t1", Content: "look", Status: "in_progress"}}
	step(t, s, nil, calls(kimi, "call_a"), withTodos(results(kimi, "call_a"), list))
	step(t, s, nil, answer("", "a1", kimi), nil)
	if err := s.AppendUser(withTurn(user("q2", kimi), 2)); err != nil {
		t.Fatal(err)
	}
	step(t, s, nil, answer("", "a2", kimi), nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	history := messageTexts(s.Context(kimi))

	// A reopening that writes nothing — a prompt held, no output — leaves the
	// file as it was: the resume entry is held like a change.
	idle := reopen(t, resumedOptions(opts), s.Path())
	if err := idle.AppendUser(withTurn(user("never answered", kimi), 3)); err != nil {
		t.Fatal(err)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(s.Path()); string(after) != string(before) {
		t.Fatalf("a reopening that wrote nothing changed the file:\n%s", after)
	}

	r := reopen(t, resumedOptions(opts), s.Path())
	if r.ID() != s.ID() || r.Path() != s.Path() || !reflect.DeepEqual(r.Header(), s.Header()) {
		t.Fatalf("reopened as id %s at %s with header %+v; want the session's own", r.ID(), r.Path(), r.Header())
	}
	if got := messageTexts(r.Context(kimi)); !reflect.DeepEqual(got, history) {
		t.Fatalf("the reopened context = %q, want %q", got, history)
	}
	if err := r.AppendUser(withTurn(user("q3", kimi), 3)); err != nil {
		t.Fatal(err)
	}
	ids := step(t, r, nil, answer("", "a3", kimi), nil)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	if got, want := fileTypes(t, s.Path()), []string{
		"session", "message", "message", "message", "message", "message", "message",
		"resume", "message", "message",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("line types = %v, want %v", got, want)
	}
	after, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("the reopened session did not only append")
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	res := tr.Entries[6]
	if want := contractOf(resumedOptions(opts)); res.Contract != want {
		t.Fatalf("the resume entry records %+v, want %+v", res.Contract, want)
	}
	if res.ParentID != tr.Entries[5].ID || ids[0] != res.ID {
		t.Fatalf("the resume entry (parent %q) is not the first of the third step (%q), after the second (%q)", res.ParentID, ids, tr.Entries[5].ID)
	}
	var turns []int
	for _, e := range tr.Entries {
		if e.Type == TypeMessage && e.Turn != 0 {
			turns = append(turns, e.Turn)
		}
	}
	if !slices.Equal(turns, []int{1, 2, 3}) {
		t.Fatalf("the turns read back as %v, want [1 2 3]", turns)
	}
	if got := tr.Entries[2].Todos; got == nil || !reflect.DeepEqual(*got, list) {
		t.Fatalf("the tool entry's todos read back as %v, want %v", got, list)
	}
	want := append(history, "user: q3", "assistant: a3")
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("context = %q, want %q (the resume entry is not history)", got, want)
	}
	// What C2's restore reads: the path, through a copy of the transcript.
	path, err := tr.Branch(tr.Leaf())
	if err != nil || len(path) != len(tr.Entries) {
		t.Fatalf("Branch = %d entries, %v; want all %d", len(path), err, len(tr.Entries))
	}
}

// tornSession writes a session of two turns and then half of a third's
// append — the user entry whole and the answer cut — and returns the store
// (closed), the file's bytes up to the last complete step, and the cut bytes.
func tornSession(t *testing.T, opts Options) (s *Store, kept, cut []byte) {
	t.Helper()
	s = newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	turn(t, s, "q2", "a2", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	u := userLine(t, "0000000a", s.Transcript().Leaf(), "q3")
	a := replyLine(t, "0000000b", "0000000a", "", "a3", kimi)
	cut = []byte(u + "\n" + a[:len(a)/2])
	f, err := os.OpenFile(s.Path(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(cut); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return s, kept, cut
}

// tornCopies is the paths of the .torn-* files beside the transcript.
func tornCopies(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(strings.TrimSuffix(path, ".jsonl") + ".torn-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestOpenTrimsATornTailAndKeepsIt (A2): a transcript that ends in a cut
// step is cut back to its last complete step, and the cut bytes are first
// saved, exactly, to <stem>.torn-<stamp> (0600), which is fsynced, and so is
// its directory, before the transcript is truncated — the step seam sees the
// order and checks the copy at the moment of the truncate. Load, the
// control, reads the same file without writing anything.
func TestOpenTrimsATornTailAndKeepsIt(t *testing.T) {
	opts := testOptions(t)
	s, kept, cut := tornSession(t, opts)
	whole, _ := os.ReadFile(s.Path())
	if _, err := Load(s.Path()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s.Path()); string(got) != string(whole) || len(tornCopies(t, s.Path())) != 0 {
		t.Fatal("control: Load wrote something")
	}

	torn := strings.TrimSuffix(s.Path(), ".jsonl") + ".torn-20260918T120000Z" // opts' clock
	var atTruncate []byte
	rec := &steps{at: map[string]func() error{
		stepTruncate: func() error {
			atTruncate, _ = os.ReadFile(torn)
			if got, _ := os.ReadFile(s.Path()); string(got) != string(whole) {
				t.Error("the transcript changed before the truncate")
			}
			return nil
		},
	}}
	ropts := opts
	ropts.Now = newClock().now
	ropts.fsStep = rec.seam
	r := reopen(t, ropts, s.Path())

	if want := []string{stepTornWrite, stepTornSync, stepDirSync, stepTruncate, stepSync}; !slices.Equal(rec.names, want) {
		t.Fatalf("repair steps = %q, want %q", rec.names, want)
	}
	if string(atTruncate) != string(cut) {
		t.Fatalf("at the truncate, the copy held %q, want the cut bytes %q", atTruncate, cut)
	}
	if got, _ := os.ReadFile(s.Path()); string(got) != string(kept) {
		t.Fatalf("the transcript after Open is\n%s\nwant\n%s", got, kept)
	}
	if copies := tornCopies(t, s.Path()); !slices.Equal(copies, []string{torn}) {
		t.Fatalf("copies beside the transcript = %q, want %q", copies, torn)
	}
	if got, _ := os.ReadFile(torn); string(got) != string(cut) {
		t.Fatalf("the copy holds %q, want %q", got, cut)
	}
	if fi, err := os.Stat(torn); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the copy's mode is %v (err %v), want 0600", fi.Mode(), err)
	}

	turn(t, r, "q3 again", "a3", kimi)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user: q1", "assistant: a1", "user: q2", "assistant: a2", "user: q3 again", "assistant: a3"}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("context = %q, want %q", got, want)
	}
}

// TestAFailedRepairLeavesTheTranscript: a failure at any step before the
// truncate refuses the open with the transcript untouched, no copy left
// beside it and the lock released; a failure after it (the transcript is
// cut, the copy is the only place the cut bytes are) refuses too, keeps the
// copy, and the next Open finds nothing left to cut.
func TestAFailedRepairLeavesTheTranscript(t *testing.T) {
	for _, op := range []string{stepTornWrite, stepTornSync, stepDirSync, stepTruncate, stepSync} {
		for _, after := range []bool{false, true} {
			if after && op == stepTruncate {
				continue // a failure after the truncate is the sync's case
			}
			t.Run(fmt.Sprintf("%s, failing %s it runs", op, map[bool]string{false: "before", true: "after"}[after]), func(t *testing.T) {
				opts := testOptions(t)
				s, kept, cut := tornSession(t, opts)
				whole, _ := os.ReadFile(s.Path())
				injected := errors.New("injected: I/O error")
				ropts := opts
				ropts.fsStep = func(name string, do func() error) error {
					if name != op {
						return do()
					}
					if after {
						if err := do(); err != nil {
							return err
						}
					}
					return injected
				}
				if _, err := Open(ropts, s.Path()); !errors.Is(err, injected) {
					t.Fatalf("Open = %v, want the injected failure", err)
				}
				if lockedElsewhere(t, s.Path()) {
					t.Fatal("the failed Open kept the lock")
				}
				got, _ := os.ReadFile(s.Path())
				copies := tornCopies(t, s.Path())
				if op != stepSync {
					if string(got) != string(whole) || len(copies) != 0 {
						t.Fatalf("after a failure before the truncate the transcript is %d bytes (want %d untouched), copies %q", len(got), len(whole), copies)
					}
				} else {
					if string(got) != string(kept) || len(copies) != 1 {
						t.Fatalf("after a failure past the truncate the transcript is %d bytes (want %d, cut), copies %q", len(got), len(kept), copies)
					}
					if c, _ := os.ReadFile(copies[0]); string(c) != string(cut) {
						t.Fatal("the copy does not hold the cut bytes")
					}
				}
				// Nothing is stuck: a plain Open now succeeds.
				rec := &steps{}
				ropts.fsStep = rec.seam
				reopen(t, ropts, s.Path())
				if op == stepSync && len(rec.names) != 0 {
					t.Fatalf("the Open after the cut repaired again: %q", rec.names)
				}
			})
		}
	}
}

// TestOpenNeverReplacesAnEarlierCopy: a copy already at the name Open would
// save to — an earlier Open's, the same second — is neither replaced nor
// removed: Open refuses, the transcript untouched, and a later second's Open
// saves beside it.
func TestOpenNeverReplacesAnEarlierCopy(t *testing.T) {
	opts := testOptions(t)
	s, kept, cut := tornSession(t, opts)
	whole, _ := os.ReadFile(s.Path())
	earlier := strings.TrimSuffix(s.Path(), ".jsonl") + ".torn-20260918T120000Z"
	if err := os.WriteFile(earlier, []byte("an earlier copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	ropts := opts
	ropts.Now = newClock().now // its first second is the copy's
	if _, err := Open(ropts, s.Path()); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Open = %v, want the copy's name taken (fs.ErrExist)", err)
	}
	if got, _ := os.ReadFile(earlier); string(got) != "an earlier copy" {
		t.Fatalf("the earlier copy now holds %q", got)
	}
	if got, _ := os.ReadFile(s.Path()); string(got) != string(whole) {
		t.Fatal("the refused Open changed the transcript")
	}
	reopen(t, ropts, s.Path()) // a second later
	if got, _ := os.ReadFile(s.Path()); string(got) != string(kept) {
		t.Fatal("the next Open did not cut the tail")
	}
	copies := tornCopies(t, s.Path())
	if len(copies) != 2 {
		t.Fatalf("copies %q, want the earlier one and a new one", copies)
	}
	if got, _ := os.ReadFile(copies[1]); string(got) != string(cut) {
		t.Fatalf("the new copy %s holds %q, want %q", copies[1], got, cut)
	}
}

// TestOpenAddsAMissingNewline (A2): a transcript whose last line is whole
// but has no newline — a crash between a line and its newline, or an edit —
// keeps that line, and Open ends it, cutting nothing and saving no copy, so
// the next append starts a line of its own.
func TestOpenAddsAMissingNewline(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	whole, _ := os.ReadFile(s.Path())
	if err := os.WriteFile(s.Path(), whole[:len(whole)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &steps{}
	ropts := opts
	ropts.fsStep = rec.seam
	r := reopen(t, ropts, s.Path())
	if want := []string{stepNewline, stepSync}; !slices.Equal(rec.names, want) {
		t.Fatalf("repair steps = %q, want %q", rec.names, want)
	}
	if got, _ := os.ReadFile(s.Path()); string(got) != string(whole) {
		t.Fatalf("after Open the file is\n%s\nwant\n%s", got, whole)
	}
	if copies := tornCopies(t, s.Path()); len(copies) != 0 {
		t.Fatalf("Open saved %q with nothing cut", copies)
	}
	turn(t, r, "q2", "a2", kimi)
	if got := fileTypes(t, s.Path()); len(got) != 6 {
		t.Fatalf("line types after an append = %v", got)
	}

	// The control: a file that already ends in a newline is not written.
	rec.names = nil
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopen(t, ropts, s.Path())
	if len(rec.names) != 0 {
		t.Fatalf("Open of a whole file repaired it: %q", rec.names)
	}
}

// TestOpenRefusesToTrimANewerEntryType (A2): when what Open would cut holds
// an entry of a type this craze does not know, a newer craze wrote it —
// maybe as a whole unit this one cannot tell from a cut step — so Open
// refuses with ErrNewerTranscript and leaves the file, and every byte of it,
// alone. The control: the same entry before the last complete step is kept,
// and the file opens.
func TestOpenRefusesToTrimANewerEntryType(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	newer := `{"type":"compaction","id":"00000003","parentId":"00000002","timestamp":"2026-09-18T12:00:00.000Z","summary":"so far"}`
	u2 := userLine(t, "00000004", "00000003", "q2")
	for name, raw := range map[string]string{
		"alone at the end":        lines(headerText(t), u1, a1, newer),
		"before a torn line":      lines(headerText(t), u1, a1, newer) + u2[:10],
		"before a cut user entry": lines(headerText(t), u1, a1, newer, u2),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, raw)
			rec := &steps{}
			opts := testOptions(t)
			opts.SessionID, opts.fsStep = "", rec.seam
			if _, err := Open(opts, path); !errors.Is(err, ErrNewerTranscript) || !strings.Contains(err.Error(), `"compaction"`) {
				t.Fatalf("Open = %v, want ErrNewerTranscript naming the type", err)
			}
			if got, _ := os.ReadFile(path); string(got) != raw || len(rec.names) != 0 || len(tornCopies(t, path)) != 0 {
				t.Fatalf("a refused Open touched the file (steps %q)", rec.names)
			}
			if lockedElsewhere(t, path) {
				t.Fatal("the refused Open kept the lock")
			}
		})
	}
	t.Run("control: kept before a complete step", func(t *testing.T) {
		a2 := replyLine(t, "00000005", "00000004", "", "a2", kimi)
		path := writeFile(t, lines(headerText(t), u1, a1, newer, u2, a2))
		opts := testOptions(t)
		opts.SessionID = ""
		r := reopen(t, opts, path)
		if tr := r.Transcript(); len(tr.Entries) != 5 || tr.Entries[2].Type != "compaction" {
			t.Fatalf("kept %d entries; want all 5, the newer one among them", len(tr.Entries))
		}
	})
}

// TestOpenNeverTrimsANewerLine (A2, astra r1-c0c1): what Open would cut is
// judged line by line, not by the entries that joined the tree. A whole last
// line of a type this craze does not know is a newer craze's even when it
// fails this craze's checks — a parent it never saw, an id it repeats, an
// envelope it refuses — and so never joined the entries: Open refuses with
// ErrNewerTranscript and leaves every byte, where Load, read-only, still
// skips the line as it always has. The controls: a known type's line failing
// the same check, and a torn line of the newer type, are cut and kept aside.
func TestOpenNeverTrimsANewerLine(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	whole := lines(headerText(t), u1, a1)
	for name, last := range map[string]string{
		"a parent it never saw":     `{"type":"compaction","id":"new-id","parentId":"missing-id","timestamp":"2026-09-18T12:00:00Z"}` + "\n",
		"no newline":                `{"type":"compaction","id":"new-id","parentId":"missing-id","timestamp":"2026-09-18T12:00:00Z"}`,
		"an id it repeats":          `{"type":"compaction","id":"00000002","parentId":"00000002","timestamp":"2026-09-18T12:00:00Z"}` + "\n",
		"an envelope it refuses":    `{"type":"compaction","timestamp":"2026-09-18T12:00:00Z"}` + "\n",
		"a type that is not a name": `{"type":7,"id":"new-id","parentId":"00000002","timestamp":"2026-09-18T12:00:00Z"}` + "\n",
		"no type at all":            `{"id":"new-id","parentId":"00000002","timestamp":"2026-09-18T12:00:00Z"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			raw := whole + last
			path := writeFile(t, raw)
			if tr, err := Load(path); err != nil || len(tr.Entries) != 2 {
				t.Fatalf("control: Load = %v; want the line skipped as before", err)
			}
			rec := &steps{}
			opts := testOptions(t)
			opts.SessionID, opts.fsStep = "", rec.seam
			if _, err := Open(opts, path); !errors.Is(err, ErrNewerTranscript) {
				t.Fatalf("Open = %v, want ErrNewerTranscript", err)
			}
			if got, _ := os.ReadFile(path); string(got) != raw || len(rec.names) != 0 || len(tornCopies(t, path)) != 0 {
				t.Fatalf("a refused Open touched the file (steps %q)", rec.names)
			}
			if lockedElsewhere(t, path) {
				t.Fatal("the refused Open kept the lock")
			}
		})
	}
	for name, last := range map[string]string{
		"control: a known type's line failing the check": `{"type":"model_change","id":"new-id","parentId":"missing-id","timestamp":"2026-09-18T12:00:00Z","provider":"p","model":"m","wire_model":"w"}` + "\n",
		"control: a torn line of the newer type":         `{"type":"compaction","id":"new-id","parentId":"missing-`,
		"control: zero bytes":                            "\x00\x00\x00\x00",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, whole+last)
			opts := testOptions(t)
			opts.SessionID = ""
			reopen(t, opts, path)
			if got, _ := os.ReadFile(path); string(got) != whole {
				t.Fatalf("the transcript after Open is\n%s\nwant\n%s", got, whole)
			}
			copies := tornCopies(t, path)
			if len(copies) != 1 {
				t.Fatalf("copies beside the transcript = %q, want one", copies)
			}
			if got, _ := os.ReadFile(copies[0]); string(got) != last {
				t.Fatalf("the copy holds %q, want %q", got, last)
			}
		})
	}
}

// TestOpenRollsBackEveryCrashPrefix (A3): a crash can persist any byte
// prefix of an append. For every prefix of one step's — written by a
// reopened store, so a resume entry, then a held model change, the turn's
// user entry, an assistant entry with two calls, and the tool entry
// answering them — Open keeps exactly the complete steps (the step itself
// only once its last line is whole, newline or not), the copy holds exactly
// what it cut, and a new step appends after it and the file loads paired: no
// prefix leaves a state an append makes corrupt (ErrUnpaired).
func TestOpenRollsBackEveryCrashPrefix(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	baseIDs := entryIDs(s.Transcript().Entries)

	// The step, appended by a reopened store so its bytes are the store's.
	full := reopen(t, opts, s.Path())
	if err := full.AppendModelChange(minimax); err != nil {
		t.Fatal(err)
	}
	if err := full.AppendUser(withTurn(user("q2", minimax), 2)); err != nil {
		t.Fatal(err)
	}
	step(t, full, nil, calls(minimax, "call_a", "call_b"), results(minimax, "call_a", "call_b"))
	if err := full.Close(); err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	app := whole[len(base):]
	allIDs := entryIDs(full.Transcript().Entries)
	if n := strings.Count(string(app), "\n"); n != 5 || len(allIDs) != len(baseIDs)+5 {
		t.Fatalf("the append has %d lines and %d entries; the fixture is not what this test expects", n, len(allIDs)-len(baseIDs))
	}

	dir := t.TempDir()
	for k := 0; k <= len(app); k++ {
		// A directory per prefix, so looking for its copy lists two files,
		// not every prefix's.
		sub := filepath.Join(dir, fmt.Sprint(k))
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sub, filepath.Base(s.Path()))
		if err := os.WriteFile(path, append(append([]byte(nil), base...), app[:k]...), 0o600); err != nil {
			t.Fatal(err)
		}
		ropts := opts
		ropts.SessionID = ""
		ropts.fsStep = (&steps{skipSyncs: true}).seam
		r, err := Open(ropts, path)
		if err != nil {
			t.Fatalf("prefix %d: Open: %v", k, err)
		}
		wantIDs, wantFile, wantCut := baseIDs, base, app[:k]
		if k >= len(app)-1 { // the tool line whole, its newline or not
			wantIDs, wantFile, wantCut = allIDs, whole, nil
		}
		if got := entryIDs(r.Transcript().Entries); !slices.Equal(got, wantIDs) {
			t.Fatalf("prefix %d: kept %q, want %q", k, got, wantIDs)
		}
		if got, _ := os.ReadFile(path); string(got) != string(wantFile) {
			t.Fatalf("prefix %d: the file after Open is\n%s\nwant\n%s", k, got, wantFile)
		}
		copies := tornCopies(t, path)
		switch {
		case len(wantCut) == 0 && len(copies) != 0:
			t.Fatalf("prefix %d: nothing cut, but %q saved", k, copies)
		case len(wantCut) > 0 && len(copies) != 1:
			t.Fatalf("prefix %d: copies %q, want one", k, copies)
		case len(wantCut) > 0:
			if got, _ := os.ReadFile(copies[0]); string(got) != string(wantCut) {
				t.Fatalf("prefix %d: the copy holds %q, want %q", k, got, wantCut)
			}
		}

		// A new step appends after whatever was kept.
		if err := r.AppendUser(withTurn(user("next", kimi), 3)); err != nil {
			t.Fatalf("prefix %d: AppendUser: %v", k, err)
		}
		if _, err := r.AppendStep(nil, calls(kimi, "call_c"), results(kimi, "call_c")); err != nil {
			t.Fatalf("prefix %d: AppendStep: %v", k, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		tr, err := Load(path)
		if err != nil {
			t.Fatalf("prefix %d: Load after the append: %v", k, err)
		}
		if n := len(tr.Entries); n != len(wantIDs)+4 {
			t.Fatalf("prefix %d: %d entries after the append, want %d (kept, resume, user, calls, results)", k, n, len(wantIDs)+4)
		}
		for _, m := range []Model{kimi, minimax} {
			if err := unpairedIn(tr.Context(m)); err != nil {
				t.Fatalf("prefix %d: the context for %s: %v", k, m.Alias, err)
			}
		}
	}
}

// TestASecondOpenerIsBusy (A4): a Store holds its session's lock until
// Close, so a second Open — in another process, or here on another
// descriptor — is ErrBusy naming the session. A live New session holds it
// too, taken on the temporary file before the link gives the transcript its
// name: the step seam, at the link, finds the temporary file already locked,
// and an Open racing the link's result finds the published file locked. The
// lock is released by Close, and by nothing before it.
func TestASecondOpenerIsBusy(t *testing.T) {
	opts := testOptions(t)
	var tmpLocked, pathBusy error
	s := newStore(t, func() Options {
		o := opts
		o.fsStep = func(op string, do func() error) error {
			if op != "link" {
				return do()
			}
			tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(s0path(opts)), ".*.tmp"))
			if len(tmps) != 1 {
				tmpLocked = fmt.Errorf("temporary files %q, want one", tmps)
			} else if !lockedElsewhere(t, tmps[0]) {
				tmpLocked = errors.New("the temporary file is not locked before the link")
			}
			if err := do(); err != nil {
				return err
			}
			if _, err := Open(opts, s0path(opts)); !errors.Is(err, ErrBusy) {
				pathBusy = fmt.Errorf("Open racing the link = %v, want ErrBusy", err)
			}
			return nil
		}
		return o
	}())
	turn(t, s, "q1", "a1", kimi)
	for _, err := range []error{tmpLocked, pathBusy} {
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := Open(opts, s.Path())
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "native session "+s.ID()+" is open in another process") {
		t.Fatalf("Open of a live New session = %v, want ErrBusy naming it", err)
	}
	turn(t, s, "q2", "a2", kimi) // the refused Open took nothing from the owner
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r := reopen(t, opts, s.Path())
	if _, err := Open(opts, s.Path()); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second Open = %v, want ErrBusy", err)
	}
	turn(t, r, "q3", "a3", kimi)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if lockedElsewhere(t, s.Path()) {
		t.Fatal("Close did not release the lock")
	}
	reopen(t, opts, s.Path())
}

// s0path is where a store New makes from opts files its transcript.
func s0path(opts Options) string {
	return sessionPath(opts.Home, opts.Workspace, opts.SessionID, newClock().now())
}

// TestATestDoubleIsLockedToo: a descriptor from a test's openFile double has
// no Fd, and the store locks the temporary file through one of its own, so
// the session is as locked as production's, before the link, until Close.
func TestATestDoubleIsLockedToo(t *testing.T) {
	opts := testOptions(t)
	var log writeLog
	opts.openFile = log.opener()
	var locked bool
	log.onWrite = func() {
		tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(s0path(opts)), ".*.tmp"))
		locked = len(tmps) == 1 && lockedElsewhere(t, tmps[0])
	}
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	log.onWrite = nil
	if !locked {
		t.Fatal("the temporary file was not locked when the first write reached it")
	}
	if _, err := Open(opts, s.Path()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Open of a live session = %v, want ErrBusy", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if lockedElsewhere(t, s.Path()) {
		t.Fatal("Close did not release the second descriptor's lock")
	}
}

// TestAFailedFirstWriteReleasesItsLock: the link's failure closes the
// temporary file's descriptor, and the lock with it. The seam holds a
// descriptor of its own on the temporary file so the file outlives its
// removal, and finds it unlocked once create has given up.
func TestAFailedFirstWriteReleasesItsLock(t *testing.T) {
	opts := testOptions(t)
	var held *os.File
	opts.fsStep = func(op string, do func() error) error {
		tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(s0path(opts)), ".*.tmp"))
		if len(tmps) != 1 {
			t.Fatalf("temporary files %q, want one", tmps)
		}
		f, err := os.Open(tmps[0])
		if err != nil {
			t.Fatal(err)
		}
		held = f
		return errors.New("injected: link failed")
	}
	s := newStore(t, opts)
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(answer("", "a", kimi)); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("the first write = %v, want the injected link failure", err)
	}
	if held == nil {
		t.Fatal("the link step never ran")
	}
	defer func() { _ = held.Close() }()
	if err := flockNB(held.Fd()); err != nil {
		t.Fatalf("the temporary file is still locked after create gave up: %v", err)
	}
	if names := sessionDirNames(t, s); len(names) != 0 {
		t.Fatalf("a failed first write left %q", names)
	}
}

// TestAFailedOpenReleasesTheLock (A4): every refusal after the lock is taken
// gives it back, so a failed Open never wedges the session: after each, the
// file can be locked from another descriptor.
func TestAFailedOpenReleasesTheLock(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	child := func(t *testing.T) string {
		opts := testOptions(t)
		opts.ParentSession, opts.ParentToolCall = "00000000-0000-4000-8000-0000000000ff", "t1.1.1"
		s := newStore(t, opts)
		turn(t, s, "q", "a", kimi)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return s.Path()
	}
	newer := `{"type":"x","id":"00000003","parentId":"00000002","timestamp":"2026-09-18T12:00:00.000Z"}`
	for name, tc := range map[string]struct {
		raw  string // the file; "" for a sub-agent's, which New writes
		want error
		opts func(*Options)
	}{
		"no header":        {lines("{}", u1, a1), ErrNoHeader, nil},
		"damage mid-file":  {lines(headerText(t), u1, "{not json", a1), ErrCorrupt, nil},
		"a sub-agent's":    {"", ErrNotResumable, nil},
		"another session":  {lines(headerText(t), u1, a1), ErrCorrupt, func(o *Options) { o.SessionID = "someone-else" }},
		"another cwd":      {lines(headerText(t), u1, a1), ErrCorrupt, func(o *Options) { o.Workspace = "/work/other" }},
		"a newer type cut": {lines(headerText(t), u1, a1, newer), ErrNewerTranscript, nil},
	} {
		t.Run(name, func(t *testing.T) {
			var path string
			if tc.raw == "" {
				path = child(t)
			} else {
				path = writeFile(t, tc.raw)
			}
			opts := testOptions(t)
			opts.SessionID = ""
			if tc.opts != nil {
				tc.opts(&opts)
			}
			if _, err := Open(opts, path); !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
			if lockedElsewhere(t, path) {
				t.Fatal("the failed Open kept the lock")
			}
		})
	}
	if _, err := Open(testOptions(t), filepath.Join(t.TempDir(), "missing.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open of a missing file = %v, want fs.ErrNotExist", err)
	}
}

// TestAReopenedEmptyTranscriptNeedsAUser: a file whose every entry Open cut —
// its first turn's write torn after the user entry — reopens with no
// entries, and, like a new session's first step, its next needs a user entry
// to write: a transcript never starts with an answer.
func TestAReopenedEmptyTranscriptNeedsAUser(t *testing.T) {
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	path := writeFile(t, lines(headerText(t), userLine(t, "00000001", "", "q1"))+a1[:20])
	opts := testOptions(t)
	opts.SessionID = ""
	r := reopen(t, opts, path)
	if n := len(r.Transcript().Entries); n != 0 {
		t.Fatalf("kept %d entries, want none", n)
	}
	if err := r.AppendAssistant(answer("", "a", kimi)); !errors.Is(err, ErrNoUser) {
		t.Fatalf("an answer first = %v, want ErrNoUser", err)
	}
	turn(t, r, "q1 again", "a1", kimi)
	if got := fileTypes(t, path); !reflect.DeepEqual(got, []string{"session", "resume", "message", "message"}) {
		t.Fatalf("line types = %v", got)
	}
	tr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, []string{"user: q1 again", "assistant: a1"}) {
		t.Fatalf("context = %q", got)
	}
}
