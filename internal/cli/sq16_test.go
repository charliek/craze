package cli

import (
	"bytes"
	"errors"
	"fmt"
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

// holdingCraze is another craze that holds sessions: its own claims under its
// own host id and, when serving, a live registry entry under that id — the
// entry its control socket's Bind writes (plan 027 §3.8). serve rewrites that
// entry for the session its engine runs, as runHost.onEngine's rewrite does:
// an attach goes only to an entry that names the session claimed. Nothing
// accepts on the socket: an attach to it is the seam's (recordAttach).
func holdingCraze(t *testing.T, ws string, serving bool) (claims *sessionClaims, hostID string, serve func(crazeID string)) {
	t.Helper()
	env := rundir.ProcessEnv()
	hostID = rundir.NewHostID()
	serve = func(string) { t.Fatal("fixture: a holder with no socket serves nothing") }
	if serving {
		h, err := rundir.Bind(env, hostID, rundir.Entry{StartedAt: time.Now().UTC(), Workspace: ws})
		if err != nil {
			t.Fatalf("the holder's registry entry: %v", err)
		}
		t.Cleanup(func() { _ = h.Close() })
		serve = func(crazeID string) {
			t.Helper()
			if err := h.Update(func(e *rundir.Entry) { e.CrazeSessionID, e.Ready = crazeID, true }); err != nil {
				t.Fatalf("the holder's registry rewrite: %v", err)
			}
		}
	}
	claims = newSessionClaims(env, hostID, io.Discard)
	t.Cleanup(claims.releaseAll)
	return claims, hostID, serve
}

// attachCall is one attach runTUI handed the seam.
type attachCall struct {
	target attachTarget
	view   attachView
}

// recordAttach replaces the attach itself for the test: what a --continue
// would have attached to is recorded, and nothing takes the terminal.
func recordAttach(t *testing.T) *[]attachCall {
	t.Helper()
	calls := &[]attachCall{}
	prev := attachRun
	attachRun = func(target attachTarget, view attachView, _ io.Writer) error {
		*calls = append(*calls, attachCall{target: target, view: view})
		return nil
	}
	t.Cleanup(func() { attachRun = prev })
	return calls
}

// runContinue is `craze --continue --workspace ws argv...` through runTUI,
// its stderr captured.
func runContinue(t *testing.T, ws string, stderr io.Writer, argv ...string) error {
	t.Helper()
	cmd, f := parseTUIFlags(t, append([]string{"--continue", "--workspace", ws}, argv...)...)
	cmd.SetErr(stderr)
	return runTUI(cmd, f, hostEnv{})
}

// TestContinueOfAnOpenSessionAttaches (A19, PR 4 — PR 2's
// TestContinueOfAnOpenSessionRefuses, as planned: "PR 2 refuses; PR 4
// attaches"): a --continue of a row another craze holds and serves attaches
// to it — resolved through the host id the holder's lock names, for the craze
// id the row was claimed under — after one stderr line naming the holder's
// pid, which also names the flags of a new session the command line gave and
// the attach ignores. Nothing is built, bound or claimed for it: the holder's
// entry is the only one in the registry, its claim stands, and the index is as
// the holder left it. A legacy row attaches the same way, under the id the
// first loader gave it.
func TestContinueOfAnOpenSessionAttaches(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		argv     []string
		ignored  string
	}{
		{"a row with a craze id", "018f-open-thread", []string{"--model", "gpt-5", "--plan", "--theme", "dracula", "--no-mouse"},
			" (ignored: --model, --plan)"},
		{"a legacy row", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			indexHome(t)
			ws := t.TempDir()
			row := sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: tc.id, Title: "open", TitleKind: sessions.TitleKindAgent}
			seedRow(t, row, time.Minute)
			holder, hostID, serve := holdingCraze(t, ws, true)
			id, _, err := holder.claimRow(row)
			if err != nil || id == "" {
				t.Fatalf("the first craze's claim: %q, %v", id, err)
			}
			serve(id)
			index, err := os.ReadFile(indexPath(t))
			if err != nil {
				t.Fatal(err)
			}
			calls := recordAttach(t)
			var stderr bytes.Buffer
			if err := runContinue(t, ws, &stderr, tc.argv...); err != nil {
				t.Fatalf("the second --continue: %v", err)
			}
			want := "craze: that session is open in another craze (pid " + strconv.Itoa(os.Getpid()) + "); attaching" + tc.ignored + "\n"
			if stderr.String() != want {
				t.Fatalf("said %q, want %q", stderr.String(), want)
			}
			if len(*calls) != 1 {
				t.Fatalf("attached %d times, want once", len(*calls))
			}
			call := (*calls)[0]
			if call.target.entry.HostID != hostID || call.target.sessionID != id || call.target.entry.Workspace != ws {
				t.Fatalf("attached to %+v, want the holder %s's session %s", call.target, hostID, id)
			}
			if wantView := (attachView{theme: resolveThemeFor(t, tc.argv), noMouse: tc.ignored != ""}); call.view != wantView {
				t.Fatalf("the attach's view %+v, want %+v", call.view, wantView)
			}
			entries, err := rundir.Hosts(rundir.ProcessEnv())
			if err != nil || len(entries) != 1 || entries[0].HostID != hostID {
				t.Fatalf("the registry after the attach: %+v, %v — only the holder's entry", entries, err)
			}
			var held *rundir.HeldError
			if _, err := anotherCraze(t).claimSession(id); !errors.As(err, &held) || held.Holder.HostID != hostID {
				t.Fatalf("the holder's claim on %s: %v", id, err)
			}
			if after, err := os.ReadFile(indexPath(t)); err != nil || !bytes.Equal(after, index) {
				t.Fatalf("the attach wrote the index (%v):\n%s\nwas\n%s", err, after, index)
			}
		})
	}
}

