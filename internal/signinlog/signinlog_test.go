package signinlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/paths"
)

// The sign-in log's own tests (plan 034 §3.3, A11, A12): the record's exact
// form, one line per event kind; validation at the write; the files' modes,
// the cap and its one rotation; several writers, in one process and in two,
// across a rotation; every refusal of an unsafe directory or file, and that a
// log refused or broken stops with one Failure; the drop count; the bounded
// close; no file left open; crash output left alone.

// setVar sets *p to v for one test.
func setVar[T any](t *testing.T, p *T, v T) {
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}

// fixedNow pins the records' time for one test.
func fixedNow(t *testing.T) {
	setVar(t, &now, func() time.Time { return time.Date(2026, 10, 3, 12, 34, 56, 789_000_000, time.UTC) })
}

// nativeDir is a fresh native directory, not yet made.
func nativeDir(t *testing.T) string { return filepath.Join(t.TempDir(), "native") }

func logsDir(native string) string    { return filepath.Join(native, paths.LogsName) }
func logPath(native, n string) string { return filepath.Join(logsDir(native), n) }

// readLog is native's signin.log, or "" when there is none.
func readLog(t *testing.T, native, name string) string {
	t.Helper()
	b, err := os.ReadFile(logPath(native, name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// mustOpen opens native's log with o, which must not be refused: its setup
// over (ready) and the log on.
func mustOpen(t *testing.T, native string, o Options) *Log {
	t.Helper()
	l, err := OpenWith(native, o)
	if err != nil {
		t.Fatal(err)
	}
	awaitReady(t, l)
	if err := l.Failure(); err != nil {
		t.Fatalf("the log was refused: %v", err)
	}
	return l
}

// awaitReady waits a few seconds for l's setup to be over, passed or not.
func awaitReady(t *testing.T, l *Log) {
	t.Helper()
	select {
	case <-l.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the log's setup never finished")
	}
}

// mustClose closes l, which must have written everything and not stopped.
func mustClose(t *testing.T, l *Log) {
	t.Helper()
	if err := l.closeWithin(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := l.Failure(); err != nil {
		t.Fatalf("the log stopped: %v", err)
	}
}

const attemptID8 = "0a1b2c3d"

// TestRecordForms is the record's form, one event of each kind as chatgptauth
// reports it: the time (UTC, milliseconds), the surface, the attempt and the
// kind, then the fields the kind sets in a fixed order. These lines are the
// format the reference docs list.
func TestRecordForms(t *testing.T) {
	fixedNow(t)
	const head = `{"time":"2026-10-03T12:34:56.789Z","surface":"tui","attempt":"0a1b2c3d",`
	covered := map[chatgptauth.EventKind]bool{}
	for _, tc := range []struct {
		ev   chatgptauth.Event
		want string
	}{
		{chatgptauth.Event{Kind: chatgptauth.EventBegin, Mode: chatgptauth.ModePasteOnly, Port: 1455, Reason: chatgptauth.ReasonPortBusy, Registration: chatgptauth.RegistrationNew, Elapsed: 3 * time.Millisecond},
			`"event":"begin","mode":"paste_only","reason":"port_busy","port":1455,"registration":"new","elapsed_ms":3}`},
		{chatgptauth.Event{Kind: chatgptauth.EventBrowserOpened, Elapsed: 40 * time.Millisecond},
			`"event":"browser_opened","elapsed_ms":40}`},
		{chatgptauth.Event{Kind: chatgptauth.EventBrowserFailed, Elapsed: 41 * time.Millisecond},
			`"event":"browser_failed","elapsed_ms":41}`},
		{chatgptauth.Event{Kind: chatgptauth.EventAddressCopied, Elapsed: 2 * time.Second},
			`"event":"address_copied","elapsed_ms":2000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventListenerRefused, Status: 400, Refusal: chatgptauth.RefusalOtherAttempt, Elapsed: 9 * time.Second},
			`"event":"listener_refused","status":400,"refusal":"other_attempt","elapsed_ms":9000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventListenerRefusals, Status: 400, Refusal: chatgptauth.RefusalOtherAttempt, Count: 3, Elapsed: 60 * time.Second},
			`"event":"listener_refusals","status":400,"refusal":"other_attempt","elapsed_ms":60000,"count":3}`},
		{chatgptauth.Event{Kind: chatgptauth.EventPasteRefused, Refusal: chatgptauth.RefusalMismatch, Part: chatgptauth.PartState, Elapsed: 30 * time.Second},
			`"event":"paste_refused","refusal":"mismatch","part":"state","elapsed_ms":30000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventRedirectReceived, Via: chatgptauth.ViaPaste, Elapsed: 45 * time.Second},
			`"event":"redirect_received","via":"paste","elapsed_ms":45000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventSignedIn, Usage: chatgptauth.UsagePlan, Registration: chatgptauth.RegistrationReused, Elapsed: 46 * time.Second},
			`"event":"signed_in","usage":"plan","registration":"reused","elapsed_ms":46000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventDeclined, Step: chatgptauth.StepAuthorize, Code: "access_denied", Elapsed: 20 * time.Second},
			`"event":"declined","step":"authorize","code":"access_denied","elapsed_ms":20000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventFailed, Step: chatgptauth.StepExchange, Status: 400, Code: "invalid_grant", Class: chatgptauth.ClassRefused, Elapsed: 21 * time.Second},
			`"event":"failed","step":"exchange","status":400,"code":"invalid_grant","class":"refused","elapsed_ms":21000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventFailed, Step: chatgptauth.StepIDToken, Class: chatgptauth.ClassIDToken, Check: chatgptauth.CheckNonce},
			`"event":"failed","step":"id_token","class":"id_token","check":"nonce"}`},
		{chatgptauth.Event{Kind: chatgptauth.EventFailed, Step: chatgptauth.StepExchange, Class: chatgptauth.ClassTimeout, Elapsed: 30 * time.Second},
			`"event":"failed","step":"exchange","class":"timeout","elapsed_ms":30000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventCancelled, Reason: chatgptauth.Reason(chatgptauth.CloseEsc), Elapsed: 12 * time.Second},
			`"event":"cancelled","reason":"esc","elapsed_ms":12000}`},
		{chatgptauth.Event{Kind: chatgptauth.EventModelsFetched, Models: 8, ClientVersion: "0.160.0", Elapsed: 800 * time.Millisecond},
			`"event":"models_fetched","models":8,"client_version":"0.160.0","elapsed_ms":800}`},
		{chatgptauth.Event{Kind: chatgptauth.EventModelsFetched, ClientVersion: "0.160.0"},
			`"event":"models_fetched","models":0,"client_version":"0.160.0"}`},
		{chatgptauth.Event{Kind: chatgptauth.EventModelsEmpty, ClientVersion: "0.160.0", Elapsed: 700 * time.Millisecond},
			`"event":"models_empty","client_version":"0.160.0","elapsed_ms":700}`},
		{chatgptauth.Event{Kind: chatgptauth.EventModelsFailed, Step: chatgptauth.StepModels, Status: 403, Code: chatgptauth.CodeUnrecognised, Class: chatgptauth.ClassRefused, ClientVersion: "0.160.0"},
			`"event":"models_failed","step":"models","status":403,"code":"unrecognised","class":"refused","client_version":"0.160.0"}`},
	} {
		covered[tc.ev.Kind] = true
		tc.ev.Attempt = attemptID8
		got := string(line(render(tc.ev, SurfaceTUI, now()), 0))
		if want := head + tc.want + "\n"; got != want {
			t.Errorf("%s:\n got %s\nwant %s", tc.ev.Kind, got, want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("%s: the record is not JSON", tc.ev.Kind)
		}
	}
	// Every kind chatgptauth exports has a line above.
	for _, k := range chatgptauth.EventKinds() {
		if !covered[k] {
			t.Errorf("the table shows no %s record", k)
		}
	}
	if got := string(line(render(chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: attemptID8}, SurfaceCLI, now()), 7)); !strings.HasSuffix(got, `"event":"begin","dropped":7}`+"\n") {
		t.Fatalf("a record after dropped ones = %s", got)
	}
}

