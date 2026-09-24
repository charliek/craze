package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
)

// attachProbeReattachBound bounds how many times the probe's attached fold
// re-attaches with no cursor after an Omitted record or a subscription that
// closed with an error (plan 024 §3.6 items 3 and 6; execution amendment X22:
// at most two re-attaches per omitted record). A probe that needs more than
// this is not racing an ordinary omission, and reports why rather than loop.
const attachProbeReattachBound = 8

// attachProbeSettle bounds the probe's cleanup's wait for a comparison that
// was handed over and is still in flight (plan 024 §3.6 item 7): generous,
// since a live smoke session (§8 V1) is the flag's whole point. Past it the
// probe's context is cancelled, which ends whatever the goroutine was waiting
// for, and the file says ERROR. A variable only so a test can shorten it;
// production never writes it, and a probe reads it once, when it is built.
var attachProbeSettle = 20 * time.Second

// attachProbeAttached is a test seam, nil in production: the probe's
// goroutine calls it once its first attach's subscription is registered,
// before it folds a single record, with the probe's own context. A test
// releases a held agent from it (the SAME test), or holds the attached fold
// there until the cleanup's cancel (the deadlock test). A probe reads it once,
// when it is built.
var attachProbeAttached func(ctx context.Context)

// attachProbeReceived is a test seam, nil in production: the probe's
// goroutine calls it with every receive from its subscription's Records, as it
// takes it and before it does anything with it — a record (ok), or Records
// closing (!ok, rec zero). A test waits there for the moment a record — an
// Omitted one above all — or the subscription's end has reached the attached
// fold, or holds the fold there, rather than guessing at either with a sleep.
// A probe reads it once, when it is built.
var attachProbeReceived func(rec agent.Record, ok bool)

// attachProbeMaxItems is a test seam, 0 in production (the log's default,
// 1,024 records): the probe's subscription budget in records, on every attach
// it makes (engine.AttachOptions.MaxItems). A test shrinks it to have the log
// drop the attached fold's subscription as a slow consumer at a moment it
// chooses. A probe reads it once, when it is built.
var attachProbeMaxItems int

// attachProbe is `craze prompt --attach-probe=PATH` (plan 024 §5 row C4,
// hidden): a second, independent fold of the engine's own model, compared
// against the CLI's own read of the primary at a common seq. It follows
// C3's AttachClient (internal/engine/attach_test.go) — the shape a client
// folds an attachment in — but cannot import it: that file is test code.
//
// Three parties, and what each owns:
//
//   - The primary's reader (drive's goroutine: readChain, then finishRun's
//     sweeps) folds first through onEvent, one event at a time. It never
//     waits on the probe: at the chain's last ending it hands the goroutine
//     the common seq n and first's view at n over a buffered channel, and goes
//     back to draining.
//   - The probe's goroutine, started on the first EventText, attaches and
//     folds its subscription — never past what the reader has folded, and
//     never past n — then compares and writes PATH.
//   - The cleanup (settleAttachProbe, finishRun's last step, so on every exit
//     drive takes) waits, bounded, for a comparison in flight, then cancels
//     the probe's context, joins the goroutine and, when no comparison was
//     made, writes ERROR.
type attachProbe struct {
	path   string
	eng    *engine.Engine
	stderr io.Writer

	// run is the run's own context, which a signal ends. ctx is the probe's,
	// derived from it and cancelled by cleanup on every exit path before the
	// goroutine is joined: Attach honours it while it waits for the log's
	// boundary (X21), and the goroutine's every wait selects on it.
	run    context.Context
	ctx    context.Context
	cancel context.CancelFunc

	// settle, attached, received and maxItems are attachProbeSettle,
	// attachProbeAttached, attachProbeReceived and attachProbeMaxItems as they
	// stood when the probe was built.
	settle   time.Duration
	attached func(context.Context)
	received func(agent.Record, bool)
	maxItems int

	// The reader's own, touched only on drive's goroutine.
	//
	// first is "the first client" (plan 024 §5 row C4): folded from seq 1,
	// stamped only from events (Options.Clock nil) and reading an error's
	// text the way the primary carries it — the publisher's own value, not
	// the codec's *agent.RemoteError a decoding client is handed (X14) — so
	// both sides account the same bytes for it.
	first     *transcript.Model
	started   bool            // the goroutine has been started
	over      bool            // the chain's last ending was folded: first is frozen
	handed    *probeTarget    // what was handed to the goroutine, nil until then
	lastEnded *agent.TurnInfo // the last turn ending folded, for cleanup's reason

	// Between the reader and the goroutine. progress is first's seq, stored
	// after every fold, and progressed says it moved (a non-blocking send on a
	// channel of one): the goroutine folds no record the reader has not
	// folded, so it never runs past the n it will be handed. target carries
	// that hand-over, once, and never blocks the reader (a buffer of one).
	progress   atomic.Uint64
	progressed chan struct{}
	target     chan probeTarget
	joined     chan struct{} // closed when the goroutine has returned

	// The goroutine's own; cleanup reads them only after joined is closed.
	att      probeAttach
	wrote    bool  // the goroutine wrote PATH
	writeErr error // and failed to: cleanup reports it on stderr
}

