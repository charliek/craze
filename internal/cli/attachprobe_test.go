package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// TestAttachProbeHiddenFromHelp is the hard stop's own claim: the flag is
// hidden, and never documented.
func TestAttachProbeHiddenFromHelp(t *testing.T) {
	isolateRunEnv(t)
	var stdout, stderr strings.Builder
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt --help: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "attach-probe") {
		t.Fatalf("--attach-probe leaked into --help:\n%s", stdout.String())
	}
}

// TestAttachProbeInertWithoutTheFlag is the Tests section's own claim: with
// the flag absent, no file and the same stdout as today. It runs the same
// tool-using, two-turn script with and without --attach-probe and holds
// stdout to be byte-identical, so the flag's presence — and the extra
// goroutine, fold and Attach it drives — never reaches the command's own
// output.
func TestAttachProbeInertWithoutTheFlag(t *testing.T) {
	without := runPromptStdout(t, "tasks", []string{"--follow-up", "two"}, "one")

	probePath := filepath.Join(t.TempDir(), "probe.txt")
	with := runPromptStdout(t, "tasks", []string{"--follow-up", "two", "--attach-probe", probePath}, "one")

	if without != with {
		t.Fatalf("--attach-probe changed stdout:\nwithout:\n%s\nwith:\n%s", without, with)
	}
	if _, err := os.Stat(probePath); err != nil {
		t.Fatalf("the probe file was not written: %v", err)
	}
}

// TestAttachProbeSameOverToolsAndFollowUp is the Tests section's happy path,
// attached mid-chain for certain (r9 finding 4): a tool-using script (two
// tool_calls per turn) with a follow-up drives the probe's whole lifecycle —
// the attached goroutine started on the first EventText, folded through the
// rest of the chain, compared at the chain's end — and the file must say
// SAME.
//
// The fake agent's gate (CRAZE_FAKE_GATE) holds each turn after its first
// tool. In tasks that is before the turn's only text, so the first turn is
// let through at once — the probe cannot attach before any text — and the
// follow-up's turn only from the probe's attach hook, once the attachment's
// subscription is registered. So the snapshot is cut before the last turn has
// ended, and the rest of that turn reaches the attached side through its
// subscription: the file's attached line must show a snapshot before the
// common seq and at least one record folded after it.
func TestAttachProbeSameOverToolsAndFollowUp(t *testing.T) {
	probePath := filepath.Join(t.TempDir(), "probe.txt")
	fifo := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_GATE", fifo)
	attached := make(chan struct{})
	var once sync.Once
	setAttachProbeAttached(t, func(context.Context) { once.Do(func() { close(attached) }) })

	released := make(chan error, 1)
	go func() {
		// The first turn: its gate comes before any text, so before any
		// attach.
		if err := releaseFakeGate(fifo); err != nil {
			released <- err
			return
		}
		// The follow-up's turn: only once the probe has attached. A probe
		// that never attaches still gets the turn released, after the
		// watchdog, so the run ends and the test says why instead of hanging.
		var late error
		select {
		case <-attached:
		case <-time.After(stubWatchdog):
			late = errors.New("the probe never attached")
		}
		if err := releaseFakeGate(fifo); err != nil {
			released <- err
			return
		}
		released <- late
	}()
	runPromptStdout(t, "tasks", []string{"--follow-up", "two", "--attach-probe", probePath}, "one")
	if err := <-released; err != nil {
		t.Fatalf("the fake agent's gate: %v", err)
	}

	body := readProbeFile(t, probePath)
	lines := strings.Split(body, "\n")
	if lines[0] != "SAME" {
		t.Fatalf("the probe file's first line = %q, want SAME:\n%s", lines[0], body)
	}
	s, k, to, r := parseAttachedLine(t, lines[1])
	n := firstSeq(t, body)
	if to != n {
		t.Fatalf("the attached fold stopped at seq %d, the common seq is %d:\n%s", to, n, body)
	}
	if s >= n {
		t.Fatalf("the snapshot at seq %d is not before the last turn's ending at seq %d: not a mid-chain attach", s, n)
	}
	if k == 0 || s+k != n {
		t.Fatalf("the attached fold folded %d records past its snapshot at %d to reach %d", k, s, n)
	}
	if r != 0 {
		t.Fatalf("re-attached %d times on a short session:\n%s", r, body)
	}
	if !strings.Contains(body, "--- attached, at seq ") {
		t.Fatalf("the probe file is missing the attached projection:\n%s", body)
	}
	retained := regexp.MustCompile(`(?m)^retained: main \d+ entries \d+ bytes; subs \d+ / \d+ entries / \d+ bytes$`)
	if !retained.MatchString(body) {
		t.Fatalf("the probe file is missing its retained-size line:\n%s", body)
	}
	t.Logf("attached at seq %d, folded %d records to the common seq %d", s, k, n)
}