// TestAHeldRowAttachesWhateverTheSpawnFlags (sol r66 4a): a row with a craze
// id is claimed before the spawn flags are asked of its provider, so a native
// session another craze holds is attached to under --agent-bin — which an
// attach ignores, and its note says so — where the flag would refuse a load.
// A native row no one holds is refused as ever, exit 2, and the claim this run
// took for it is given back: another craze can claim it at once, nothing was
// built, and the index is as it was.
func TestAHeldRowAttachesWhateverTheSpawnFlags(t *testing.T) {
	const binMsg = "craze: --agent-bin cannot be used with provider native, which runs inside craze"
	row := func(ws, id string) sessions.Row {
		return sessions.Row{SessionID: "native-1", Provider: "native", CWD: ws, CrazeID: id, Title: "a native thread", TitleKind: sessions.TitleKindAgent}
	}

	t.Run("held", func(t *testing.T) {
		indexHome(t)
		ws := t.TempDir()
		seedRow(t, row(ws, "018f-native-held"), time.Minute)
		holder, hostID, serve := holdingCraze(t, ws, true)
		if _, err := holder.claimSession("018f-native-held"); err != nil {
			t.Fatal(err)
		}
		serve("018f-native-held")
		calls := recordAttach(t)
		var stderr bytes.Buffer
		if err := runContinue(t, ws, &stderr, "--agent-bin", "/bin/true"); err != nil {
			t.Fatalf("a held native row under --agent-bin: %v", err)
		}
		want := "craze: that session is open in another craze (pid " + strconv.Itoa(os.Getpid()) + "); attaching (ignored: --agent-bin)\n"
		if stderr.String() != want || len(*calls) != 1 || (*calls)[0].target.entry.HostID != hostID ||
			(*calls)[0].target.sessionID != "018f-native-held" {
			t.Fatalf("said %q, attached %+v; want %q and the holder's session", stderr.String(), *calls, want)
		}
	})

	t.Run("free", func(t *testing.T) {
		indexHome(t)
		ws := t.TempDir()
		seedRow(t, row(ws, "018f-native-free"), time.Minute)
		before, err := os.ReadFile(indexPath(t))
		if err != nil {
			t.Fatal(err)
		}
		calls := recordAttach(t)
		claims := testClaims(t, io.Discard)
		cfg, built, err := runResolveLoadWith(t, ws, claims, "--continue", "--agent-bin", "/bin/true")
		assertExit(t, err, 2, binMsg)
		if len(built) != 0 || cfg.Session != nil || len(*calls) != 0 {
			t.Fatalf("a refused load built %d sessions, attached %d times", len(built), len(*calls))
		}
		if _, err := anotherCraze(t).claimSession("018f-native-free"); err != nil {
			t.Fatalf("the refused load kept its claim: %v", err)
		}
		if after, err := os.ReadFile(indexPath(t)); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("a refused load wrote the index (%v):\n%s\nwas\n%s", err, after, before)
		}
	})
}

// TestAHolderServingAnotherSessionIsNotAttachedTo (sol r66 4b): a holder
// keeps every claim for its life while the engine it serves can be replaced,
// so its host id alone does not say its socket serves the claimed session.
// An entry that serves another is refused — exit 1, the pid, and why — and
// the resume picker offers no --session that would select the other session;
// an entry not yet rewritten for its engine serves nothing yet.
func TestAHolderServingAnotherSessionIsNotAttachedTo(t *testing.T) {
	for _, tc := range []struct {
		name, serves, why string
	}{
		{"another session", "018f-another", " — it serves another session"},
		{"no session yet", "", " — it serves no control socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			indexHome(t)
			ws := t.TempDir()
			row := sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-claimed", Title: "claimed", UpdatedAt: time.Now(), TitleKind: sessions.TitleKindAgent}
			seedRow(t, row, time.Minute)
			holder, _, serve := holdingCraze(t, ws, true)
			if _, err := holder.claimSession("018f-claimed"); err != nil {
				t.Fatal(err)
			}
			if tc.serves != "" {
				serve(tc.serves)
			}
			refused := "that session is open in another craze (pid " + strconv.Itoa(os.Getpid()) + ")"
			calls := recordAttach(t)
			var stderr bytes.Buffer
			assertExit(t, runContinue(t, ws, &stderr), 1, "craze: "+refused+tc.why)
			if len(*calls) != 0 || stderr.Len() != 0 {
				t.Fatalf("attached %d times, said %q", len(*calls), stderr.String())
			}
			claims := testClaims(t, io.Discard)
			if _, _, err := claims.pickerClaim(row); err == nil || err.Error() != refused+tc.why {
				t.Fatalf("the picker's refusal %v, want %q", err, refused+tc.why)
			}
		})
	}
}