// probeTarget is the reader's hand-over: the common seq, first's view there
// and its V6 measurement, all taken at the chain's last ending by the one
// goroutine that folds first — so, with nothing else folding it, from one
// consistent point.
type probeTarget struct {
	n        uint64
	view     probeView
	retained string
}

// probeAttach is the attached fold as it stands: the model restored from the
// current attachment's snapshot at snapSeq and folded folded records past it,
// and why it stopped short, when it did.
type probeAttach struct {
	model      *transcript.Model
	sub        *agent.Subscription
	snapSeq    uint64
	folded     int
	reattached []string
	// fatal is the verdict line of a fold that cannot go on ("ERROR: …", or
	// "DIFF: …" for a windowed snapshot); "" while it can.
	fatal string
}

// newAttachProbe builds a probe over eng, or nil when path is empty — the
// flag's own "off" state. Every other method treats a nil *attachProbe as a
// no-op, so the two call sites prompt.go carries (consume's onEvent and
// finishRun's settleAttachProbe) cost nothing when the flag was not given: no
// fold, no goroutine, no attach, no file.
//
// It is nil, too, when path already exists as anything but a regular file — a
// FIFO, a device, a directory, a symlink (Lstat: never followed, never
// opened). That is refused before anything else, on stderr — the flag's one
// error path — and no probe runs: PATH is only ever replaced by a regular file
// (writeProbeFile), and a destination that is not one is a mistake to report,
// not a file to overwrite (r10 finding 1). Any other Lstat error is left to the
// write, which reports it the same way.
func newAttachProbe(ctx context.Context, path string, eng *engine.Engine, stderr io.Writer) *attachProbe {
	if path == "" {
		return nil
	}
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "craze: --attach-probe: %s is not a regular file (mode %s); no probe run\n", path, fi.Mode())
		return nil
	}
	pctx, cancel := context.WithCancel(ctx)
	return &attachProbe{
		path:     path,
		eng:      eng,
		stderr:   stderr,
		run:      ctx,
		ctx:      pctx,
		cancel:   cancel,
		settle:   attachProbeSettle,
		attached: attachProbeAttached,
		received: attachProbeReceived,
		maxItems: attachProbeMaxItems,
		first: transcript.New(transcript.Options{
			ErrText: func(e error) string { return e.Error() },
		}),
		progressed: make(chan struct{}, 1),
		target:     make(chan probeTarget, 1),
		joined:     make(chan struct{}),
	}
}

// onEvent folds ev into the probe's first client, starts the attached fold on
// the first EventText, and at the chain's own end — nothing claimed and
// nothing queued behind this turn's ended, exactly the condition readChain's
// own turnEnded treats as nothing left to run — hands the comparison over.
// After that ending first is frozen and every later event is ignored. p may
// be nil.
func (p *attachProbe) onEvent(ev agent.Event) {
	if p == nil || p.over {
		return
	}
	p.first.Fold(ev)
	p.progress.Store(p.first.Seq())
	select {
	case p.progressed <- struct{}{}:
	default:
	}
	if ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnEnded {
		t := *ev.Turn
		p.lastEnded = &t
	}
	if !p.started && ev.Type == agent.EventText {
		p.started = true
		go p.loop()
	}
	if ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnEnded &&
		ev.Turn.Next == "" && ev.Turn.Pending == 0 {
		p.over = true
		if p.started {
			p.handOver()
		}
	}
}