// TestAttachProbeAHeldComparisonDoesNotHoldTheCommand is r9's blocker as a
// schedule: a comparison that can never complete. The attach hook holds the
// probe's goroutine once its subscription is registered, until the probe's
// own context ends, so the attached fold never folds a record. The chain's
// last ending is still handed over — the reader never waits for the probe —
// and the command finishes after its normal work plus the bounded settle: the
// cleanup cancels, the hook returns, the goroutine writes ERROR and is joined.
//
// The follow-up's turn is held until the hook is entered, so the attach is
// registered, and held, before the chain's end.
func TestAttachProbeAHeldComparisonDoesNotHoldTheCommand(t *testing.T) {
	const settle = 200 * time.Millisecond
	setAttachProbeSettle(t, settle)
	entered, returned := make(chan struct{}), make(chan struct{})
	setAttachProbeAttached(t, func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		close(returned)
	})

	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	o.attachProbePath = filepath.Join(t.TempDir(), "probe.txt")
	first := stubTurn{
		emit: []agent.Event{
			{Type: agent.EventText, Text: "hello"},
			{Type: agent.EventDone, StopReason: "end_turn"},
		},
		res: agent.Result{StopReason: "end_turn"},
	}
	second := endTurn()
	second.before = func() {
		select {
		case <-entered:
		case <-time.After(stubWatchdog):
		}
	}
	s := newStubSession(t, first, second)

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- runChain(o, context.Background(), s, "go", "follow-up") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the probe changed the run's status: %v", err)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("a held comparison held the command")
	}
	if elapsed := time.Since(start); elapsed < settle {
		t.Fatalf("the run ended after %s, before its settle of %s: the comparison in flight was not waited for", elapsed, settle)
	}
	select {
	case <-entered:
	default:
		t.Fatal("the probe never attached")
	}

	body := readProbeFile(t, o.attachProbePath)
	line := strings.SplitN(body, "\n", 2)[0]
	if !strings.HasPrefix(line, "ERROR: the probe's context ended before the attached fold reached seq ") {
		t.Fatalf("the probe file's first line = %q, want the ERROR of a fold that never reached n:\n%s", line, body)
	}
	// No leak: the held hook came back on the cancel, and the goroutine was
	// joined before the run returned.
	select {
	case <-returned:
	default:
		t.Fatal("the held hook was never released")
	}
	select {
	case <-o.probe.joined:
	default:
		t.Fatal("the probe's goroutine was not joined when the run returned")
	}
}