// TestRecordValidates (plan 034 Q7, A11): a value outside its set, an attempt
// id that is not 8 lowercase hex digits, a client_version that is not dotted
// numeric, a number outside its range, a surface that is neither — each is
// written "invalid", never as it was. The controls are the valid forms beside
// each.
func TestRecordValidates(t *testing.T) {
	fixedNow(t)
	field := func(ev chatgptauth.Event, surface, key string) any {
		t.Helper()
		var rec map[string]any
		if err := json.Unmarshal(line(render(ev, surface, now()), 0), &rec); err != nil {
			t.Fatal(err)
		}
		return rec[key]
	}
	ok := chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: attemptID8}
	for _, tc := range []struct {
		name    string
		ev      chatgptauth.Event
		surface string
		key     string
		want    any
	}{
		{"a valid attempt id", ok, SurfaceTUI, "attempt", attemptID8},
		{"an attempt id in capitals", chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: "0A1B2C3D"}, SurfaceTUI, "attempt", invalid},
		{"an attempt id too long", chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: "0a1b2c3d4"}, SurfaceTUI, "attempt", invalid},
		{"no attempt id", chatgptauth.Event{Kind: chatgptauth.EventBegin}, SurfaceTUI, "attempt", invalid},
		{"an unknown kind", chatgptauth.Event{Kind: "token_shown", Attempt: attemptID8}, SurfaceTUI, "event", invalid},
		{"an unknown surface", ok, "web", "surface", invalid},
		{"a code craze knows", chatgptauth.Event{Kind: chatgptauth.EventFailed, Attempt: attemptID8, Code: "invalid_grant"}, SurfaceCLI, "code", "invalid_grant"},
		{"a code craze does not know", chatgptauth.Event{Kind: chatgptauth.EventFailed, Attempt: attemptID8, Code: "sk-not-a-code"}, SurfaceCLI, "code", invalid},
		{"a step", chatgptauth.Event{Kind: chatgptauth.EventFailed, Attempt: attemptID8, Step: chatgptauth.StepJWKS}, SurfaceCLI, "step", "jwks"},
		{"no step of chatgptauth's", chatgptauth.Event{Kind: chatgptauth.EventFailed, Attempt: attemptID8, Step: "revoke"}, SurfaceCLI, "step", invalid},
		{"a client_version", chatgptauth.Event{Kind: chatgptauth.EventModelsEmpty, Attempt: attemptID8, ClientVersion: "0.160.0"}, SurfaceCLI, "client_version", "0.160.0"},
		{"four parts", chatgptauth.Event{Kind: chatgptauth.EventModelsEmpty, Attempt: attemptID8, ClientVersion: "1.2.3.4"}, SurfaceCLI, "client_version", "1.2.3.4"},
		{"five parts", chatgptauth.Event{Kind: chatgptauth.EventModelsEmpty, Attempt: attemptID8, ClientVersion: "1.2.3.4.5"}, SurfaceCLI, "client_version", invalid},
		{"seven digits", chatgptauth.Event{Kind: chatgptauth.EventModelsEmpty, Attempt: attemptID8, ClientVersion: "1234567"}, SurfaceCLI, "client_version", invalid},
		{"a trailing dot", chatgptauth.Event{Kind: chatgptauth.EventModelsEmpty, Attempt: attemptID8, ClientVersion: "0.160."}, SurfaceCLI, "client_version", invalid},
		{"a query", chatgptauth.Event{Kind: chatgptauth.EventModelsEmpty, Attempt: attemptID8, ClientVersion: "0.160.0&x=1"}, SurfaceCLI, "client_version", invalid},
		{"a status", chatgptauth.Event{Kind: chatgptauth.EventFailed, Attempt: attemptID8, Status: 503}, SurfaceCLI, "status", 503.0},
		{"a status out of range", chatgptauth.Event{Kind: chatgptauth.EventFailed, Attempt: attemptID8, Status: 1000}, SurfaceCLI, "status", invalid},
		{"a port out of range", chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: attemptID8, Port: 65536}, SurfaceCLI, "port", invalid},
		{"a negative count", chatgptauth.Event{Kind: chatgptauth.EventListenerRefusals, Attempt: attemptID8, Count: -1}, SurfaceCLI, "count", invalid},
		{"too many models", chatgptauth.Event{Kind: chatgptauth.EventModelsFetched, Attempt: attemptID8, Models: maxModels + 1}, SurfaceCLI, "models", invalid},
		{"a negative elapsed time", chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: attemptID8, Elapsed: -time.Second}, SurfaceCLI, "elapsed_ms", invalid},
		{"an elapsed time past 30 days", chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: attemptID8, Elapsed: 31 * 24 * time.Hour}, SurfaceCLI, "elapsed_ms", invalid},
	} {
		if got := field(tc.ev, tc.surface, tc.key); got != tc.want {
			t.Errorf("%s: %s = %v; want %v", tc.name, tc.key, got, tc.want)
		}
	}
	// No caller's mistake puts free text in a line: appendStr itself refuses
	// a byte JSON would escape, or a space.
	for _, v := range []string{`a"b`, "a b", "a\nb", `a\b`, "é"} {
		if got := string(appendStr(nil, "k", v)); got != `"k":"invalid"` {
			t.Errorf("appendStr(%q) = %s", v, got)
		}
	}
}