// handOver is the reader's whole part in the comparison, and it never waits:
// first's seq, view and size at the chain's last ending — consecutive reads of
// a model nothing else folds, so one consistent point (transcript exports no
// single-cut accessor for all three projections) — sent to the goroutine on a
// channel with room for it.
func (p *attachProbe) handOver() {
	t := probeTarget{n: p.first.Seq(), view: viewOf(p.first), retained: p.retainedLine()}
	p.handed = &t
	p.target <- t
}

// settleAttachProbe is finishRun's last step and the probe's cleanup: drive
// defers finishRun, so it runs on every exit drive takes — the chain's clean
// end, a claim given up on (gaveUp) or refused as the agent's own turn's with
// follow-ups still queued, a drain given up on, a signal, a failed turn, the
// primary closing, a follow-up the queue refused, or a run in which no text
// ever came.
//
// When a comparison was handed over it waits for it, bounded by the probe's
// settle, and keeps reading the primary meanwhile, through consume, exactly as
// syncEvents does while it waits for Sync: the attached fold's own Attach may
// be waiting for the log's boundary, which a publisher holds while it waits
// for room in the primary, and only this reader frees that room. A primary
// that has closed has nothing left to read, and then the bound is all there
// is. When no comparison was handed over there is nothing to wait for. Either
// way cleanup then cancels, joins and writes what the goroutine did not.
func (o *promptOpts) settleAttachProbe(sess agent.Session, queue *[]string) (bool, error) {
	p := o.probe
	if p == nil {
		return false, nil
	}
	rejected := false
	var err error
	if p.handed != nil {
		timer := time.NewTimer(p.settle)
		defer timer.Stop()
		events := sess.Events()
	wait:
		for {
			select {
			case <-p.joined:
				break wait
			case <-timer.C:
				break wait
			case ev, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				r, cerr := o.consume(ev, queue)
				rejected = rejected || r
				if cerr != nil {
					// stdout is failing: stop writing to it, as syncEvents
					// does, and let the bound and the cancel end the wait.
					err, events = cerr, nil
				}
			}
		}
	}
	p.cleanup()
	return rejected, err
}

// cleanup cancels the probe's context, joins its goroutine — which closes its
// subscription on the way out — and writes PATH when the goroutine did not:
// "ERROR: never attached" when no text ever started it, and otherwise an
// ERROR naming why no comparison was made.
func (p *attachProbe) cleanup() {
	p.cancel()
	if p.started {
		<-p.joined
	}
	if p.writeErr != nil {
		fmt.Fprintf(p.stderr, "craze: --attach-probe: writing %s: %v\n", p.path, p.writeErr)
	}
	if p.wrote {
		return
	}
	var line string
	switch {
	case !p.started:
		line = "ERROR: never attached"
	case p.handed == nil:
		line = "ERROR: the chain ended without a clean last ending (" + p.why() + ")"
	default:
		// Handed over, but the goroutine had already returned: the run's
		// own context (a signal) ended the probe's before the chain's last
		// ending reached the reader.
		line = "ERROR: the probe's context ended before the comparison was handed over (" + p.why() + ")"
	}
	t := p.handed
	if t == nil {
		t = &probeTarget{n: p.first.Seq(), view: viewOf(p.first), retained: p.retainedLine()}
	}
	if err := p.writeFile(line, *t); err != nil {
		fmt.Fprintf(p.stderr, "craze: --attach-probe: writing %s: %v\n", p.path, err)
	}
}

// why is cleanup's account of an exit that made no comparison, from what the
// reader saw: a signal, and the last turn ending folded (the one that ended
// the run, or the last before it) with what was still queued behind it; and
// from what the goroutine hit, read after the join.
func (p *attachProbe) why() string {
	var parts []string
	if p.run.Err() != nil {
		parts = append(parts, "the run's context ended: a signal")
	}
	if t := p.lastEnded; t != nil {
		s := fmt.Sprintf("the last ending seen: %s, stop %q", t.ID, t.StopReason)
		if t.ErrClass != "" {
			s += fmt.Sprintf(", class %s", t.ErrClass)
		}
		if t.Next != "" {
			s += ", next " + t.Next
		}
		parts = append(parts, fmt.Sprintf("%s, %d still queued", s, t.Pending))
	} else {
		parts = append(parts, "no turn ended")
	}
	if p.att.fatal != "" {
		parts = append(parts, "the attached fold: "+p.att.fatal)
	}
	return strings.Join(parts, "; ")
}