// TestAttachProbeAForeignTurnGiveUpWritesError is r9's finding 3: the claim
// of the first prompt is refused because the agent keeps the session for a
// turn of its own, the run gives up on it, and exits with two follow-ups still
// queued — an ending with rows pending, never the chain's clean last ending.
// With text on the stream first (the agent's own turn's) the probe had
// started, and its cleanup must write why no comparison ran and join; with no
// text ever it never attached, and says so.
func TestAttachProbeAForeignTurnGiveUpWritesError(t *testing.T) {
	for _, tc := range []struct {
		name string
		text bool
		want string
	}{
		{name: "attached", text: true, want: "ERROR: the chain ended without a clean last ending ("},
		{name: "never attached", text: false, want: "ERROR: never attached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			o := stubOpts(&stdout, &stderr)
			o.attachProbePath = filepath.Join(t.TempDir(), "probe.txt")
			s := newStubSession(t, endTurn())
			s.refuseWhileForeign = true
			s.foreign = true
			if tc.text {
				// On the run's own goroutine, in the gap before the give-up:
				// the text is on the primary before the ending it causes.
				o.beforeGiveUp = func() {
					o.beforeGiveUp = nil
					s.emit(agent.Event{Type: agent.EventText, Text: "the agent's own words"})
				}
			}

			done := make(chan error, 1)
			go func() { done <- runChain(o, context.Background(), s, "go", "f1", "f2") }()
			select {
			case err := <-done:
				var ee *exitError
				if !errors.As(err, &ee) || ee.code != 1 {
					t.Fatalf("err %v, want the give-up's exit 1", err)
				}
			case <-time.After(stubWatchdog):
				t.Fatal("the give-up never ended the run")
			}
			if !strings.Contains(stderr.String(), "kept the session") {
				t.Fatalf("stderr %q", stderr.String())
			}

			body := readProbeFile(t, o.attachProbePath)
			lines := strings.Split(body, "\n")
			if !strings.HasPrefix(lines[0], tc.want) {
				t.Fatalf("the probe file's first line = %q, want %q…:\n%s", lines[0], tc.want, body)
			}
			if !tc.text {
				if lines[1] != "attached: none, reattached 0" {
					t.Fatalf("attached line %q", lines[1])
				}
				return
			}
			if !strings.Contains(lines[0], "class foreign_turn, 2 still queued") {
				t.Fatalf("the ERROR does not say what ended the run: %q", lines[0])
			}
			select {
			case <-o.probe.joined:
			default:
				t.Fatal("the probe's goroutine was not joined when the run returned")
			}
		})
	}
}

// TestAttachProbeRefusesAPathThatIsNotARegularFile is r10's finding 1 at its
// source: PATH already a FIFO with no reader. Opening it to write waits for a
// reader for good, beyond any context's reach, and a probe that did so would
// hold the command in its cleanup's join. The probe refuses such a PATH before
// anything else — on stderr, the flag's documented error path — and runs no
// probe at all, so the command finishes in its normal time with its own
// status, and the FIFO is left as it was. Both the run that reaches a
// comparison (text, then the chain's clean last ending) and the run that never
// attaches (no text ever: cleanup's own direct write) are held to it.
func TestAttachProbeRefusesAPathThatIsNotARegularFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		turn stubTurn
	}{
		{name: "a completed comparison", turn: textTurn("hello")},
		{name: "no text", turn: endTurn()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			o := stubOpts(&stdout, &stderr)
			path := filepath.Join(t.TempDir(), "probe.fifo")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			o.attachProbePath = path

			if err := runChainWithin(t, o, newStubSession(t, tc.turn), "go"); err != nil {
				t.Fatalf("the refused probe changed the run's status: %v", err)
			}
			if o.probe != nil {
				t.Fatal("a probe was built over a FIFO")
			}
			want := "craze: --attach-probe: " + path + " is not a regular file (mode p"
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr %q, want the refusal %q…", stderr.String(), want)
			}
			if fi, err := os.Lstat(path); err != nil || fi.Mode().Type() != os.ModeNamedPipe {
				t.Fatalf("the FIFO at PATH was not left as it was: %v, %v", fi, err)
			}
		})
	}
}