// TestOpenMakesTheFiles (§3.3): the logs directory is made 0700 and the log
// 0600; a log left wider by something else is narrowed; a record is one line.
func TestOpenMakesTheFiles(t *testing.T) {
	native := nativeDir(t)
	l := mustOpen(t, native, Options{})
	l.Record(chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: attemptID8}, SurfaceCLI)
	mustClose(t, l)
	if fi, err := os.Lstat(logsDir(native)); err != nil || fi.Mode().Perm() != 0o700 || !fi.IsDir() {
		t.Fatalf("logs is %v, %v; want a 0700 directory", fi.Mode(), err)
	}
	for _, n := range []string{FileName, lockName} {
		if fi, err := os.Lstat(logPath(native, n)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v, %v; want 0600", n, fi.Mode(), err)
		}
	}
	if got := strings.Count(readLog(t, native, FileName), "\n"); got != 1 {
		t.Fatalf("the log holds %d lines; want 1", got)
	}
	if err := os.Chmod(logPath(native, FileName), 0o644); err != nil {
		t.Fatal(err)
	}
	l = mustOpen(t, native, Options{})
	mustClose(t, l)
	if fi, _ := os.Lstat(logPath(native, FileName)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("a 0644 log was left %v; want it narrowed to 0600", fi.Mode().Perm())
	}
	if _, err := Open(""); err == nil {
		t.Fatal("a log was opened with no native directory")
	}
}

// begin is a begin event whose port tells records apart.
func begin(id string, n int) chatgptauth.Event {
	return chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: id, Port: 1000 + n}
}