// loop is the attached fold's own goroutine: never the primary's own reader
// (plan 024 §3.6 item 4). It attaches, folds, and — once it has the reader's
// hand-over — compares at exactly n and writes PATH. It returns without
// writing only when its context ended before anything was handed over; cleanup
// writes then.
func (p *attachProbe) loop() {
	defer close(p.joined)
	a := &p.att
	if p.attach(a) && p.attached != nil {
		p.attached(p.ctx)
	}
	target, stopped := p.fold(a)
	if a.sub != nil {
		a.sub.Close()
		a.sub = nil
	}
	if target == nil {
		t, ok := p.awaitTarget()
		if !ok {
			return
		}
		target = &t
	}
	line := p.verdict(a, *target, stopped)
	p.writeErr = p.writeFile(line, *target)
	p.wrote = true
}

// fold folds the subscription one record at a time, never past the reader's
// own progress and, once handed the target, never past its n: records the
// reader has not reached yet wait, in order, in pending (at most the log's
// lead over the primary, which a full primary bounds). It returns once the
// fold reached n (or its snapshot was already past it), once the fold cannot
// go on (a.fatal), or once the probe's context ended (stopped) — whatever it
// has been handed by then.
//
// An Omitted record, or Records closing with an error other than the log's own
// close (agent.ErrSlowConsumer, say), discards the fold and re-attaches with
// no cursor, bounded by attachProbeReattachBound, exactly as C3's AttachClient
// does — but only once it is next under the same limit an ordinary record
// waits for. Each waits in sequence, like any record: an omission in pending
// at its own seq, and a subscription's failure as a marker behind the last
// record it delivered, at the first seq it will never deliver (failedAt). One
// past the reader's progress may turn out to be past n, and a fold that
// discarded the prefix it holds up to n for it would re-attach past n, and so
// have nothing comparable to say (r10 finding 2). Once n is the limit, neither
// is ever acted on beyond it; one at or before n still restarts the fold. The
// log closing (agent.ErrClosed) is no failure a re-attach can mend, and ends
// the fold at once.
func (p *attachProbe) fold(a *probeAttach) (target *probeTarget, stopped bool) {
	var pending []agent.Record
	// failed is why the current subscription ended, "" while it runs, and
	// failedAt the seq its marker stands at: one past the last record it
	// delivered — pending's last, or the last the model folded (or was
	// restored at) when nothing is pending.
	var failed string
	var failedAt uint64
	// restart discards the fold, pending and a failure's marker included —
	// none of it is anything the next attachment's subscription will follow on
	// from — and re-attaches.
	restart := func(reason string) {
		pending, failed, failedAt = nil, "", 0
		if len(a.reattached) >= attachProbeReattachBound {
			a.fatal = fmt.Sprintf("ERROR: gave up after %d re-attaches (last: %s)", len(a.reattached), reason)
			return
		}
		a.reattached = append(a.reattached, reason)
		p.attach(a)
	}
	for a.fatal == "" {
		if p.ctx.Err() != nil {
			return target, true
		}
		limit := p.progress.Load()
		if target != nil {
			limit = target.n
		}
		restarted := false
		for len(pending) > 0 && pending[0].Seq <= limit {
			rec := pending[0]
			pending = pending[1:]
			if rec.Omitted != nil {
				restarted = true
				restart(fmt.Sprintf("omitted %d", rec.Seq))
				break
			}
			ev, err := rec.Event()
			if err != nil {
				a.fatal = fmt.Sprintf("ERROR: decode seq %d: %v", rec.Seq, err)
				return target, false
			}
			a.model.Fold(ev)
			a.folded++
		}
		if !restarted && failed != "" && len(pending) == 0 && failedAt <= limit {
			// The marker is next, and the limit needs what the subscription
			// will never deliver.
			restarted = true
			restart(failed)
		}
		if restarted {
			continue
		}
		if target != nil && a.model.Seq() >= target.n {
			return target, false
		}
		records := a.sub.Records()
		if failed != "" {
			// Closed, and a closed channel is always ready: wait on the
			// reader's progress, its hand-over or the context instead.
			records = nil
		}
		select {
		case rec, ok := <-records:
			if p.received != nil {
				p.received(rec, ok)
			}
			if ok {
				pending = append(pending, rec)
				continue
			}
			err := a.sub.Err()
			if errors.Is(err, agent.ErrClosed) {
				a.fatal = fmt.Sprintf("ERROR: the event log closed under the attached fold at seq %d", a.model.Seq())
				continue
			}
			failed, failedAt = fmt.Sprint(err), a.model.Seq()+1
			if len(pending) > 0 {
				failedAt = pending[len(pending)-1].Seq + 1
			}
		case <-p.progressed:
		case t := <-p.target:
			target = &t
		case <-p.ctx.Done():
		}
	}
	return target, false
}