// TestAttachProbeReplacesAFIFOThatAppearsLater is the other half of r10's
// finding 1: PATH passes the probe's check, and is a FIFO with no reader by the
// time the probe writes it. The probe never opens PATH — it renames a regular
// file of its own over it — so nothing waits: the command finishes in its
// normal time, nothing is said on stderr, and PATH is the probe's file. The
// comparison's write (on the goroutine) and cleanup's direct write (no text
// ever) are held to it each.
func TestAttachProbeReplacesAFIFOThatAppearsLater(t *testing.T) {
	t.Run("a completed comparison", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		o := stubOpts(&stdout, &stderr)
		path := filepath.Join(t.TempDir(), "probe.txt")
		o.attachProbePath = path
		// The FIFO appears once the probe has attached, and the chain's last
		// turn is let through only after that: so the comparison is made, and
		// written, over a FIFO.
		entered := make(chan struct{})
		made := make(chan error, 1)
		var once sync.Once
		setAttachProbeAttached(t, func(context.Context) {
			once.Do(func() {
				made <- syscall.Mkfifo(path, 0o600)
				close(entered)
			})
		})

		s := newStubSession(t, textTurn("hello"), heldUntil(entered, endTurn()))
		if err := runChainWithin(t, o, s, "go", "follow-up"); err != nil {
			t.Fatalf("the probe changed the run's status: %v", err)
		}
		mustMake(t, made)
		body := readRegularProbeFile(t, path)
		if line := strings.SplitN(body, "\n", 2)[0]; line != "SAME" {
			t.Fatalf("the probe file's first line = %q, want SAME:\n%s", line, body)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr %q, want nothing", stderr.String())
		}
	})

	t.Run("no text", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		o := stubOpts(&stdout, &stderr)
		path := filepath.Join(t.TempDir(), "probe.txt")
		o.attachProbePath = path
		// The FIFO appears inside the only turn, after the probe was built
		// and before cleanup writes "never attached" directly.
		made := make(chan error, 1)
		turn := endTurn()
		turn.before = func() { made <- syscall.Mkfifo(path, 0o600) }

		if err := runChainWithin(t, o, newStubSession(t, turn), "go"); err != nil {
			t.Fatalf("the probe changed the run's status: %v", err)
		}
		mustMake(t, made)
		body := readRegularProbeFile(t, path)
		if line := strings.SplitN(body, "\n", 2)[0]; line != "ERROR: never attached" {
			t.Fatalf("the probe file's first line = %q, want ERROR: never attached:\n%s", line, body)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr %q, want nothing", stderr.String())
		}
	})
}