// resolveThemeFor is the theme an attach's view takes from argv: --theme's
// value, else the default (no config file under indexHome).
func resolveThemeFor(t *testing.T, argv []string) string {
	t.Helper()
	for i, a := range argv {
		if a == "--theme" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return tui.DefaultTheme
}

// TestContinueOfASessionWithNoSocketStillRefuses (§3.9): a holder with no
// live registry entry — its control socket opted out — has nothing to attach
// through, so PR 2's refusal stands, exit 1 naming its pid, and says why.
// Nothing is attached, and nothing is said before it.
func TestContinueOfASessionWithNoSocketStillRefuses(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	row := sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-quiet-host", TitleKind: sessions.TitleKindNone}
	seedRow(t, row, time.Minute)
	holder, _, _ := holdingCraze(t, ws, false)
	if _, _, err := holder.claimRow(row); err != nil {
		t.Fatal(err)
	}
	calls := recordAttach(t)
	var stderr bytes.Buffer
	err := runContinue(t, ws, &stderr)
	assertExit(t, err, 1, "craze: that session is open in another craze (pid "+strconv.Itoa(os.Getpid())+") — it serves no control socket")
	if len(*calls) != 0 || stderr.Len() != 0 {
		t.Fatalf("attached %d times, said %q", len(*calls), stderr.String())
	}
}

// TestContinueOfAnUnnamedHolderKeepsTheRefusal: a lock whose holder has not
// written its line yet (pid ?) names no host to attach through, and nothing
// says whether it serves: PR 2's refusal, as it was.
func TestContinueOfAnUnnamedHolderKeepsTheRefusal(t *testing.T) {
	home := indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-unnamed", TitleKind: sessions.TitleKindNone}, time.Minute)
	c := anotherCraze(t)
	if _, err := c.claimSession("018f-unnamed"); err != nil {
		t.Fatal(err)
	}
	c.releaseAll()
	f, err := os.OpenFile(filepath.Join(home, ".cache", "craze", "locks", "018f-unnamed.lock"), os.O_RDWR, 0)
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
	calls := recordAttach(t)
	var stderr bytes.Buffer
	err = runContinue(t, ws, &stderr)
	assertExit(t, err, 1, "craze: that session is open in another craze (pid ?)")
	if len(*calls) != 0 || stderr.Len() != 0 {
		t.Fatalf("attached %d times, said %q", len(*calls), stderr.String())
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

// TestThePickerRefusalNamesTheAttach (§3.9; X31 5: "PR 4 adds the hint"):
// the resume picker still refuses a row another craze holds and builds
// nothing, and its error row now says where the session can be reached — `—
// craze attach --session <hostId>` when the holder serves it, `— it serves no
// control socket` when it does not — word-wrapped in the box, the command
// whole on one row.
func TestThePickerRefusalNamesTheAttach(t *testing.T) {
	for _, serving := range []bool{true, false} {
		t.Run(fmt.Sprintf("serving=%v", serving), func(t *testing.T) {
			indexHome(t)
			ws := t.TempDir()
			rows := []sessions.Row{{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-busy", Title: "open elsewhere", UpdatedAt: time.Now()}}
			seedRow(t, rows[0], 0)
			holder, hostID, serve := holdingCraze(t, ws, serving)
			if _, err := holder.claimSession("018f-busy"); err != nil {
				t.Fatal(err)
			}
			if serving {
				serve("018f-busy")
			}
			claims := testClaims(t, io.Discard)
			refused := "that session is open in another craze (pid " + strconv.Itoa(os.Getpid()) + ")"
			hint := " — it serves no control socket"
			if serving {
				hint = " — craze attach --session " + hostID
			}
			if _, _, err := claims.pickerClaim(rows[0]); err == nil || err.Error() != refused+hint {
				t.Fatalf("the picker's refusal %v, want %q", err, refused+hint)
			}
			m, loaded := pickerModel(t, rows, claims.pickerClaim)
			m, cmd := m.Update(enterKey)
			m = feed(m, runCmd(t, cmd, 5*time.Second))
			view := ansi.Strip(m.View())
			if !strings.Contains(view, refused) || !strings.Contains(view, strings.TrimPrefix(hint, " — ")) {
				t.Fatalf("the picker does not show %q and %q:\n%s", refused, hint, view)
			}
			if len(*loaded) != 0 {
				t.Fatalf("a refused row was built: %+v", *loaded)
			}
		})
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
