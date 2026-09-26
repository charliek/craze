package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// SQ16 (plan 027 §3.9): one host per craze session. --continue and the resume
// picker claim the row they load before anything is built; a session another
// craze holds is refused, and no agent is spawned.

// anotherCraze is a second process's claims, as far as the session locks can
// tell: its own descriptors, its own host id.
func anotherCraze(t *testing.T) *sessionClaims {
	t.Helper()
	c := newSessionClaims(rundir.ProcessEnv(), rundir.NewHostID(), io.Discard)
	t.Cleanup(c.releaseAll)
	return c
}

// TestContinueOfAnOpenSessionRefuses: a --continue of a row another craze has
// claimed is exit 1 with the holder's pid, and build is never called — no
// agent is spawned. A legacy row is refused the same way, under the id the
// first loader gave it.
func TestContinueOfAnOpenSessionRefuses(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"a row with a craze id", "018f-open-thread"},
		{"a legacy row", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			indexHome(t)
			ws := t.TempDir()
			row := sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: tc.id, Title: "open", TitleKind: sessions.TitleKindAgent}
			seedRow(t, row, time.Minute)
			first := anotherCraze(t)
			id, _, err := first.claimRow(row)
			if err != nil || id == "" {
				t.Fatalf("the first craze's claim: %q, %v", id, err)
			}
			cfg, built, err := runResolveLoad(t, ws, "--continue")
			code, msg := exitCode(t, err)
			want := "craze: that session is open in another craze (pid " + strconv.Itoa(os.Getpid()) + ")"
			if code != 1 || msg != want {
				t.Fatalf("exit %d %q, want 1 %q", code, msg, want)
			}
			if len(built) != 0 {
				t.Fatalf("a refused --continue built %d sessions", len(built))
			}
			if cfg.Session != nil || cfg.CrazeSessionID != "" {
				t.Fatalf("a refusal left a session behind: %+v", cfg)
			}
		})
	}
}

// TestContinueOfASessionWhoseHolderHasNotWrittenReadsPidUnknown: the instant
// after another craze's flock and before its line, the lock names nobody.
// The refusal stands, as `pid ?`.
func TestContinueOfASessionWhoseHolderHasNotWrittenReadsPidUnknown(t *testing.T) {
	home := indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-quiet", TitleKind: sessions.TitleKindNone}, time.Minute)
	// Made by a claim, so the tree is craze's own, then emptied and held on a
	// descriptor of the test's, as a holder between its flock and its write.
	c := anotherCraze(t)
	if _, err := c.claimSession("018f-quiet"); err != nil {
		t.Fatal(err)
	}
	c.releaseAll()
	lock := filepath.Join(home, ".cache", "craze", "locks", "018f-quiet.lock")
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	_, built, err := runResolveLoad(t, ws, "--continue")
	code, msg := exitCode(t, err)
	if code != 1 || msg != "craze: that session is open in another craze (pid ?)" || len(built) != 0 {
		t.Fatalf("exit %d %q, built %d", code, msg, len(built))
	}
}

// TestContinueRefusesABusyIndex: the index lock held past the bound is exit 1,
// and nothing is built.
func TestContinueRefusesABusyIndex(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, TitleKind: sessions.TitleKindNone}, time.Minute)
	unlock, err := atomicfile.Lock(indexPath(t) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	claims := testClaims(t, io.Discard)
	claims.indexWait = 100 * time.Millisecond
	_, built, err := runResolveLoadWith(t, ws, claims, "--continue")
	code, msg := exitCode(t, err)
	if code != 1 || msg != "craze: the session index is busy — try again" || len(built) != 0 {
		t.Fatalf("exit %d %q, built %d", code, msg, len(built))
	}
}