// TestAttachProbeAnOmissionPastTheCommonSeqIsNeverActedOn is r10's finding 2
// as a schedule. The probe attaches at S, mid-chain; the reader is held just
// short of the chain's last ending n (on the write of the last turn's text,
// which it has already folded); the ending n is committed, and an oversized
// child event behind it, n+1, reaches every subscription as an Omitted record.
// The attached fold is handed all of it while the reader is still held, so the
// omission arrives with the ending n it is behind waiting, unfolded, for the
// reader's progress. Only then is the reader let go.
//
// The omission is past the common seq: it must wait in sequence and, once n is
// the limit, never be acted on. A fold that discards what it holds for it and
// re-attaches is cut past n and has nothing comparable to say. The verdict is
// SAME at n, with no re-attach.
func TestAttachProbeAnOmissionPastTheCommonSeqIsNeverActedOn(t *testing.T) {
	const (
		maxRecord = 16 << 10
		marker    = "the last turn's text, held on its write"
	)
	entered := make(chan struct{})
	var once sync.Once
	setAttachProbeAttached(t, func(context.Context) { once.Do(func() { close(entered) }) })
	omissions := make(chan uint64, 8)
	setAttachProbeReceived(t, func(rec agent.Record, ok bool) {
		if ok && rec.Omitted != nil {
			select {
			case omissions <- rec.Seq:
			default:
			}
		}
	})

	out := &heldWriter{marker: []byte(marker), held: make(chan struct{}), release: make(chan struct{})}
	var stderr bytes.Buffer
	o := &promptOpts{json: true, stdout: out, stderr: &stderr, foreignMax: 100 * time.Millisecond}
	path := filepath.Join(t.TempDir(), "probe.txt")
	o.attachProbePath = path

	// The last turn waits for the probe's attach, so S is before it.
	last := textTurn(marker)
	s := newStubSessionOn(t, agent.EventLogOptions{MaxRecordBytes: maxRecord}, textTurn("hello"), heldUntil(entered, last))
	// Registered after the session's cleanup, so it runs first: a failing test
	// never leaves the reader held while the session closes.
	t.Cleanup(out.let)

	// The test's own subscription, from before the first event: the barrier
	// for the chain's last ending.
	watch, err := s.log.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()

	done := make(chan error, 1)
	go func() { done <- runChain(o, context.Background(), s, "go", "follow-up") }()

	select {
	case <-out.held:
	case <-time.After(stubWatchdog):
		t.Fatal("the reader never reached the last turn's text")
	}
	n := awaitLastEnding(t, watch)
	s.emit(agent.Event{Type: agent.EventThought, Agent: "sub-1", Text: strings.Repeat("o", 2*maxRecord)})
	select {
	case seq := <-omissions:
		if seq != n+1 {
			t.Fatalf("the attached fold was handed an omission at seq %d, want %d: right behind the last ending", seq, n+1)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("the attached fold was never handed the omission")
	}
	out.let()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the probe changed the run's status: %v", err)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("the run never ended")
	}

	body := readProbeFile(t, path)
	lines := strings.Split(body, "\n")
	if lines[0] != "SAME" {
		t.Fatalf("the probe file's first line = %q, want SAME:\n%s", lines[0], body)
	}
	if got := firstSeq(t, body); got != n {
		t.Fatalf("first was handed over at seq %d, the chain's last ending is %d", got, n)
	}
	snap, k, to, r := parseAttachedLine(t, lines[1])
	if to != n || snap >= n || snap+k != n {
		t.Fatalf("attached: snapshot %d, folded %d to %d; want a snapshot before %d folded up to it:\n%s", snap, k, to, n, body)
	}
	if r != 0 {
		t.Fatalf("re-attached %d times for an omission past the common seq:\n%s", r, body)
	}
}