// awaitTarget waits for the reader's hand-over or the probe's context,
// whichever is first — preferring a hand-over already made, so a comparison
// handed over before cleanup's cancel is always answered.
func (p *attachProbe) awaitTarget() (probeTarget, bool) {
	select {
	case t := <-p.target:
		return t, true
	default:
	}
	select {
	case t := <-p.target:
		return t, true
	case <-p.ctx.Done():
		select {
		case t := <-p.target:
			return t, true
		default:
			return probeTarget{}, false
		}
	}
}

// verdict is PATH's first line for a fold handed t: the fold's own failure;
// a snapshot already past n, which is never compared (plan 024 §3.6 item 7:
// views from different seqs say nothing); SAME or DIFF from the model stopped
// at exactly n — nothing folds it any more, so its three projections are one
// point; or, when the context ended first, an ERROR saying n was never
// reached.
func (p *attachProbe) verdict(a *probeAttach, t probeTarget, stopped bool) string {
	switch {
	case a.fatal != "":
		return a.fatal
	case a.snapSeq > t.n:
		return fmt.Sprintf("DIFF: snapshot at seq %d is past the common seq %d; not comparable", a.snapSeq, t.n)
	case a.model.Seq() == t.n:
		if d := diffViews(t.view, viewOf(a.model)); d != "" {
			return "DIFF: " + d
		}
		return "SAME"
	case stopped:
		return fmt.Sprintf("ERROR: the probe's context ended before the attached fold reached seq %d (it is at seq %d)", t.n, a.model.Seq())
	}
	return fmt.Sprintf("ERROR: the attached fold stopped at seq %d, short of seq %d", a.model.Seq(), t.n)
}

// attach attaches with no cursor, discarding whatever a had folded and closing
// its subscription, and reports whether it can fold. A failure sets a.fatal.
func (p *attachProbe) attach(a *probeAttach) bool {
	if a.sub != nil {
		a.sub.Close()
	}
	a.model, a.sub, a.snapSeq, a.folded = nil, nil, 0, 0
	model, sub, fatal := p.attachOnce()
	if fatal != "" {
		a.fatal = fatal
		return false
	}
	a.model, a.sub, a.snapSeq = model, sub, model.Seq()
	return true
}

// attachOnce attaches with no cursor, on the probe's own context, and
// Restores the snapshot it is handed. AttachOptions with no Cursor always asks
// for a snapshot (no cursor to honour; MaxItems is only the subscription's
// budget, attachProbeMaxItems), so unlike C3's AttachClient — whose adopt
// keeps the model when a resumed cursor is honoured — this never needs that
// branch: every attach the probe makes, first or re-attach alike, is with no
// cursor (plan 024 §3.6 items 3 and 6).
//
// A snapshot that comes back windowed is reported (DIFF), not folded: a probe
// attaching to a short live session is never windowed (plan 024 brief C4;
// execution amendment X23's windowing is for a session whose backlog since
// the cut outgrew a snapshot's budget, not a fresh attach's own cut).
func (p *attachProbe) attachOnce() (*transcript.Model, *agent.Subscription, string) {
	a, err := p.eng.Attach(p.ctx, engine.AttachOptions{MaxItems: p.maxItems})
	if err != nil {
		return nil, nil, "ERROR: attach: " + err.Error()
	}
	if a.Snapshot == nil {
		if a.Sub != nil {
			a.Sub.Close()
		}
		return nil, nil, "ERROR: attach answered with neither a snapshot nor an honoured cursor"
	}
	if a.Snapshot.Main.Windowed {
		a.Sub.Close()
		return nil, nil, fmt.Sprintf("DIFF: the main transcript's snapshot at seq %d came back windowed", a.Snapshot.Seq)
	}
	for _, s := range a.Snapshot.Subs {
		if s.Windowed {
			a.Sub.Close()
			return nil, nil, fmt.Sprintf("DIFF: child %s's snapshot at seq %d came back windowed", s.ID, a.Snapshot.Seq)
		}
	}
	return transcript.Restore(a.Snapshot, transcript.Options{}), a.Sub, ""
}