// TestRotatesOnceAtTheCap (§3.3): a record that would take signin.log past
// the cap first moves it to signin.log.1 — replacing the rotation before —
// and starts a new one; nothing is split.
func TestRotatesOnceAtTheCap(t *testing.T) {
	fixedNow(t)
	native := nativeDir(t)
	one := int64(len(line(render(begin(attemptID8, 1), SurfaceCLI, now()), 0)))
	l := mustOpen(t, native, Options{Max: 3 * one})
	for i := range 7 {
		l.Record(begin(attemptID8, i), SurfaceCLI)
	}
	mustClose(t, l)
	cur, rot := readLog(t, native, FileName), readLog(t, native, rotatedName)
	if strings.Count(cur, "\n") != 1 || strings.Count(rot, "\n") != 3 {
		t.Fatalf("after 7 records of a 3-record cap: %d in the log, %d in the rotation; want 1 and 3", strings.Count(cur, "\n"), strings.Count(rot, "\n"))
	}
	if !strings.Contains(cur, `"port":1006`) || !strings.Contains(rot, `"port":1003`) || !strings.Contains(rot, `"port":1005`) {
		t.Fatalf("the files hold the wrong records:\n%s---\n%s", rot, cur)
	}
	if fi, err := os.Lstat(logPath(native, rotatedName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the rotation is %v, %v", fi.Mode(), err)
	}
}

// The multi-writer test's shape: writers × each records, with a cap that
// makes exactly one rotation of the whole (size·2/3 holds the first part, the
// rest fits the second file).
const (
	writers   = 4
	perWriter = 30
	childEnv  = "CRAZE_SIGNINLOG_TEST_CHILD"
)

// writerID is writer w's attempt id.
func writerID(w int) string { return fmt.Sprintf("%08x", 0xa0000000+w) }

// TestWritersAcrossARotation (plan 034 A12): two Logs in this process and two
// in child processes — the test binary run again — write one log at once,
// across a rotation, and every record of each is in one of the two files,
// once. The control is the cap: the rotation file is not empty.
func TestWritersAcrossARotation(t *testing.T) {
	fixedNow(t)
	native := nativeDir(t)
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	one := int64(len(line(render(begin(writerID(0), 0), SurfaceCLI, now()), 0)))
	max := one * writers * perWriter * 2 / 3
	var children []*exec.Cmd
	for w := 2; w < writers; w++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWriterChild$")
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s:%d:%d", childEnv, native, w, max))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("child output:\n%s", out.String())
			}
		})
		children = append(children, cmd)
	}
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Go(func() { writeAll(t, native, w, max) })
	}
	wg.Wait()
	for _, c := range children {
		if err := c.Wait(); err != nil {
			t.Fatalf("a child writer failed: %v", err)
		}
	}
	rot, cur := readLog(t, native, rotatedName), readLog(t, native, FileName)
	if rot == "" {
		t.Fatal("control: nothing rotated, so the test proved nothing about a rotation")
	}
	seen := map[string]int{}
	for _, l := range strings.Split(strings.TrimSuffix(rot+cur, "\n"), "\n") {
		var rec struct {
			Attempt string `json:"attempt"`
			Port    int    `json:"port"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("a record is not JSON: %v", err)
		}
		seen[fmt.Sprintf("%s/%d", rec.Attempt, rec.Port)]++
	}
	for w := range writers {
		for i := range perWriter {
			if k := fmt.Sprintf("%s/%d", writerID(w), 1000+i); seen[k] != 1 {
				t.Fatalf("writer %d's record %d is in the files %d times; want once", w, i, seen[k])
			}
		}
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("the files hold %d records; want %d", len(seen), writers*perWriter)
	}
}

// writeAll is writer w's records, through a Log of its own.
func writeAll(t *testing.T, native string, w int, max int64) {
	l, err := OpenWith(native, Options{Max: max})
	if err != nil {
		t.Error(err)
		return
	}
	for i := range perWriter {
		l.Record(begin(writerID(w), i), SurfaceCLI)
	}
	if err := l.closeWithin(time.Minute); err != nil {
		t.Error(err)
	}
	if err := l.Failure(); err != nil {
		t.Error(err)
	}
}

// TestWriterChild is TestWritersAcrossARotation's child writer; run directly
// it does nothing.
func TestWriterChild(t *testing.T) {
	spec := os.Getenv(childEnv)
	if spec == "" {
		t.Skip("only runs as TestWritersAcrossARotation's child")
	}
	parts := strings.Split(spec, ":")
	w, _ := strconv.Atoi(parts[1])
	max, _ := strconv.ParseInt(parts[2], 10, 64)
	fixedNow(t)
	writeAll(t, parts[0], w, max)
}

// refusedOpen opens native's log, which must be refused, as its writer sets
// it up, within a few seconds, with a Failure holding want, Stopped closed;
// and a record after reaches nothing the refusal protects.
func refusedOpen(t *testing.T, native, want string) {
	t.Helper()
	l, err := Open(native)
	if err != nil {
		t.Fatalf("Open answered %v; a refusal is the writer's", err)
	}
	defer func() { _ = l.Close() }()
	select {
	case <-l.Stopped():
	case <-time.After(10 * time.Second):
		t.Fatalf("the log was not refused; want it refused (%s)", want)
	}
	if err := l.Failure(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal says %v; want it to say %q", err, want)
	}
	l.Record(begin(attemptID8, 0), SurfaceCLI) // dropped: the log is off
}

// TestRefusesWhatIsNotItsOwn (plan 034 A12): the log refuses — as it is set
// up, with a reason naming the path — a logs directory that is a symlink, is not a
// directory, is writable by group or others, or belongs to another user; and
// a signin.log (or its lock) that is a symlink, a FIFO, another name of a file
// (a hard link), or another user's. A symlink is not followed: its target is
// neither written nor created. The controls: the same directory made right
// opens.
func TestRefusesWhatIsNotItsOwn(t *testing.T) {
	setup := func(t *testing.T) string {
		native := nativeDir(t)
		if err := os.MkdirAll(native, 0o700); err != nil {
			t.Fatal(err)
		}
		return native
	}
	// setupLogs is setup with its logs directory made, 0700.
	setupLogs := func(t *testing.T) string {
		native := setup(t)
		if err := os.Mkdir(logsDir(native), 0o700); err != nil {
			t.Fatal(err)
		}
		return native
	}
	t.Run("a symlinked logs", func(t *testing.T) {
		native := setup(t)
		elsewhere := filepath.Join(native, "elsewhere")
		if err := os.Mkdir(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("elsewhere", logsDir(native)); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "is a symbolic link")
		if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
			t.Fatal("the symlink's target was written")
		}
	})
	t.Run("a symlinked logs out of the native directory", func(t *testing.T) {
		native := setup(t)
		if err := os.Symlink(t.TempDir(), logsDir(native)); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "is a symbolic link")
	})
	t.Run("logs is a file", func(t *testing.T) {
		native := setup(t)
		if err := os.WriteFile(logsDir(native), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "is not a directory")
	})
	for _, mode := range []os.FileMode{0o770, 0o702, 0o777} {
		t.Run(fmt.Sprintf("logs %04o", mode), func(t *testing.T) {
			native := setupLogs(t)
			if err := os.Chmod(logsDir(native), mode); err != nil {
				t.Fatal(err)
			}
			refusedOpen(t, native, "is writable by other users")
			if err := os.Chmod(logsDir(native), 0o755); err != nil {
				t.Fatal(err)
			}
			l, err := Open(native)
			if err != nil {
				t.Fatalf("the control: a 0755 logs, writable by its owner alone, was refused: %v", err)
			}
			mustClose(t, l)
		})
	}
	t.Run("logs of another user (uid seam)", func(t *testing.T) {
		native := setup(t)
		prev := uid
		uid = func() int { return os.Getuid() + 1 }
		t.Cleanup(func() { uid = prev })
		// The directory itself is refused, before any file in it is looked at.
		refusedOpen(t, native, logsDir(native)+" belongs to another user")
		uid = prev
		l, err := Open(native)
		if err != nil {
			t.Fatalf("the control: the same directory as its own user's was refused: %v", err)
		}
		mustClose(t, l)
	})
	t.Run("a symlinked signin.log", func(t *testing.T) {
		native := setupLogs(t)
		target := filepath.Join(native, "token-like-file")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../token-like-file", logPath(native, FileName)); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "is a symbolic link")
		if b, _ := os.ReadFile(target); string(b) != "keep" {
			t.Fatal("the symlink's target was written")
		}
	})
	t.Run("a dangling symlinked signin.log", func(t *testing.T) {
		native := setupLogs(t)
		if err := os.Symlink("../made-by-the-log", logPath(native, FileName)); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, FileName)
		if _, err := os.Lstat(filepath.Join(native, "made-by-the-log")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("a dangling symlink's target was created")
		}
	})
	t.Run("a symlinked lock", func(t *testing.T) {
		native := setupLogs(t)
		if err := os.WriteFile(filepath.Join(native, "other.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../other.lock", logPath(native, lockName)); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "is a symbolic link")
	})
	t.Run("a FIFO at signin.log", func(t *testing.T) {
		native := setupLogs(t)
		if err := syscall.Mkfifo(logPath(native, FileName), 0o600); err != nil {
			t.Skipf("no FIFO here: %v", err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			refusedOpen(t, native, FileName)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a FIFO at signin.log blocked the open")
		}
	})
	t.Run("a FIFO with a reader", func(t *testing.T) {
		native := setupLogs(t)
		fifo := logPath(native, FileName)
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("no FIFO here: %v", err)
		}
		r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		refusedOpen(t, native, "is not a regular file")
	})
	t.Run("a hard-linked signin.log", func(t *testing.T) {
		native := setupLogs(t)
		other := filepath.Join(native, "other-name")
		if err := os.WriteFile(other, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(other, logPath(native, FileName)); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "has another name")
		if b, _ := os.ReadFile(other); len(b) != 0 {
			t.Fatal("the hard link's other name was written")
		}
	})
	t.Run("files of another user (as root)", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to give a file to another user; the uid seam's case covers the check")
		}
		native := setup(t)
		l := mustOpen(t, native, Options{})
		mustClose(t, l)
		if err := os.Chown(logPath(native, FileName), 65534, 65534); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "belongs to another user")
		if err := os.Chown(logPath(native, FileName), 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(logsDir(native), 65534, 65534); err != nil {
			t.Fatal(err)
		}
		refusedOpen(t, native, "belongs to another user")
	})
}

// TestABrokenLogStops (plan 034 §3.3): a log that becomes unsafe after it
// opened — a FIFO planted at signin.log — refuses the next record and stops
// for good: Failure says why, and nothing more is tried. The control is the
// record before, which is written.
func TestABrokenLogStops(t *testing.T) {
	native := nativeDir(t)
	l := mustOpen(t, native, Options{})
	l.Record(begin(attemptID8, 0), SurfaceTUI)
	waitFor(t, func() bool { return strings.Count(readLog(t, native, FileName), "\n") == 1 })
	if l.Failure() != nil {
		t.Fatal("the control: the log stopped before anything broke it")
	}
	if err := os.Remove(logPath(native, FileName)); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(logPath(native, FileName), 0o600); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	l.Record(begin(attemptID8, 1), SurfaceTUI)
	waitFor(t, func() bool { return l.Failure() != nil })
	if !strings.Contains(l.Failure().Error(), FileName) {
		t.Fatalf("the failure says %q; want it to name the file", l.Failure())
	}
	l.Record(begin(attemptID8, 2), SurfaceTUI) // dropped silently
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logPath(native, FileName)); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, native, FileName); got != "" {
		t.Fatal("a stopped log wrote again")
	}
}

// waitFor polls cond for a few seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// holdLock takes native's log lock, as another writer would, until the
// answer is called.
func holdLock(t *testing.T, native string) func() {
	t.Helper()
	unlock, err := atomicfile.Lock(logPath(native, lockName))
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)
	return release
}

// TestDropsAreCounted (§3.3): a record the queue has no room for, or that
// waited past lockWait for another writer's lock, is dropped — Record never
// blocks — and counted, and the count goes with the next record written: the
// one that waited for the lock as they were dropped, when one did. The
// controls: the records after it carry no count.
func TestDropsAreCounted(t *testing.T) {
	t.Run("the lock held past its bound", func(t *testing.T) {
		native := nativeDir(t)
		setVar(t, &lockWait, 20*time.Millisecond)
		l := mustOpen(t, native, Options{})
		release := holdLock(t, native)
		for i := range 3 {
			l.Record(begin(attemptID8, i), SurfaceCLI)
		}
		waitFor(t, func() bool { return l.dropped.Load() == 3 })
		release()
		l.Record(begin(attemptID8, 3), SurfaceCLI)
		mustClose(t, l)
		got := readLog(t, native, FileName)
		if strings.Count(got, "\n") != 1 || !strings.Contains(got, `"port":1003,"dropped":3}`) {
			t.Fatalf("after 3 records dropped at a held lock the log holds:\n%s", got)
		}
	})
	t.Run("the queue full", func(t *testing.T) {
		native := nativeDir(t)
		setVar(t, &lockWait, time.Minute)
		l := mustOpen(t, native, Options{})
		release := holdLock(t, native)
		l.Record(begin(attemptID8, 0), SurfaceCLI)
		waitFor(t, func() bool { return len(l.queue) == 0 }) // the writer has it, and waits
		for i := 1; i <= queueLen+10; i++ {
			start := time.Now()
			l.Record(begin(attemptID8, i), SurfaceCLI)
			if time.Since(start) > time.Second {
				t.Fatal("Record blocked")
			}
		}
		if l.dropped.Load() != 10 {
			t.Fatalf("%d records dropped; want the 10 past the queue's room", l.dropped.Load())
		}
		release()
		mustClose(t, l)
		lines := strings.Split(strings.TrimSuffix(readLog(t, native, FileName), "\n"), "\n")
		if len(lines) != queueLen+1 {
			t.Fatalf("the log holds %d records; want %d", len(lines), queueLen+1)
		}
		if !strings.HasSuffix(lines[0], `"port":1000,"dropped":10}`) || strings.Contains(lines[1], "dropped") {
			t.Fatalf("the drop count is not on the record that waited as they were dropped, alone:\n%s\n%s", lines[0], lines[1])
		}
	})
}

// TestCloseIsBounded (§3.3): Close waits for the queue to be written, and no
// longer than its bound: a writer stuck behind another's lock does not hold
// the program's exit. The control is the same Close with the lock free, which
// writes everything.
func TestCloseIsBounded(t *testing.T) {
	native := nativeDir(t)
	setVar(t, &lockWait, time.Minute)
	l := mustOpen(t, native, Options{})
	release := holdLock(t, native)
	l.Record(begin(attemptID8, 0), SurfaceCLI)
	start := time.Now()
	if err := l.closeWithin(100 * time.Millisecond); !errors.Is(err, errFlush) {
		t.Fatalf("Close behind a held lock = %v; want errFlush", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %s", d)
	}
	release()
	if err := l.Close(); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
	// The abandoned writer is let go by release; join it before the next
	// part (and the cleanup of lockWait), as Close never does.
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned writer never finished after the lock was released")
	}

	native = nativeDir(t)
	l = mustOpen(t, native, Options{})
	for i := range 20 {
		l.Record(begin(attemptID8, i), SurfaceCLI)
	}
	mustClose(t, l)
	if n := strings.Count(readLog(t, native, FileName), "\n"); n != 20 {
		t.Fatalf("the control: Close wrote %d of 20 records", n)
	}
	l.Record(begin(attemptID8, 99), SurfaceCLI) // after Close: dropped, no panic
}

// TestNoFileStaysOpen (§3.3: no persistent fd): between records the process
// holds no descriptor of the log, its rotation or its lock (Linux's
// /proc/self/fd says).
func TestNoFileStaysOpen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/self/fd")
	}
	native := nativeDir(t)
	l := mustOpen(t, native, Options{Max: 300})
	defer func() { _ = l.Close() }()
	for i := range 5 {
		l.Record(begin(attemptID8, i), SurfaceCLI)
	}
	waitFor(t, func() bool { return readLog(t, native, rotatedName) != "" && len(l.queue) == 0 })
	if _, err := os.ReadDir("/proc/self/fd"); err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	// The last record's closes follow its dequeue: they are waited for, as
	// long as waitFor waits, and a descriptor still open then is held.
	held := openIn(native)
	for deadline := time.Now().Add(10 * time.Second); len(held) != 0 && time.Now().Before(deadline); held = openIn(native) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(held) != 0 {
		t.Fatalf("descriptors in the native directory are open between records: %v", held)
	}
}

// openIn is each of this process's descriptors open on a path under dir, as
// "fd → path" (Linux's /proc/self/fd).
func openIn(dir string) []string {
	ents, _ := os.ReadDir("/proc/self/fd")
	var out []string
	for _, e := range ents {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && strings.HasPrefix(target, dir) {
			out = append(out, e.Name()+" → "+target)
		}
	}
	return out
}

// TestNilLog: the log a surface could not open takes every call.
func TestNilLog(t *testing.T) {
	var l *Log
	l.Record(begin(attemptID8, 0), SurfaceCLI)
	if l.Failure() != nil || l.Close() != nil {
		t.Fatal("a nil log answered something")
	}
}

const (
	crashMarker   = "signinlog-crash-marker-41c2"
	crashChildEnv = "CRAZE_SIGNINLOG_CRASH_CHILD"
)

// TestLeavesCrashOutputAlone (plan 034 A12, C2a's pattern): the sign-in log
// never touches the runtime's crash output. A child sets the crash output to
// a known file, opens a log, records across a rotation, closes it and
// panics; the panic is in the known file and in neither log file. The control
// is the rotation: the child did rotate.
func TestLeavesCrashOutputAlone(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
	cmd.Env = append(os.Environ(), crashChildEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("the child was meant to die of its panic: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), crashMarker) {
		t.Fatalf("the child did not panic with the marker:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "known.crash")); !strings.Contains(string(b), crashMarker) {
		t.Fatalf("the crash output left the file the process set: %q", b)
	}
	native := filepath.Join(dir, "native")
	for _, n := range []string{FileName, rotatedName} {
		if strings.Contains(readLog(t, native, n), crashMarker) {
			t.Fatalf("the crash output went to %s", n)
		}
	}
	if readLog(t, native, rotatedName) == "" {
		t.Fatal("control: the child never rotated, so the test proved nothing")
	}
}

// TestCrashChild is TestLeavesCrashOutputAlone's child; run directly it does
// nothing.
func TestCrashChild(t *testing.T) {
	dir := os.Getenv(crashChildEnv)
	if dir == "" {
		t.Skip("only runs as TestLeavesCrashOutputAlone's child")
	}
	known, err := os.Create(filepath.Join(dir, "known.crash"))
	if err != nil {
		t.Fatal(err)
	}
	if err := debug.SetCrashOutput(known, debug.CrashOptions{}); err != nil {
		t.Fatal(err)
	}
	l := mustOpen(t, filepath.Join(dir, "native"), Options{Max: 300})
	for i := range 6 {
		l.Record(begin(attemptID8, i), SurfaceCLI)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	panic(crashMarker)
}

// TestDepsAreItsOwn: the package imports only the standard library and the
// craze leaves the plan names — chatgptauth (the sets), atomicfile (the
// lock), paths (the name) — never the harness, the TUI or the CLI (depguard
// holds the line at lint; this is the build's own view of it).
func TestDepsAreItsOwn(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, ".").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	var craze []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if p := sc.Text(); strings.HasPrefix(p, "github.com/charliek/craze/") {
			craze = append(craze, p)
		}
	}
	want := []string{
		"github.com/charliek/craze/internal/atomicfile",
		"github.com/charliek/craze/internal/chatgptauth",
		"github.com/charliek/craze/internal/paths",
	}
	if !slices.Equal(craze, want) {
		t.Fatalf("signinlog imports %v of craze; want %v", craze, want)
	}
}

// TestOpenTouchesNoFile (plan 034 review r3 #4): Open answers at once and
// leaves every file to its writer, so a file system that hangs — Stall, the
// writer held before it touches a file — never holds up a sign-in: nothing is
// made while the writer is held, Record does not block, and Close gives up on
// it within its bound. The control is the same writer let go: it makes the
// files and writes what was queued.
func TestOpenTouchesNoFile(t *testing.T) {
	native := nativeDir(t)
	stall := make(chan struct{})
	start := time.Now()
	l, err := OpenWith(native, Options{Stall: stall})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		l.Record(begin(attemptID8, i), SurfaceTUI)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Open and three records took %s with the writer held", d)
	}
	if _, err := os.Lstat(native); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open made the native directory itself (%v); the writer is held", err)
	}
	start = time.Now()
	if err := l.closeWithin(100 * time.Millisecond); !errors.Is(err, errFlush) {
		t.Fatalf("Close with the writer held = %v; want errFlush", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close waited %s for a held writer", d)
	}
	close(stall)
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the control: the writer let go never finished")
	}
	if l.Failure() != nil || strings.Count(readLog(t, native, FileName), "\n") != 3 {
		t.Fatalf("the control: the writer let go wrote %q (%v); want the 3 queued records", readLog(t, native, FileName), l.Failure())
	}
}

// TestNoOpenWaitsOnAFIFO (plan 034 review r3 #2): a FIFO where logs goes is
// refused at once, never opened — one there before the look, and one swapped
// in between the look and the open (afterLogsCheck), which a plain open would
// wait on for a writer forever; and a directory swapped in there instead is
// refused as changed. The same holds for the native directory itself, which
// each record opens as "<dir>/.": a FIFO swapped in there after setup stops
// the log at the next record rather than holding its writer. The control is
// the swap seam left empty: the log opens and writes.
func TestNoOpenWaitsOnAFIFO(t *testing.T) {
	// within fails the test unless f answers within a few seconds; a FIFO
	// left with a writer-less open behind is opened for writing once the
	// test is done, so no open outlives it.
	within := func(t *testing.T, fifo string, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f()
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
			<-done
			t.Fatal("an open waited on a FIFO")
		}
	}
	// swapLogs makes the next look-then-open of logs find a directory and
	// open what to put there.
	swapLogs := func(t *testing.T, native string, put func(string) error) {
		t.Helper()
		var once sync.Once
		setVar(t, &afterLogsCheck, func() {
			once.Do(func() {
				if err := os.Rename(logsDir(native), logsDir(native)+".looked"); err != nil {
					t.Error(err)
				}
				if err := put(logsDir(native)); err != nil {
					t.Error(err)
				}
			})
		})
	}
	mkfifo := func(p string) error { return syscall.Mkfifo(p, 0o600) }
	t.Run("a FIFO at logs", func(t *testing.T) {
		native := nativeDir(t)
		if err := os.MkdirAll(native, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := mkfifo(logsDir(native)); err != nil {
			t.Skipf("no FIFO here: %v", err)
		}
		within(t, logsDir(native), func() { refusedOpen(t, native, "is not a directory") })
	})
	t.Run("a FIFO swapped in after the look", func(t *testing.T) {
		native := nativeDir(t)
		swapLogs(t, native, mkfifo)
		within(t, logsDir(native), func() { refusedOpen(t, native, "opening "+logsDir(native)) })
	})
	t.Run("another directory swapped in after the look", func(t *testing.T) {
		native := nativeDir(t)
		swapLogs(t, native, func(p string) error { return os.Mkdir(p, 0o700) })
		refusedOpen(t, native, "changed as it was opened")
	})
	t.Run("a FIFO swapped in at the native directory", func(t *testing.T) {
		native := nativeDir(t)
		l := mustOpen(t, native, Options{})
		defer func() { _ = l.Close() }()
		if err := os.Rename(native, native+".set-up"); err != nil {
			t.Fatal(err)
		}
		if err := mkfifo(native); err != nil {
			t.Skipf("no FIFO here: %v", err)
		}
		// A writer held in an open of the FIFO is let go when the test ends.
		t.Cleanup(func() {
			if w, err := os.OpenFile(native, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
		})
		within(t, native, func() {
			l.Record(begin(attemptID8, 0), SurfaceCLI)
			select {
			case <-l.Stopped():
			case <-time.After(10 * time.Second):
				t.Error("a record's open of the native directory did not fail")
				return
			}
			if err := l.Failure(); err == nil || !strings.Contains(err.Error(), "not a directory") {
				t.Errorf("the record's refusal = %v; want not a directory", err)
			}
		})
	})
	t.Run("control", func(t *testing.T) {
		native := nativeDir(t)
		l := mustOpen(t, native, Options{})
		l.Record(begin(attemptID8, 0), SurfaceCLI)
		mustClose(t, l)
		if strings.Count(readLog(t, native, FileName), "\n") != 1 {
			t.Fatal("the control: the log did not write")
		}
	})
}

// TestACreateRaceIsNoRefusal (plan 034 review r3 #3a): two crazes making the
// lock or the log at the same moment — the other one's create landing between
// this one's open, which found nothing, and its O_EXCL create — is not a
// refusal: the loser opens and checks the file the winner made, and the log
// stays on. The control is the file made in that window as a symlink instead,
// which is still refused.
func TestACreateRaceIsNoRefusal(t *testing.T) {
	for _, name := range []string{lockName, FileName} {
		t.Run(name, func(t *testing.T) {
			native := nativeDir(t)
			raced := 0
			setVar(t, &beforeCreate, func(n string) {
				if n == name && raced == 0 {
					raced++
					if err := os.WriteFile(logPath(native, n), nil, 0o600); err != nil {
						t.Error(err)
					}
				}
			})
			l := mustOpen(t, native, Options{})
			l.Record(begin(attemptID8, 0), SurfaceCLI)
			mustClose(t, l)
			if raced != 1 || strings.Count(readLog(t, native, FileName), "\n") != 1 {
				t.Fatalf("raced %d times; the log holds %q", raced, readLog(t, native, FileName))
			}
		})
	}
	t.Run("control: a symlink made in the window", func(t *testing.T) {
		native := nativeDir(t)
		setVar(t, &beforeCreate, func(n string) {
			if n == FileName {
				_ = os.Symlink("../elsewhere", logPath(native, n))
			}
		})
		refusedOpen(t, native, FileName)
		if _, err := os.Lstat(filepath.Join(native, "elsewhere")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the symlink's target was created")
		}
	})
}

// TestLockBusyAtSetupIsADrop (plan 034 review r3 #3b): another craze holding
// the lock past lockWait as the log is set up, and at its first record, is
// contention, not an unsafe log: the log stays on, the first record is
// dropped and counted, and the record after the lock is let go is written
// with that count. The control is the record after, which carries none.
func TestLockBusyAtSetupIsADrop(t *testing.T) {
	native := nativeDir(t)
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logsDir(native), 0o700); err != nil {
		t.Fatal(err)
	}
	setVar(t, &lockWait, 20*time.Millisecond)
	release := holdLock(t, native)
	l, err := Open(native)
	if err != nil {
		t.Fatal(err)
	}
	awaitReady(t, l)
	if err := l.Failure(); err != nil {
		t.Fatalf("a lock held at setup stopped the log: %v", err)
	}
	l.Record(begin(attemptID8, 0), SurfaceCLI)
	waitFor(t, func() bool { return l.dropped.Load() == 1 })
	if err := l.Failure(); err != nil {
		t.Fatalf("a lock held at the first record stopped the log: %v", err)
	}
	release()
	l.Record(begin(attemptID8, 1), SurfaceCLI)
	l.Record(begin(attemptID8, 2), SurfaceCLI)
	mustClose(t, l)
	lines := strings.Split(strings.TrimSuffix(readLog(t, native, FileName), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], `"port":1001,"dropped":1}`) || strings.Contains(lines[1], "dropped") {
		t.Fatalf("after the first record dropped at a held lock the log holds:\n%s", strings.Join(lines, "\n"))
	}
}