// TestAttachProbeASubscriptionFailurePastTheCommonSeqIsNeverActedOn is the
// same schedule for the attached fold's subscription ending with an error —
// the log dropping it as a slow consumer — after it has delivered through the
// chain's last ending n. The probe's budget is shrunk to budget records (the
// whole chain after the attach is well under it, so nothing is dropped before
// n). The reader is held just short of n, as above; the attached fold is held
// the moment it takes n; budget+2 events are published behind n, which
// overflows the budget whatever the subscription's owner had taken, so once
// the publishes return the log has dropped it (the drop is decided inside the
// publish). The fold is let go, and Records closes with ErrSlowConsumer while
// the reader is still held. Only then is the reader let go.
//
// The failure is past n: queued behind the last record delivered, it must
// never be acted on once n is the limit. A fold that discards what it holds
// for it and re-attaches is cut past n, with nothing comparable to say. The
// verdict is SAME at n, with no re-attach.
func TestAttachProbeASubscriptionFailurePastTheCommonSeqIsNeverActedOn(t *testing.T) {
	const (
		budget = 16
		marker = "the last turn's text, held on its write"
	)
	setAttachProbeMaxItems(t, budget)
	entered := make(chan struct{})
	var once sync.Once
	setAttachProbeAttached(t, func(context.Context) { once.Do(func() { close(entered) }) })
	atEnding := make(chan uint64, 1)
	closed := make(chan struct{}, 1)
	letFold := make(chan struct{})
	var letOnce sync.Once
	let := func() { letOnce.Do(func() { close(letFold) }) }
	// heldOnce is the hook's own: it runs only on the probe's goroutine.
	heldOnce := false
	setAttachProbeReceived(t, func(rec agent.Record, ok bool) {
		switch {
		case !ok:
			select {
			case closed <- struct{}{}:
			default:
			}
		case !heldOnce && isLastEnding(rec):
			// Held with n taken from Records and not yet queued: nothing
			// reads the subscription until the test lets the fold go.
			heldOnce = true
			atEnding <- rec.Seq
			<-letFold
		}
	})

	out := &heldWriter{marker: []byte(marker), held: make(chan struct{}), release: make(chan struct{})}
	var stderr bytes.Buffer
	o := &promptOpts{json: true, stdout: out, stderr: &stderr, foreignMax: 100 * time.Millisecond}
	path := filepath.Join(t.TempDir(), "probe.txt")
	o.attachProbePath = path

	s := newStubSession(t, textTurn("hello"), heldUntil(entered, textTurn(marker)))
	// Registered after the session's cleanup, so they run first: a failing
	// test never leaves the reader or the fold held while the session closes.
	t.Cleanup(out.let)
	t.Cleanup(let)

	done := make(chan error, 1)
	go func() { done <- runChain(o, context.Background(), s, "go", "follow-up") }()

	select {
	case <-out.held:
	case <-time.After(stubWatchdog):
		t.Fatal("the reader never reached the last turn's text")
	}
	var n uint64
	select {
	case n = <-atEnding:
	case <-time.After(stubWatchdog):
		t.Fatal("the attached fold was never handed the chain's last ending")
	}
	for range budget + 2 {
		s.emit(agent.Event{Type: agent.EventThought, Agent: "sub-1", Text: "x"})
	}
	let()
	select {
	case <-closed:
	case <-time.After(stubWatchdog):
		t.Fatal("the attached fold's subscription was never dropped")
	}
	out.let()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the probe changed the run's status: %v", err)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("the run never ended")
	}

	body := readProbeFile(t, path)
	lines := strings.Split(body, "\n")
	if lines[0] != "SAME" {
		t.Fatalf("the probe file's first line = %q, want SAME:\n%s", lines[0], body)
	}
	if got := firstSeq(t, body); got != n {
		t.Fatalf("first was handed over at seq %d, the chain's last ending is %d", got, n)
	}
	snap, k, to, r := parseAttachedLine(t, lines[1])
	if to != n || snap >= n || snap+k != n {
		t.Fatalf("attached: snapshot %d, folded %d to %d; want a snapshot before %d folded up to it:\n%s", snap, k, to, n, body)
	}
	if r != 0 {
		t.Fatalf("re-attached %d times for a failure past the common seq:\n%s", r, body)
	}
}

// TestAttachProbeASnapshotPastTheCommonSeqIsNotCompared is the late attach:
// an attachment cut after the seq the reader handed over is never compared —
// views from different seqs say nothing (plan 024 §3.6 item 7) — however
// alike they happen to be.
func TestAttachProbeASnapshotPastTheCommonSeqIsNotCompared(t *testing.T) {
	m := transcript.New(transcript.Options{})
	m.Fold(agent.Event{Seq: 5, Type: agent.EventText, Text: "hello"})
	a := &probeAttach{model: m, snapSeq: 5}
	p := &attachProbe{}
	got := p.verdict(a, probeTarget{n: 3, view: viewOf(m)}, false)
	if want := "DIFF: snapshot at seq 5 is past the common seq 3; not comparable"; got != want {
		t.Fatalf("verdict %q, want %q", got, want)
	}
}

// TestDiffProbeModelsDetectsADifference is the comparison logic's own unit
// test (the Tests section): two hand-folded models built the same way must
// compare SAME, and two that differ must compare DIFF, with no fake agent or
// engine involved.
func TestDiffProbeModelsDetectsADifference(t *testing.T) {
	build := func(text string) *transcript.Model {
		m := transcript.New(transcript.Options{})
		m.Fold(agent.Event{Seq: 1, Type: agent.EventText, Text: text})
		m.Fold(agent.Event{Seq: 2, Type: agent.EventDone, StopReason: "end_turn"})
		return m
	}

	if d := diffProbeModels(build("hello"), build("hello")); d != "" {
		t.Fatalf("two identical folds compared DIFF: %s", d)
	}
	d := diffProbeModels(build("hello"), build("goodbye"))
	if d == "" {
		t.Fatal("two folds with different text compared SAME, want a difference")
	}
}