// attachedLine is PATH's second line: where the attached fold's current
// attachment was cut, how many records it folded past that, the seq it
// stands at, and how many times it re-attached — so a live run shows the
// attach was mid-turn (S before n, K above zero).
func (a *probeAttach) attachedLine() string {
	if a.model == nil {
		return fmt.Sprintf("attached: none, reattached %d", len(a.reattached))
	}
	return fmt.Sprintf("attached: snapshot seq %d, folded %d records to seq %d, reattached %d",
		a.snapSeq, a.folded, a.model.Seq(), len(a.reattached))
}

// writeFile writes PATH: line (SAME, "DIFF: …" or "ERROR: …"), the attached
// line, both projections as a readable JSON dump so a DIFF can be diagnosed —
// first's at t's seq, the attached fold's at its own — the re-attach reasons,
// and the V6 measurement: the first client's model size (plan 024 §5 row C4).
// It reads p.att, so it runs on the goroutine or, after the join, in cleanup.
//
// The probe never writes to stdout or stderr except, from cleanup, a failure to
// write PATH itself — an IO error — or, from newAttachProbe, a PATH it refuses,
// and only because the flag was given (the hard stop: with the flag absent the
// command's behaviour is exactly today's).
func (p *attachProbe) writeFile(line string, t probeTarget) error {
	a := &p.att
	var buf bytes.Buffer
	fmt.Fprintln(&buf, line)
	fmt.Fprintln(&buf, a.attachedLine())
	fmt.Fprintf(&buf, "--- first, at seq %d ---\n", t.n)
	writeProbeJSON(&buf, dumpOf(t.view))
	if a.model != nil {
		fmt.Fprintf(&buf, "--- attached, at seq %d ---\n", a.model.Seq())
		writeProbeJSON(&buf, dumpOf(viewOf(a.model)))
	} else {
		buf.WriteString("--- attached ---\n(none: the attached fold never produced one)\n")
	}
	if len(a.reattached) > 0 {
		fmt.Fprintf(&buf, "reattached: %s\n", strings.Join(a.reattached, "; "))
	}
	buf.WriteString(t.retained)
	buf.WriteString("\n")
	return writeProbeFile(p.path, buf.Bytes())
}

// writeProbeFile puts data at path without ever opening path: a regular
// temporary file created beside it — so on the same file system — is written,
// closed and renamed over it. Opening path itself for writing waits for good
// on a FIFO with no reader, and no context reaches a blocked open or write, so
// cleanup's cancel could not end it and its join would wait for ever (r10
// finding 1); a file created new and a rename never wait on whatever path is.
// newAttachProbe refuses a path that is not a regular file up front; this is
// what keeps one that becomes a FIFO afterwards from blocking all the same. The
// temporary file is created 0600, the mode PATH has always been written with,
// and is removed again when anything fails.
func writeProbeFile(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// retainedLine is the plan's V6 measurement: the first client's model size
// (Transcript.Len, Transcript.Bytes), e.g.
// "retained: main 412 entries 1843022 bytes; subs 3 / 57 entries / 90211 bytes".
// It reads first, so it runs on the reader.
func (p *attachProbe) retainedLine() string {
	main := p.first.Main
	subs := p.first.Subs()
	var entries, bts int
	for _, id := range subs {
		t := p.first.Sub(id)
		entries += t.Len()
		bts += t.Bytes()
	}
	return fmt.Sprintf("retained: main %d entries %d bytes; subs %d / %d entries / %d bytes",
		main.Len(), main.Bytes(), len(subs), entries, bts)
}

func writeProbeJSON(buf *bytes.Buffer, v any) {
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(buf, "(could not encode: %v)\n", err)
	}
}

// ------------------------------------------------------------ comparisons

// probeView is everything two folds of one sequence must agree on: both
// projections and the last-ended ask list. It is C3's AttachClient test
// helper's ModelView, copied rather than imported (attach_test.go is test
// code, and this package cannot import it).
type probeView struct {
	History transcript.History
	State   transcript.State
	Ended   []transcript.AskEnding
}

// viewOf reads m's three projections, each under m's own lock. They are one
// point only while nothing folds m between the reads: the probe takes them
// from first on the reader, its only folder, and from the attached model once
// its goroutine has stopped folding it.
func viewOf(m *transcript.Model) probeView {
	return probeView{History: m.History(), State: m.State(), Ended: m.EndedAsks()}
}