// TestAContinueWhoseClaimCannotBeAttemptedStillLoads: a lock tree craze will
// not trust is a warning, not a lockout — the session loads unclaimed.
func TestAContinueWhoseClaimCannotBeAttemptedStillLoads(t *testing.T) {
	home := indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-mine", TitleKind: sessions.TitleKindNone}, time.Minute)
	// A cache directory another user could write: validation refuses it.
	cache := filepath.Join(home, ".cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0o777); err != nil {
		t.Fatal(err)
	}
	var diag lockedBuffer
	claims := testClaims(t, &diag)
	cfg, built, err := runResolveLoadWith(t, ws, claims, "--continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if len(built) != 1 || cfg.CrazeSessionID != "018f-mine" {
		t.Fatalf("built %d, id %q", len(built), cfg.CrazeSessionID)
	}
	lines := diagLines(diag.String())
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "craze: session lock not taken: ") {
		t.Fatalf("said %q, want one `craze: session lock not taken:` line", lines)
	}
	// The engine's hook does not try, or warn, again.
	claims.ensure("018f-mine")
	if n := len(diagLines(diag.String())); n != 1 {
		t.Fatalf("the hook warned again: %q", diag.String())
	}
}

// TestTwoLoadersOfALegacyRowShareOneClaim (§3.9, astra r2 18): two crazes
// loading one legacy row at once serialise on the index lock — the second
// reads the id the first minted — so they contend for one session lock and
// exactly one of them holds it.
func TestTwoLoadersOfALegacyRowShareOneClaim(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	row := sessions.Row{SessionID: "s-legacy", Provider: "cursor", CWD: ws, TitleKind: sessions.TitleKindNone}
	seedRow(t, row, time.Minute)
	loaders := []*sessionClaims{anotherCraze(t), anotherCraze(t)}
	ids := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, c := range loaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], _, errs[i] = c.claimRow(row)
		}()
	}
	close(start)
	wg.Wait()
	winners, refused := 0, 0
	var minted string
	for i := range loaders {
		var held *rundir.HeldError
		switch {
		case errs[i] == nil:
			winners++
			minted = ids[i]
		case errors.As(errs[i], &held):
			refused++
		default:
			t.Fatalf("loader %d: %v", i, errs[i])
		}
	}
	if winners != 1 || refused != 1 {
		t.Fatalf("%d claimed and %d were refused (%v); want one of each", winners, refused, errs)
	}
	for i := range loaders {
		var held *rundir.HeldError
		if errors.As(errs[i], &held) && held.CrazeID != minted {
			t.Fatalf("the refused loader contended for %q, the winner holds %q", held.CrazeID, minted)
		}
	}
	stored, ok, err := (&sessions.Store{}).Latest(ws, "")
	if err != nil || !ok || stored.CrazeID != minted {
		t.Fatalf("the index holds %q (%v, %v), the claim %q", stored.CrazeID, ok, err, minted)
	}
}

// changingIndex is the session index with something done to it first: the
// moment between a loader's read of a row and its EnsureCrazeID.
type changingIndex struct {
	store  *sessions.Store
	before func()
}

func (x changingIndex) EnsureCrazeID(row sessions.Row, within time.Duration) (string, error) {
	x.before()
	return x.store.EnsureCrazeID(row, within)
}