// TestDiffProbeModelsCanonicalisesErrors is X14's own rule, unit-tested: a
// primary-style fold (Options.ErrText, the publisher's own error value held
// on the entry) and an attached-style fold (no ErrText, an *agent.RemoteError
// already) of the SAME error must compare SAME once the codec's own
// representation — the error's concrete type — is set aside.
func TestDiffProbeModelsCanonicalisesErrors(t *testing.T) {
	primary := transcript.New(transcript.Options{ErrText: func(e error) string { return e.Error() }})
	primary.Fold(agent.Event{Seq: 1, Type: agent.EventError, Err: errBoom{}})

	attached := transcript.New(transcript.Options{})
	attached.Fold(agent.Event{Seq: 1, Type: agent.EventError, Err: agent.RemoteErrorOf(errBoom{})})

	if d := diffProbeModels(primary, attached); d != "" {
		t.Fatalf("a primary fold and a decoded fold of the same error compared DIFF: %s", d)
	}
}

// errBoom is a plain error value with no exported fields, the shape a
// publisher's own synthetic error can take on the primary (never an
// *agent.RemoteError, X14) and which agent.RemoteErrorOf must still
// canonicalise for a comparison against a decoded one.
type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// setAttachProbeAttached installs the probe's attach hook for one test. A
// probe reads it when it is built, so it is set before the run starts and
// restored once the run is over.
func setAttachProbeAttached(t *testing.T, hook func(context.Context)) {
	t.Helper()
	prev := attachProbeAttached
	attachProbeAttached = hook
	t.Cleanup(func() { attachProbeAttached = prev })
}

// setAttachProbeReceived installs the probe's received-record hook for one
// test, the same way.
func setAttachProbeReceived(t *testing.T, hook func(agent.Record, bool)) {
	t.Helper()
	prev := attachProbeReceived
	attachProbeReceived = hook
	t.Cleanup(func() { attachProbeReceived = prev })
}

// setAttachProbeMaxItems sets the probe's subscription budget for one test.
func setAttachProbeMaxItems(t *testing.T, n int) {
	t.Helper()
	prev := attachProbeMaxItems
	attachProbeMaxItems = n
	t.Cleanup(func() { attachProbeMaxItems = prev })
}

// newStubSessionOn is newStubSession over a log built with opts: a small
// MaxRecordBytes, so an event can be oversized without being megabytes.
func newStubSessionOn(t *testing.T, opts agent.EventLogOptions, turns ...stubTurn) *stubSession {
	t.Helper()
	log := agent.NewEventLog(opts)
	s := &stubSession{
		log:    log,
		asks:   agent.NewAskRegistry(log, nil),
		closed: make(chan struct{}),
		turns:  turns,
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// textTurn is a turn that says text and ends end_turn: the first EventText is
// what starts the probe.
func textTurn(text string) stubTurn {
	return stubTurn{
		emit: []agent.Event{
			{Type: agent.EventText, Text: text},
			{Type: agent.EventDone, StopReason: "end_turn"},
		},
		res: agent.Result{StopReason: "end_turn"},
	}
}

// heldUntil is turn, run only once gate is closed — the probe's attach hook
// closes it — or, should it never be, after the watchdog, so the run ends and
// the test says why instead of hanging.
func heldUntil(gate <-chan struct{}, turn stubTurn) stubTurn {
	turn.before = func() {
		select {
		case <-gate:
		case <-time.After(stubWatchdog):
		}
	}
	return turn
}

// runChainWithin is runChain on a goroutine of its own, failing the test if
// it has not returned within the watchdog: a run held by the probe fails with
// a reason rather than hanging the package.
func runChainWithin(t *testing.T, o *promptOpts, s *stubSession, text string, followUps ...string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- runChain(o, context.Background(), s, text, followUps...) }()
	select {
	case err := <-done:
		return err
	case <-time.After(stubWatchdog):
		t.Fatal("the run never ended: the probe held the command")
		return nil
	}
}

// mustMake is the FIFO a hook made, reported: a hook that never ran, or a
// mkfifo that failed, would leave nothing for the test to be about.
func mustMake(t *testing.T, made <-chan error) {
	t.Helper()
	select {
	case err := <-made:
		if err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
	default:
		t.Fatal("the FIFO was never made")
	}
}

// readRegularProbeFile is readProbeFile for a PATH that may be a FIFO: it is
// checked with Lstat first, since reading a FIFO would wait for a writer.
func readRegularProbeFile(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the probe file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("PATH is %s, not the probe's regular file", fi.Mode())
	}
	return readProbeFile(t, path)
}