// diffProbeModels is "" when first and second agree, canonically, on every
// projection; else a short reason. It is the probe's comparison, exercised
// directly by attachprobe_test.go on two hand-folded models.
func diffProbeModels(first, second *transcript.Model) string {
	return diffViews(viewOf(first), viewOf(second))
}

// diffViews is "" when want and got agree, canonically (probeCanon): what a
// fold of the primary — the publisher's own error values — and a fold of a
// subscription — decoded ones — agree on once the codec's representation
// (times, errors) is set aside, mirroring C3's AttachClient test helper's
// DiffModels(canonical: true).
func diffViews(want, got probeView) string {
	want, got = probeCanon(want), probeCanon(got)
	switch {
	case !reflect.DeepEqual(want.History, got.History):
		return "the history projections differ"
	case !reflect.DeepEqual(want.State, got.State):
		return "the state projections differ"
	case !reflect.DeepEqual(want.Ended, got.Ended):
		return "the last-ended ask lists differ"
	}
	return ""
}

var (
	probeTimeType  = reflect.TypeFor[time.Time]()
	probeErrorType = reflect.TypeFor[error]()
)

// probeCanon is a deep copy of v with every time in UTC without a monotonic
// reading and every error the *agent.RemoteError the event codec makes of it
// (agent.RemoteErrorOf). It is C3's AttachClient test helper's Canon, copied
// rather than imported for the same reason ModelView is (attach_test.go is
// test code): the logic is otherwise unchanged.
func probeCanon[T any](v T) T {
	return probeCanonValue(reflect.ValueOf(&v).Elem()).Interface().(T)
}

func probeCanonValue(v reflect.Value) reflect.Value {
	t := v.Type()
	if t == probeTimeType {
		return reflect.ValueOf(v.Interface().(time.Time).UTC().Round(0))
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t).Elem()
		if t == probeErrorType {
			out.Set(reflect.ValueOf(agent.RemoteErrorOf(v.Interface().(error))))
			return out
		}
		out.Set(probeCanonValue(v.Elem()))
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t.Elem())
		out.Elem().Set(probeCanonValue(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(probeCanonValue(v.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(probeCanonValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := range v.Len() {
			out.Index(i).Set(probeCanonValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(probeCanonValue(it.Key()), probeCanonValue(it.Value()))
		}
		return out
	}
	return v
}

// ------------------------------------------------------------------ dumping

// probeDump is probeView flattened for JSON: State.Tools is keyed by
// transcript.ToolKey, a struct, which encoding/json cannot use as a map key,
// so it travels as a sorted slice instead.
type probeDump struct {
	History transcript.History
	State   probeStateDump
	Ended   []transcript.AskEnding
}

type probeStateDump struct {
	Seq             uint64
	Asks            []transcript.Ask
	Settings        transcript.Settings
	Queue           []agent.QueuedPrompt
	Agents          []agent.SubagentInfo
	Todos           []agent.Todo
	TodosTruncated  bool
	TruncatedQueue  map[string]bool
	Turn            transcript.Turn
	Replaying       bool
	Tools           []probeToolDump
	TruncatedAgents map[string]bool
}

type probeToolDump struct {
	Agent string
	ID    string
	Tool  *agent.ToolEvent
}

func dumpOf(v probeView) probeDump {
	tools := make([]probeToolDump, 0, len(v.State.Tools))
	for k, t := range v.State.Tools {
		tools = append(tools, probeToolDump{Agent: k.Agent, ID: k.ID, Tool: t})
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].Agent != tools[j].Agent {
			return tools[i].Agent < tools[j].Agent
		}
		return tools[i].ID < tools[j].ID
	})
	return probeDump{
		History: v.History,
		State: probeStateDump{
			Seq:             v.State.Seq,
			Asks:            v.State.Asks,
			Settings:        v.State.Settings,
			Queue:           v.State.Queue,
			Agents:          v.State.Agents,
			Todos:           v.State.Todos,
			TodosTruncated:  v.State.TodosTruncated,
			TruncatedQueue:  v.State.TruncatedQueue,
			Turn:            v.State.Turn,
			Replaying:       v.State.Replaying,
			Tools:           tools,
			TruncatedAgents: v.State.TruncatedAgents,
		},
		Ended: v.Ended,
	}
}