// evictRow takes sessionID's row out of the index, as another session's
// Upsert evicts the oldest row under the 500-row cap — and a row EnsureCrazeID
// gave an id is no younger for it: the mint leaves UpdatedAt alone.
func evictRow(t *testing.T, sessionID string) {
	t.Helper()
	path := indexPath(t)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if !strings.Contains(line, `"sessionId":"`+sessionID+`"`) {
			kept = append(kept, line)
		}
	}
	body := strings.Join(kept, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestALegacyRowEvictedBetweenTwoLoadersRefuses (astra r30 4): two loaders
// read one legacy row, the first gives it its id and claims it, and the row
// then leaves the index before the second's EnsureCrazeID. The second has no
// id to read back and nowhere durable to mint one; loading under an id of its
// own would put two agents on one provider session under two locks. So it
// refuses — exit 1, nothing built, no warning — and the first's claim stands.
func TestALegacyRowEvictedBetweenTwoLoadersRefuses(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	row := sessions.Row{SessionID: "s-legacy", Provider: "grok", CWD: ws, TitleKind: sessions.TitleKindNone}
	seedRow(t, row, time.Minute)
	first := anotherCraze(t)
	var firstID string
	var diag lockedBuffer
	second := testClaims(t, &diag)
	second.index = changingIndex{store: &sessions.Store{KnownProvider: knownProvider}, before: func() {
		// The second loader has read the row, with no id; the first loads it
		// now, and then it is evicted.
		id, _, err := first.claimRow(row)
		if err != nil || id == "" {
			t.Fatalf("the first loader's claim: %q, %v", id, err)
		}
		firstID = id
		evictRow(t, row.SessionID)
	}}
	cfg, built, err := runResolveLoadWith(t, ws, second, "--continue")
	code, msg := exitCode(t, err)
	if code != 1 || msg != "craze: the session index changed — try again" {
		t.Fatalf("exit %d %q, want 1 %q", code, msg, "craze: the session index changed — try again")
	}
	if len(built) != 0 || cfg.Session != nil || cfg.CrazeSessionID != "" {
		t.Fatalf("a refused load built %d sessions (id %q)", len(built), cfg.CrazeSessionID)
	}
	if s := diag.String(); s != "" {
		t.Fatalf("a refusal is not a warning; said %q", s)
	}
	var held *rundir.HeldError
	if _, err := anotherCraze(t).claimSession(firstID); !errors.As(err, &held) {
		t.Fatalf("the first loader's claim on %q: %v", firstID, err)
	}
}

// TestThePickerRefusesARowThatLeftTheIndex: the picker's rows are read when it
// opens, so the eviction schedule needs no seam there — another craze loads a
// listed legacy row and it is evicted before Enter. The picker stays up with
// the same refusal as its error row, and builds nothing.
func TestThePickerRefusesARowThatLeftTheIndex(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	rows := []sessions.Row{{SessionID: "s-legacy", Provider: "grok", CWD: ws, Title: "legacy", UpdatedAt: time.Now()}}
	seedRow(t, rows[0], 0)
	claims := testClaims(t, io.Discard)
	m, loaded := pickerModel(t, rows, claims.pickerClaim)
	if id, _, err := anotherCraze(t).claimRow(rows[0]); err != nil || id == "" {
		t.Fatalf("the other craze's claim: %q, %v", id, err)
	}
	evictRow(t, "s-legacy")
	m, cmd := m.Update(enterKey)
	m = feed(m, runCmd(t, cmd, 5*time.Second))
	if view := ansi.Strip(m.View()); !strings.Contains(view, "the session index changed — try again") {
		t.Fatalf("the picker does not show the refusal:\n%s", view)
	}
	if len(*loaded) != 0 {
		t.Fatalf("a row that left the index was built: %+v", *loaded)
	}
}

// pickerModel is --resume's model with the run's picker claim, and a
// LoadSession that records what it was asked to build.
func pickerModel(t *testing.T, rows []sessions.Row, claim func(sessions.Row) (string, func(), error)) (tea.Model, *[]sessions.Row) {
	t.Helper()
	var mu sync.Mutex
	loaded := &[]sessions.Row{}
	m := tui.New(tui.Config{
		Theme:        "tokyo-night",
		Workspace:    t.TempDir(),
		Provider:     agent.CursorProvider(),
		Resume:       rows,
		ClaimSession: claim,
		LoadSession: func(p agent.Provider, row sessions.Row) agent.Session {
			mu.Lock()
			defer mu.Unlock()
			*loaded = append(*loaded, row)
			s := tui.NewStub()
			s.SetProvider(p)
			return s
		},
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return tm, loaded
}

// runCmd runs a command the model returned and gives back what it answered —
// every answer of a batch that arrives within d.
func runCmd(t *testing.T, cmd tea.Cmd, d time.Duration) []tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	ch := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		if c != nil {
			go func() { ch <- c() }()
		}
	}
	var out []tea.Msg
	timeout := time.After(d)
	for range batch {
		select {
		case m := <-ch:
			out = append(out, m)
		case <-timeout:
			return out
		}
	}
	return out
}

func feed(m tea.Model, msgs []tea.Msg) tea.Model {
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return m
}

var enterKey = tea.KeyMsg{Type: tea.KeyEnter}

// TestThePickerRefusesAnOpenSession: Enter on a row another craze holds
// builds nothing; the picker stays up with an error row naming the holder,
// and moving the cursor clears it.
func TestThePickerRefusesAnOpenSession(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	rows := []sessions.Row{
		{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-busy", Title: "open elsewhere", UpdatedAt: time.Now()},
		{SessionID: "s-2", Provider: "cursor", CWD: ws, CrazeID: "018f-free", Title: "free", UpdatedAt: time.Now()},
	}
	for _, row := range rows {
		seedRow(t, row, 0)
	}
	if _, err := anotherCraze(t).claimSession("018f-busy"); err != nil {
		t.Fatal(err)
	}
	claims := testClaims(t, io.Discard)
	m, loaded := pickerModel(t, rows, claims.pickerClaim)
	m, cmd := m.Update(enterKey)
	m = feed(m, runCmd(t, cmd, 5*time.Second))
	view := ansi.Strip(m.View())
	want := "that session is open in another craze (pid " + strconv.Itoa(os.Getpid()) + ")"
	if !strings.Contains(view, want) || !strings.Contains(view, "open elsewhere") {
		t.Fatalf("the picker does not show the refusal %q:\n%s", want, view)
	}
	if len(*loaded) != 0 {
		t.Fatalf("a refused row was built: %+v", *loaded)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if view := ansi.Strip(m.View()); strings.Contains(view, "open in another craze") {
		t.Fatalf("moving the cursor kept the error row:\n%s", view)
	}
	// Our own run never took the refused session's lock.
	if _, err := anotherCraze(t).claimSession("018f-free"); err != nil {
		t.Fatalf("a refused attempt holds another row: %v", err)
	}
}

// TestThePickerNeverBlocksOnABusyIndex (astra r3 24): with the index lock
// held, Enter's Update returns without touching the index — the claim has not
// even been called — and the command, run, answers busy within its bound; the
// picker stays up with the busy row.
func TestThePickerNeverBlocksOnABusyIndex(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	rows := []sessions.Row{{SessionID: "s-1", Provider: "grok", CWD: ws, Title: "legacy", UpdatedAt: time.Now()}}
	seedRow(t, rows[0], 0)
	unlock, err := atomicfile.Lock(indexPath(t) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	claims := testClaims(t, io.Discard)
	const bound = 200 * time.Millisecond
	claims.indexWait = bound
	var calls atomic.Int32
	claim := func(row sessions.Row) (string, func(), error) {
		calls.Add(1)
		return claims.pickerClaim(row)
	}
	m, loaded := pickerModel(t, rows, claim)
	m, cmd := m.Update(enterKey)
	if n := calls.Load(); n != 0 {
		t.Fatalf("Enter's Update ran the claim %d times; it must only return a command", n)
	}
	start := time.Now()
	msgs := runCmd(t, cmd, 10*time.Second)
	took := time.Since(start)
	if calls.Load() != 1 {
		t.Fatalf("the command ran the claim %d times", calls.Load())
	}
	if took < bound || took > bound+2*time.Second {
		t.Fatalf("the claim answered after %s; its bound is %s", took, bound)
	}
	m = feed(m, msgs)
	if view := ansi.Strip(m.View()); !strings.Contains(view, "the session index is busy — try again") {
		t.Fatalf("the picker does not say the index is busy:\n%s", view)
	}
	if len(*loaded) != 0 {
		t.Fatalf("a busy claim built a session: %+v", *loaded)
	}
	// And the next Enter tries again.
	if _, cmd := m.Update(enterKey); cmd == nil {
		t.Fatal("Enter after a busy refusal started no new attempt")
	}
}