// heldWriter is a run's stdout that holds the run's reader, once, on the write
// that carries marker, until let is called; held is closed as the hold begins.
// consume folds an event into the probe's first client before it writes it,
// so a reader held here has folded that event and nothing after it. Only the
// reader writes, so fired needs no lock.
type heldWriter struct {
	buf     bytes.Buffer
	marker  []byte
	fired   bool
	held    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *heldWriter) Write(p []byte) (int, error) {
	if !w.fired && bytes.Contains(p, w.marker) {
		w.fired = true
		close(w.held)
		<-w.release
	}
	return w.buf.Write(p)
}

// let releases the hold; it may be called more than once.
func (w *heldWriter) let() { w.once.Do(func() { close(w.release) }) }

// awaitLastEnding reads sub until the chain's last ending — a turn ended with
// no successor and nothing queued — and returns its seq.
func awaitLastEnding(t *testing.T, sub *agent.Subscription) uint64 {
	t.Helper()
	timeout := time.After(stubWatchdog)
	for {
		select {
		case rec, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the test's subscription closed: %v", sub.Err())
			}
			if isLastEnding(rec) {
				return rec.Seq
			}
		case <-timeout:
			t.Fatal("the chain's last ending was never committed")
		}
	}
}

// isLastEnding says rec is the chain's last ending: a turn ended with no
// successor and nothing queued, the event the reader hands the comparison
// over at. An omitted record, or one that does not decode, is not.
func isLastEnding(rec agent.Record) bool {
	if rec.Omitted != nil {
		return false
	}
	ev, err := rec.Event()
	return err == nil && ev.Type == agent.EventTurn && ev.Turn != nil &&
		ev.Turn.Phase == agent.TurnEnded && ev.Turn.Next == "" && ev.Turn.Pending == 0
}

// setAttachProbeSettle shortens the cleanup's bounded wait for one test.
func setAttachProbeSettle(t *testing.T, d time.Duration) {
	t.Helper()
	prev := attachProbeSettle
	attachProbeSettle = d
	t.Cleanup(func() { attachProbeSettle = prev })
}

// releaseFakeGate writes the one byte the fake agent's gate waits for.
// Opening a FIFO for writing waits for its reader, so it returns once the
// agent is at its gate and has been let through: a barrier with no delay in
// it.
func releaseFakeGate(fifo string) error {
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte{1})
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

func readProbeFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the probe file: %v", err)
	}
	return string(body)
}

var (
	attachedLineRE = regexp.MustCompile(`^attached: snapshot seq (\d+), folded (\d+) records to seq (\d+), reattached (\d+)$`)
	firstSeqRE     = regexp.MustCompile(`(?m)^--- first, at seq (\d+) ---$`)
)

// parseAttachedLine is the probe file's second line: the snapshot's seq S,
// the records folded past it K, the seq the fold stands at, and the
// re-attaches R.
func parseAttachedLine(t *testing.T, line string) (s, k, to, r uint64) {
	t.Helper()
	m := attachedLineRE.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the probe file's second line %q is not an attached line", line)
	}
	return atou(t, m[1]), atou(t, m[2]), atou(t, m[3]), atou(t, m[4])
}

// firstSeq is the common seq n: the seq first was handed over at.
func firstSeq(t *testing.T, body string) uint64 {
	t.Helper()
	m := firstSeqRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the probe file has no first projection:\n%s", body)
	}
	return atou(t, m[1])
}

func atou(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
