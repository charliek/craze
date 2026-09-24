package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/transcript"
)

// ------------------------------------------------ a client's fold of attach

// AttachClient is a client's fold of an attachment (plan 024 §3.6), and the
// shape C4's probe copies: it restores the snapshot it is handed — or keeps the
// model it already has, when its cursor was honoured — and folds every record
// its subscription delivers, decoded (Record.Event), with no clock and no
// ErrText: a decoded record's error is already the *agent.RemoteError the
// engine's own model holds (X14). Two things void what it has folded, and each
// is answered the same way — discard it and attach again with NO cursor, which
// hands it a fresh snapshot to restore:
//
//   - an Omitted record, which no client can fold (the engine's model folded
//     it whole); resuming from its own cursor would be handed the same
//     omission again (panel astra 7, CodeRabbit 12);
//   - Records closing with an error — agent.ErrSlowConsumer, or an
//     agent.ErrCursorUnresolvable from the journal file's leg of a replay,
//     which may come after a prefix it already folded (CodeRabbit 3).
//
// agent.ErrClosed is the one ending it does not answer: a closed log has
// nothing to attach to. It is exported for engine_test's exactness tests, which
// drive it over tui.Stub from outside the package; it is a test's, and every
// method that can fail returns the error rather than failing a test, so a
// goroutine of the test's may drive it.
type AttachClient struct {
	ctl  Control
	opts AttachOptions // every attach after the first has no Cursor

	// Model is the client's fold.
	Model *transcript.Model
	// Sub is the subscription it is folding.
	Sub *agent.Subscription
	// Attachments is every attachment it has been handed, the first first, and
	// Folded how many records it folded from each.
	Attachments []*Attachment
	Folded      []int
	// Reattached says why each attach after the first happened, in order:
	// "omitted <seq>", or the error its subscription ended with.
	Reattached []string
}

// NewAttachClient attaches through ctl with o and adopts what it is handed.
func NewAttachClient(ctx context.Context, ctl Control, o AttachOptions) (*AttachClient, error) {
	c := &AttachClient{ctl: ctl, opts: o}
	c.opts.Cursor = nil
	if err := c.attach(ctx, o.Cursor); err != nil {
		return nil, err
	}
	return c, nil
}

// AdoptAttachment is a client whose first attachment a test made itself.
func AdoptAttachment(ctl Control, o AttachOptions, a *Attachment) *AttachClient {
	c := &AttachClient{ctl: ctl, opts: o}
	c.opts.Cursor = nil
	c.adopt(a, o.Cursor)
	return c
}

func (c *AttachClient) attach(ctx context.Context, cursor *agent.Cursor) error {
	o := c.opts
	o.Cursor = cursor
	a, err := c.ctl.Attach(ctx, o)
	if err != nil {
		return err
	}
	c.adopt(a, cursor)
	return nil
}

func (c *AttachClient) adopt(a *Attachment, cursor *agent.Cursor) {
	c.Attachments = append(c.Attachments, a)
	c.Folded = append(c.Folded, 0)
	c.Sub = a.Sub
	switch {
	case a.Snapshot != nil:
		// Whatever was folded before is discarded whole: the snapshot is the
		// model at its Seq.
		c.Model = transcript.Restore(a.Snapshot, transcript.Options{})
	case c.Model == nil:
		// A cursor honoured with nothing folded yet can only be a cursor at 0.
		c.Model = transcript.New(transcript.Options{Incarnation: cursor.Incarnation})
	}
}

// Seq is the last event the client has folded.
func (c *AttachClient) Seq() uint64 { return c.Model.Seq() }

// Resume attaches again from the client's own position — its cursor, which the
// log may honour, keeping the model, or refuse, handing it a snapshot.
func (c *AttachClient) Resume(ctx context.Context) error {
	c.Close()
	return c.attach(ctx, &agent.Cursor{Incarnation: c.Model.Incarnation(), Seq: c.Model.Seq()})
}

// Step folds the next record, or answers what voids the fold (the type's doc).
func (c *AttachClient) Step(ctx context.Context) error {
	select {
	case rec, ok := <-c.Sub.Records():
		if !ok {
			err := c.Sub.Err()
			if errors.Is(err, agent.ErrClosed) {
				return fmt.Errorf("the subscription ended: %w", err)
			}
			c.Reattached = append(c.Reattached, err.Error())
			return c.attach(ctx, nil)
		}
		if rec.Omitted != nil {
			c.Sub.Close()
			c.Reattached = append(c.Reattached, fmt.Sprintf("omitted %d", rec.Seq))
			return c.attach(ctx, nil)
		}
		if want := c.Model.Seq() + 1; rec.Seq != want {
			return fmt.Errorf("the subscription delivered seq %d after %d", rec.Seq, want-1)
		}
		ev, err := rec.Event()
		if err != nil {
			return err
		}
		c.Model.Fold(ev)
		c.Folded[len(c.Folded)-1]++
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the client was at seq %d: %w", c.Model.Seq(), ctx.Err())
	}
}

// FoldThrough steps until the client has folded seq.
func (c *AttachClient) FoldThrough(ctx context.Context, seq uint64) error {
	for c.Model.Seq() < seq {
		if err := c.Step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the subscription it is folding.
func (c *AttachClient) Close() {
	if c.Sub != nil {
		c.Sub.Close()
	}
}

// ------------------------------------------------------------ comparisons

// ModelView is everything two folds of one sequence must agree on: both
// projections and the last-ended ask list.
type ModelView struct {
	History transcript.History
	State   transcript.State
	Ended   []transcript.AskEnding
}

// ViewOf is m's view.
func ViewOf(m *transcript.Model) ModelView {
	return ModelView{History: m.History(), State: m.State(), Ended: m.EndedAsks()}
}

var (
	timeType  = reflect.TypeFor[time.Time]()
	errorType = reflect.TypeFor[error]()
)

// Canon is a deep copy of v with every time in UTC without a monotonic reading
// and every error the *agent.RemoteError the event codec makes of it: what a
// fold of the primary — the publisher's own values — and a fold of a
// subscription — decoded ones — agree on once the codec's representation is
// set aside (times travel as UTC RFC 3339, an error as its message, class and
// code). It never writes v's own storage: the payloads it walks are events'.
func Canon[T any](v T) T {
	return canon(reflect.ValueOf(&v).Elem()).Interface().(T)
}

func canon(v reflect.Value) reflect.Value {
	t := v.Type()
	if t == timeType {
		return reflect.ValueOf(v.Interface().(time.Time).UTC().Round(0))
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t).Elem()
		if t == errorType {
			out.Set(reflect.ValueOf(agent.RemoteErrorOf(v.Interface().(error))))
			return out
		}
		out.Set(canon(v.Elem()))
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t.Elem())
		out.Elem().Set(canon(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(canon(v.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(canon(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := range v.Len() {
			out.Index(i).Set(canon(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(canon(it.Key()), canon(it.Value()))
		}
		return out
	}
	return v
}

// DiffModels is "" when want and got agree on every projection, else where they
// first differ. canonical compares them through Canon (a primary's fold against
// a subscription's); otherwise they must be equal as they are.
func DiffModels(want, got *transcript.Model, canonical bool) string {
	return DiffViews(ViewOf(want), ViewOf(got), canonical)
}

// DiffViews is DiffModels over two views already taken.
func DiffViews(w, g ModelView, canonical bool) string {
	if canonical {
		w, g = Canon(w), Canon(g)
	}
	if !reflect.DeepEqual(w.History, g.History) {
		return "the histories differ: " + historyDiff(w.History, g.History)
	}
	if !reflect.DeepEqual(w.State, g.State) {
		return fmt.Sprintf("the states differ:\nwant %s\n got %s", describeState(w.State), describeState(g.State))
	}
	if !reflect.DeepEqual(w.Ended, g.Ended) {
		return fmt.Sprintf("the last-ended lists differ:\nwant %+v\n got %+v", w.Ended, g.Ended)
	}
	return ""
}

// historyDiff names the first transcript and entry at which two histories
// part.
func historyDiff(w, g transcript.History) string {
	pairs := []struct {
		id   string
		w, g transcript.TranscriptHistory
	}{{"main", w.Main, g.Main}}
	if len(w.Subs) != len(g.Subs) {
		return fmt.Sprintf("%d children, want %d", len(g.Subs), len(w.Subs))
	}
	for i := range w.Subs {
		if w.Subs[i].ID != g.Subs[i].ID {
			return fmt.Sprintf("child %d is %q, want %q", i, g.Subs[i].ID, w.Subs[i].ID)
		}
		pairs = append(pairs, struct {
			id   string
			w, g transcript.TranscriptHistory
		}{w.Subs[i].ID, w.Subs[i].TranscriptHistory, g.Subs[i].TranscriptHistory})
	}
	for _, p := range pairs {
		if reflect.DeepEqual(p.w, p.g) {
			continue
		}
		for i := range min(len(p.w.Entries), len(p.g.Entries)) {
			if !reflect.DeepEqual(p.w.Entries[i], p.g.Entries[i]) {
				return fmt.Sprintf("%s entry %d:\nwant %s\n got %s", p.id, i, describeEntry(p.w.Entries[i]), describeEntry(p.g.Entries[i]))
			}
		}
		if len(p.w.Entries) != len(p.g.Entries) {
			return fmt.Sprintf("%s holds %d entries, want %d", p.id, len(p.g.Entries), len(p.w.Entries))
		}
		return fmt.Sprintf("%s's flags: want trimmed %v windowed %v stream %v todo %d/%v, got %v %v %v %d/%v", p.id,
			p.w.Trimmed, p.w.Windowed, p.w.StreamOpen, p.w.TodoPlanned, p.w.TodoDone,
			p.g.Trimmed, p.g.Windowed, p.g.StreamOpen, p.g.TodoPlanned, p.g.TodoDone)
	}
	return "(no transcript differs, yet the histories do)"
}

func describeEntry(e transcript.Entry) string {
	text := e.Text
	if len(text) > 60 {
		text = fmt.Sprintf("%q…(%d bytes)", text[:60], len(text))
	} else {
		text = fmt.Sprintf("%q", text)
	}
	s := fmt.Sprintf("{%s %s %s at=%s end=%s open=%v streaming=%v interject=%v bytes=%d", e.ID, e.Kind, text,
		e.At.Format(time.RFC3339Nano), e.End.Format(time.RFC3339Nano), e.Open, e.Streaming, e.Interject, e.Bytes)
	if e.Tool != nil {
		s += fmt.Sprintf(" tool=%s/%s", e.Tool.ID, e.Tool.Status)
	}
	if e.Err != nil {
		s += fmt.Sprintf(" err=%#v", e.Err)
	}
	return s + "}"
}

func describeState(s transcript.State) string {
	return fmt.Sprintf("{seq %d, %d asks, settings %+v, queue %+v, %d agents, %d todos, turn %+v, replaying %v, %d tools, truncated %v}",
		s.Seq, len(s.Asks), s.Settings, s.Queue, len(s.Agents), len(s.Todos), s.Turn, s.Replaying, len(s.Tools), s.TruncatedAgents)
}

// DiffSuffix is "" when restored — a client restored from a WINDOWED snapshot
// and folded on — holds the first model's newest entries in every transcript,
// equal and under the same ids, with Windowed set on each, and the same state
// but for the tools whose rows only the first model holds (plan 024 §3.5;
// C2's TestWindowedRestoreContinuesTheSuffix states the same at the package
// level). canonical is DiffModels'.
func DiffSuffix(first, restored *transcript.Model, canonical bool) string {
	w, g := ViewOf(first), ViewOf(restored)
	if canonical {
		w, g = Canon(w), Canon(g)
	}
	if len(g.History.Subs) != len(w.History.Subs) {
		return fmt.Sprintf("%d children, the first model has %d", len(g.History.Subs), len(w.History.Subs))
	}
	pairs := [][2]transcript.TranscriptHistory{{w.History.Main, g.History.Main}}
	for i := range w.History.Subs {
		if w.History.Subs[i].ID != g.History.Subs[i].ID {
			return fmt.Sprintf("child %d is %q, the first model's %q", i, g.History.Subs[i].ID, w.History.Subs[i].ID)
		}
		pairs = append(pairs, [2]transcript.TranscriptHistory{w.History.Subs[i].TranscriptHistory, g.History.Subs[i].TranscriptHistory})
	}
	for i, p := range pairs {
		want, got := p[0], p[1]
		if len(got.Entries) > len(want.Entries) {
			return fmt.Sprintf("transcript %d: %d entries, the first model has %d", i, len(got.Entries), len(want.Entries))
		}
		if tail := want.Entries[len(want.Entries)-len(got.Entries):]; len(got.Entries) > 0 && !reflect.DeepEqual(tail, got.Entries) {
			for j := range tail {
				if !reflect.DeepEqual(tail[j], got.Entries[j]) {
					return fmt.Sprintf("transcript %d is not the first model's suffix at %d:\nwant %s\n got %s", i, j, describeEntry(tail[j]), describeEntry(got.Entries[j]))
				}
			}
		}
		if !got.Windowed || got.StreamOpen != want.StreamOpen || got.TodoPlanned != want.TodoPlanned || got.TodoDone != want.TodoDone {
			return fmt.Sprintf("transcript %d: windowed %v, stream %v (first %v), todo %d/%v (first %d/%v)", i,
				got.Windowed, got.StreamOpen, want.StreamOpen, got.TodoPlanned, got.TodoDone, want.TodoPlanned, want.TodoDone)
		}
	}
	for k, tool := range g.State.Tools {
		if !reflect.DeepEqual(w.State.Tools[k], tool) {
			return fmt.Sprintf("tool %v is %+v, the first model's %+v", k, tool, w.State.Tools[k])
		}
	}
	w.State.Tools, g.State.Tools = nil, nil
	if !reflect.DeepEqual(w.State, g.State) {
		return fmt.Sprintf("the states differ:\nwant %s\n got %s", describeState(w.State), describeState(g.State))
	}
	return ""
}

// ------------------------------------------------------------ rig helpers

// primaryCap is internal/agent's primary buffer: the 257th unread event waits.
const primaryCap = 256

// Frames the leak checks count, as runtime.Stack prints them.
const (
	ownerFrame  = "github.com/charliek/craze/internal/agent.(*Subscription).run("
	attachFrame = "github.com/charliek/craze/internal/engine.(*Engine).Attach("
)

// goroutinesRunning counts the goroutines with frame on their stack.
func goroutinesRunning(frame string) int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte(frame))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// watchdogCtx is a context the watchdog ends: a wait under it that fails is a
// deadlock, never a timing assertion.
func watchdogCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	t.Cleanup(cancel)
	return ctx
}

// textOf is an event with n bytes of text (a text event's record is ~n+100
// bytes).
func textOf(n int, c string) agent.Event {
	return agent.Event{Type: agent.EventText, Text: strings.Repeat(c, n)}
}

// publish commits evs through the session's own emit, one at a time, and
// returns the committed cutoff after the last: under NoPrimary a publish that
// returned has committed, and the observer has folded it.
func (r *rig) publish(evs ...agent.Event) uint64 {
	r.t.Helper()
	for _, ev := range evs {
		r.s.emit(ev)
	}
	return r.e.model.Seq()
}

// quietSeq is the committed cutoff once the outbox has drained and nothing is
// publishing: the engine's model's Seq.
func (r *rig) quietSeq() uint64 {
	r.t.Helper()
	r.sync()
	return r.e.model.Seq()
}

// assertMatches folds c through the engine's cutoff and holds it to the
// engine's own model there: both are at the same Seq, and nothing publishes
// while they are compared.
func assertMatches(t *testing.T, r *rig, c *AttachClient, what string) {
	t.Helper()
	n := r.quietSeq()
	if err := c.FoldThrough(watchdogCtx(t), n); err != nil {
		t.Fatalf("%s: folding through %d: %v", what, n, err)
	}
	if got := c.Seq(); got != n {
		t.Fatalf("%s: the client is at %d, the engine at %d", what, got, n)
	}
	if d := DiffModels(r.e.model, c.Model, true); d != "" {
		t.Fatalf("%s: the client's fold is not the engine's at seq %d: %s", what, n, d)
	}
}

func mustAttach(t *testing.T, r *rig, o AttachOptions) *AttachClient {
	t.Helper()
	c, err := NewAttachClient(watchdogCtx(t), r.e, o)
	if err != nil {
		t.Fatalf("attach %+v: %v", o, err)
	}
	t.Cleanup(c.Close)
	return c
}

// testJournal is a journal in a fresh directory for an incarnation of its own,
// with its options edited first, and the log options that feed it. The
// cleanup waits for its writer to finish, after the rig's (cleanups run last
// first).
func testJournal(t *testing.T, edit func(*journal.Options)) (*journal.Writer, agent.EventLogOptions) {
	t.Helper()
	inc := agent.NewIncarnation()
	o := journal.Options{
		Dir: filepath.Join(t.TempDir(), "journal"), Incarnation: inc, Cwd: t.TempDir(),
		Provider: "test", EventCodec: agent.EventCodecVersion,
	}
	if edit != nil {
		edit(&o)
	}
	w, err := journal.New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = w.Close(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), watchdog)
		defer cancel()
		_ = w.WaitFlushed(ctx, journal.MaxSeq) // ErrClosed once the writer has finished
	})
	return w, agent.EventLogOptions{NoPrimary: true, Journal: w, Incarnation: inc}
}

// ------------------------------------------------------------------- A8

// TestAttachWithAnUnresolvableCursorFallsBackToASnapshot (A8): each way the log
// refuses a cursor synchronously — another incarnation, a future seq, a
// backlog over the subscription's MaxBytes, a head that has left the ring with
// no journal, a gap the journal recorded — is answered with a snapshot and
// Reset naming the reason, and the client that restores it matches the
// engine. A cursor the ring serves is honoured: no snapshot, no Reset, and the
// client folds on from its own model.
func TestAttachWithAnUnresolvableCursorFallsBackToASnapshot(t *testing.T) {
	for _, tc := range []struct {
		reason agent.CursorReason
		lo     func(t *testing.T) agent.EventLogOptions
		// cursor is the cursor the client brings, given the log's incarnation.
		cursor   func(inc string) agent.Cursor
		maxBytes int
	}{
		{reason: agent.CursorForeignIncarnation, cursor: func(string) agent.Cursor { return agent.Cursor{Incarnation: "another", Seq: 2} }},
		{reason: agent.CursorFutureSeq, cursor: func(inc string) agent.Cursor { return agent.Cursor{Incarnation: inc, Seq: 99} }},
		{reason: agent.CursorBacklogTooLarge, cursor: func(inc string) agent.Cursor { return agent.Cursor{Incarnation: inc, Seq: 1} }, maxBytes: 2 << 10},
		{
			reason: agent.CursorNoJournal,
			lo:     func(*testing.T) agent.EventLogOptions { return agent.EventLogOptions{NoPrimary: true, RingEvents: 4} },
			cursor: func(inc string) agent.Cursor { return agent.Cursor{Incarnation: inc, Seq: 1} },
		},
		{
			// A journal whose queue can take nothing drops every record into a
			// gap it records at once, which is what the cutoff consults.
			reason: agent.CursorJournalGap,
			lo: func(t *testing.T) agent.EventLogOptions {
				_, lo := testJournal(t, func(o *journal.Options) { o.QueueBytes = 1 })
				lo.RingEvents = 4
				return lo
			},
			cursor: func(inc string) agent.Cursor { return agent.Cursor{Incarnation: inc, Seq: 1} },
		},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			lo := agent.EventLogOptions{NoPrimary: true}
			if tc.lo != nil {
				lo = tc.lo(t)
			}
			r := newRigOn(t, Options{}, lo)
			for i := range 10 {
				r.publish(textOf(1<<10, string(rune('a'+i))), agent.Event{Type: agent.EventThought, Text: "hm"})
			}
			cur := tc.cursor(r.e.log.Incarnation())
			c := mustAttach(t, r, AttachOptions{Cursor: &cur, MaxBytes: tc.maxBytes})
			a := c.Attachments[0]
			if a.Snapshot == nil || a.Reset != tc.reason {
				t.Fatalf("a %s cursor was answered with snapshot %v, reset %q", tc.reason, a.Snapshot != nil, a.Reset)
			}
			if n := r.quietSeq(); a.Snapshot.Seq != n {
				t.Fatalf("the snapshot is at %d, the engine at %d", a.Snapshot.Seq, n)
			}
			assertMatches(t, r, c, "restored")
			r.publish(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", Status: "pending"}}, textOf(10, "z"))
			assertMatches(t, r, c, "folded on")
		})
	}

	t.Run("honoured", func(t *testing.T) {
		r := newRig(t, Options{})
		r.publish(textOf(100, "a"))
		c := mustAttach(t, r, AttachOptions{})
		assertMatches(t, r, c, "attached")
		c.Close()
		r.publish(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", Status: "pending"}}, textOf(10, "b"))
		if err := c.Resume(watchdogCtx(t)); err != nil {
			t.Fatal(err)
		}
		if a := c.Attachments[1]; a.Snapshot != nil || a.Reset != "" {
			t.Fatalf("a cursor the ring serves was answered with snapshot %v, reset %q", a.Snapshot != nil, a.Reset)
		}
		assertMatches(t, r, c, "resumed from its own cursor")
		if c.Folded[1] != 2 {
			t.Fatalf("the resumed client folded %d records, want the 2 after its cursor", c.Folded[1])
		}
	})
}

// TestAttachRetriesWhenTheRingMovedPastTheSnapshot (A8, panel astra 5 and
// CodeRabbit 4): between Attach's snapshot and its Subscribe (the
// attachSnapshotted hook is that gap) the log moves past the snapshot's Seq —
// by count, by bytes, or by more backlog than the subscription's MaxBytes —
// and the refusal, whatever its reason, is answered with a fresh snapshot.
// Reset stays "" (no cursor was given), and the client matches. Four refusals
// in a row are ErrAttachRaced, code unavailable, with nothing registered.
func TestAttachRetriesWhenTheRingMovedPastTheSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		lo   agent.EventLogOptions
		o    AttachOptions
		// burst is what moves the log past the first snapshot.
		burst []agent.Event
	}{
		{
			name: "by count", lo: agent.EventLogOptions{NoPrimary: true, RingEvents: 8},
			burst: repeatEvent(textOf(8, "c"), 9),
		},
		{
			// Three records of ~5 KiB in an 8 KiB ring: each push evicts by
			// bytes, far below the ring's 4,096-record count bound.
			name: "by bytes", lo: agent.EventLogOptions{NoPrimary: true, RingBytes: 8 << 10},
			burst: repeatEvent(textOf(5<<10, "b"), 3),
		},
		{
			name: "past the subscription's budget", lo: agent.EventLogOptions{NoPrimary: true},
			o:     AttachOptions{MaxBytes: 4 << 10},
			burst: repeatEvent(textOf(3<<10, "m"), 2),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r *rig
			var seen []uint64
			r = newRigHooked(t, Options{}, tc.lo, &hooks{attachSnapshotted: func(seq uint64, attempt int) {
				seen = append(seen, seq)
				if attempt == 0 {
					r.publish(tc.burst...)
				}
			}})
			first := r.publish(textOf(10, "x"), textOf(10, "y"), agent.Event{Type: agent.EventThought, Text: "z"})
			c := mustAttach(t, r, tc.o)
			moved := first + uint64(len(tc.burst))
			if len(seen) != 2 || seen[0] != first || seen[1] != moved {
				t.Fatalf("the snapshots were cut at %v, want %d and then %d", seen, first, moved)
			}
			if a := c.Attachments[0]; a.Snapshot.Seq != moved || a.Reset != "" {
				t.Fatalf("the attachment is at %d with reset %q, want %d and none", a.Snapshot.Seq, a.Reset, moved)
			}
			r.publish(textOf(10, "after"))
			assertMatches(t, r, c, "after the retry")
		})
	}

	t.Run("four in a row", func(t *testing.T) {
		var r *rig
		var seen []int
		r = newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true, RingEvents: 8}, &hooks{attachSnapshotted: func(_ uint64, attempt int) {
			seen = append(seen, attempt)
			r.publish(repeatEvent(textOf(8, "c"), 9)...)
		}})
		r.publish(textOf(10, "x"))
		owners := goroutinesRunning(ownerFrame)
		a, err := r.e.Attach(watchdogCtx(t), AttachOptions{})
		if a != nil || !errors.Is(err, ErrAttachRaced) || Code(err) != "unavailable" || !gateRefusal(err) {
			t.Fatalf("four refusals in a row: %v, %v (code %q)", a, err, Code(err))
		}
		if fmt.Sprint(seen) != "[0 1 2 3]" {
			t.Fatalf("attempts %v, want the first and three retries", seen)
		}
		if got := goroutinesRunning(ownerFrame); got > owners {
			t.Fatalf("%d subscription owners run after a raced attach, %d before", got, owners)
		}
		// Nothing was registered, so the log's next commits reach only the
		// rig's own subscription and the attach can simply be asked again.
		c := mustAttach(t, r, AttachOptions{Cursor: &agent.Cursor{Incarnation: r.e.log.Incarnation(), Seq: r.quietSeq()}})
		if c.Attachments[0].Snapshot != nil {
			t.Fatal("a cursor at the cutoff was not honoured after a raced attach")
		}
	})
}

func repeatEvent(ev agent.Event, n int) []agent.Event {
	out := make([]agent.Event, n)
	for i := range out {
		out[i] = ev
	}
	return out
}

// corruptJournalLine breaks seq's event line in w's file in place: its first
// byte, so the line is still whole and the reader finds it malformed only when
// it gets there, after the lines before it have gone out.
func corruptJournalLine(t *testing.T, w *journal.Writer, seq uint64) {
	t.Helper()
	raw, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(raw, fmt.Appendf(nil, `"type":"event","seq":%d,`, seq))
	if i < 0 {
		t.Fatalf("seq %d has no line in the journal", seq)
	}
	start := bytes.LastIndexByte(raw[:i], '\n') + 1
	f, err := os.OpenFile(w.Path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("#"), int64(start)); err != nil {
		t.Fatal(err)
	}
}

// TestAnAsynchronousReplayFailureIsAReset (A8, CodeRabbit 3): a client resumes
// from a cursor whose head has left the ring; the journal holds it, so the
// cursor is honoured synchronously, and the file leg delivers a prefix and then
// finds a line it cannot read. The subscription ends with that reason through
// Sub.Err() (evicted), the client discards what it folded — the prefix
// included — and attaches again with no cursor, and matches.
func TestAnAsynchronousReplayFailureIsAReset(t *testing.T) {
	w, lo := testJournal(t, nil)
	lo.RingEvents = 8
	r := newRigOn(t, Options{}, lo)
	for i := range 5 {
		r.publish(textOf(20, string(rune('a'+i))))
	}
	c := mustAttach(t, r, AttachOptions{})
	assertMatches(t, r, c, "attached at 5")
	c.Close()
	for i := range 35 {
		r.publish(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%d", i%7), Status: fmt.Sprint(i)}})
	}
	last := r.quietSeq()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, last); err != nil {
		t.Fatal(err)
	}
	corruptJournalLine(t, w, 15)

	if err := c.Resume(watchdogCtx(t)); err != nil {
		t.Fatal(err)
	}
	if a := c.Attachments[1]; a.Snapshot != nil || a.Reset != "" {
		t.Fatalf("the resume was answered with snapshot %v, reset %q: the journal's leg is asynchronous", a.Snapshot != nil, a.Reset)
	}
	failed := c.Sub
	assertMatches(t, r, c, "after the failed leg")
	var refused agent.ErrCursorUnresolvable
	if !errors.As(failed.Err(), &refused) || refused.Reason != agent.CursorEvicted {
		t.Fatalf("the failed subscription ended with %v, want evicted", failed.Err())
	}
	if len(c.Attachments) != 3 || c.Folded[1] != 9 || len(c.Reattached) != 1 || c.Attachments[2].Snapshot == nil {
		t.Fatalf("attachments %d, folded %v, reattached %q: want the prefix 6..14 folded and discarded, then one snapshot",
			len(c.Attachments), c.Folded, c.Reattached)
	}
	r.publish(textOf(10, "on"))
	assertMatches(t, r, c, "folded on")
}

// TestAnOmittedRecordMakesTheClientReattach (A8, panel astra 7, CodeRabbit
// 12): an event over MaxRecordBytes after the client's cut is folded whole by
// the engine's model and reaches the subscription as an Omitted record. The
// client re-attaches with no cursor — a snapshot at or beyond the omitted
// seq — and matches the engine, oversized event included.
func TestAnOmittedRecordMakesTheClientReattach(t *testing.T) {
	r := newRigOn(t, Options{}, agent.EventLogOptions{NoPrimary: true, MaxRecordBytes: 16 << 10})
	r.publish(textOf(10, "a"), agent.Event{Type: agent.EventThought, Text: "hm"})
	c := mustAttach(t, r, AttachOptions{})
	omitted := r.publish(textOf(20<<10, "o"))
	r.publish(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", Status: "pending"}}, textOf(10, "b"))
	assertMatches(t, r, c, "after the omission")
	if len(c.Reattached) == 0 || c.Reattached[0] != fmt.Sprintf("omitted %d", omitted) {
		t.Fatalf("the client re-attached for %q, want the omitted record %d", c.Reattached, omitted)
	}
	if last := c.Attachments[len(c.Attachments)-1]; last.Snapshot == nil || last.Snapshot.Seq < omitted {
		t.Fatalf("the last attachment is not a snapshot at or past %d", omitted)
	}
	// The client's model holds what the oversized event drew: the stream's tail.
	if tail := c.Model.History().Main.Entries; len(tail) == 0 || !strings.Contains(fmt.Sprint(tail), "ooooo") {
		t.Fatal("the restored model does not hold the oversized event's text")
	}
}

// TestAttachDuringAReplayIsServedAtTheCutoff (A8, §3.5 "Replayed and load"):
// an attach inside a replay bracket is answered at the cutoff like any other —
// the snapshot says Replaying — and the client folds on through the bracket's
// end and matches.
func TestAttachDuringAReplayIsServedAtTheCutoff(t *testing.T) {
	r := newRig(t, Options{})
	r.publish(
		agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}},
		agent.Event{Type: agent.EventUser, Text: "an old prompt", Replayed: true},
		agent.Event{Type: agent.EventThought, Text: "old thinking", Replayed: true},
	)
	c := mustAttach(t, r, AttachOptions{})
	if s := c.Attachments[0].Snapshot; !s.Replaying || s.Seq != 3 {
		t.Fatalf("the snapshot inside the bracket is at %d, replaying %v", s.Seq, s.Replaying)
	}
	r.publish(
		agent.Event{Type: agent.EventText, Text: "an old answer", Replayed: true},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "old", Status: "completed"}, Replayed: true},
		agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}},
		textOf(10, "live"),
	)
	assertMatches(t, r, c, "through the bracket's end")
	if c.Model.State().Replaying {
		t.Fatal("the client is still replaying after the bracket's end")
	}
}

// parkTheDrainer leaves the outbox's drainer holding the publishing boundary
// on a full, unread primary: 255 events fill all but one slot, and a batch of
// two is enqueued. Its first event takes the last slot and is committed — the
// rig's subscription delivers it, which is the barrier — and the second then
// waits for room inside the same hold of the boundary, a batch being one hold.
// So from the barrier on, the boundary stays held until the primary is read.
// It returns the seq of the batch's second event.
func parkTheDrainer(t *testing.T, r *rig) uint64 {
	t.Helper()
	for i := range primaryCap - 1 {
		if !r.s.log.Publish(context.Background(), nil, textOf(8, string(rune('a'+i%26)))) {
			t.Fatal("filling the primary")
		}
	}
	r.s.log.Enqueue(textOf(8, "A"), textOf(8, "B"))
	r.until(func(ev agent.Event) bool { return ev.Seq == primaryCap })
	return primaryCap + 1
}

type attachResult struct {
	a   *Attachment
	err error
}

// TestAttachIsCancellableWithThePrimaryFull (A8, panel astra 6): a publisher
// holds the boundary on a full primary nobody is reading, and Attach — past its
// snapshot, into Subscribe's wait for the boundary — is cancelled: it returns
// ctx's error with the primary still unread, registers nothing and leaves no
// goroutine behind. The log carries on once the primary is read.
func TestAttachIsCancellableWithThePrimaryFull(t *testing.T) {
	snapped := make(chan uint64, 1)
	r := newRigHooked(t, Options{}, agent.EventLogOptions{}, &hooks{attachSnapshotted: func(seq uint64, _ int) { snapped <- seq }})
	parked := parkTheDrainer(t, r)
	owners := goroutinesRunning(ownerFrame)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan attachResult, 1)
	go func() {
		a, err := r.e.Attach(ctx, AttachOptions{})
		done <- attachResult{a, err}
	}()
	if seq := awaitValue(t, snapped, "the snapshot"); seq != parked-1 {
		t.Fatalf("the snapshot is at %d, want %d", seq, parked-1)
	}
	cancel()
	res := awaitValue(t, done, "the cancelled attach")
	if res.a != nil || !errors.Is(res.err, context.Canceled) {
		t.Fatalf("the cancelled attach returned %v, %v; want nil, context.Canceled", res.a, res.err)
	}
	if n := len(r.e.Events()); n != primaryCap {
		t.Fatalf("the primary holds %d events: it was read before the attach was released", n)
	}
	if got := goroutinesRunning(attachFrame); got != 0 {
		t.Fatalf("%d goroutines are still in Attach", got)
	}
	if got := goroutinesRunning(ownerFrame); got > owners {
		t.Fatalf("%d subscription owners run after a cancelled attach, %d before", got, owners)
	}

	<-r.e.Events()
	r.until(func(ev agent.Event) bool { return ev.Seq == parked })
	if got := goroutinesRunning(ownerFrame); got > owners {
		t.Fatalf("%d subscription owners run once the log moved on, %d before: the cancelled attach registered one", got, owners)
	}
}

// TestAttachWhileAPublisherHoldsTheBoundary (A8, §3.6 item 8, CodeRabbit 21):
// the same parked publisher, and an Attach that has cut its snapshot and is
// waiting for the boundary. When the primary is read, the publisher commits
// its second event — and folds it, taking the model's mutex — before it lets
// the boundary go. The attach completes: an Attach that held the mutex across
// Subscribe would be waiting for a publisher waiting for it, and this test
// would end at the watchdog. The client then matches, the committed event
// replayed from the cut.
func TestAttachWhileAPublisherHoldsTheBoundary(t *testing.T) {
	snapped := make(chan uint64, 1)
	r := newRigHooked(t, Options{}, agent.EventLogOptions{}, &hooks{attachSnapshotted: func(seq uint64, _ int) { snapped <- seq }})
	parked := parkTheDrainer(t, r)
	done := make(chan attachResult, 1)
	go func() {
		a, err := r.e.Attach(context.Background(), AttachOptions{})
		done <- attachResult{a, err}
	}()
	if seq := awaitValue(t, snapped, "the snapshot"); seq != parked-1 {
		t.Fatalf("the snapshot is at %d, want %d", seq, parked-1)
	}
	select {
	case res := <-done:
		t.Fatalf("the attach returned (%v) while a publisher held the boundary", res.err)
	default:
	}
	<-r.e.Events()
	res := awaitValue(t, done, "the attach once the primary was read")
	if res.err != nil || res.a.Snapshot == nil || res.a.Snapshot.Seq != parked-1 {
		t.Fatalf("the attach returned %+v, %v", res.a, res.err)
	}
	c := AdoptAttachment(r.e, AttachOptions{}, res.a)
	t.Cleanup(c.Close)
	if err := c.FoldThrough(watchdogCtx(t), parked); err != nil {
		t.Fatal(err)
	}
	if c.Folded[0] != 1 {
		t.Fatalf("the client folded %d records to reach %d, want the one committed while it waited", c.Folded[0], parked)
	}
	drainPrimaryInto(r)
	assertMatches(t, r, c, "after the publisher let go")
}

// drainPrimaryInto empties the primary, so a rig built with one can sync.
func drainPrimaryInto(r *rig) {
	for {
		select {
		case <-r.e.Events():
		default:
			return
		}
	}
}

// awaitValue is await for a channel that carries a value.
func awaitValue[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(watchdog):
		t.Fatalf("still waiting for %s after %s", what, watchdog)
	}
	panic("unreachable")
}

// TestAStalledAttachedSubscriberIsDroppedNotWaitedFor (A8): an attached client
// that never reads is ended with ErrSlowConsumer once its buffer overflows —
// no publish waits for it, and the engine runs a turn and the rig's own
// subscriber sees all of it. The client then re-attaches, a reset in process,
// and matches.
func TestAStalledAttachedSubscriberIsDroppedNotWaitedFor(t *testing.T) {
	r := newRig(t, Options{})
	r.publish(textOf(10, "a"))
	c := mustAttach(t, r, AttachOptions{MaxItems: 4})
	stalled := c.Sub
	r.submit("hello")
	r.until(lastEnding)
	for i := range 20 {
		r.publish(textOf(10, string(rune('a'+i))))
	}
	if st := r.e.State(); st.Activity != ActivityIdle {
		t.Fatalf("the engine is %s with a stalled subscriber", st.Activity)
	}
	if h := r.e.log.Health(); h.SubscribersDropped != 1 {
		t.Fatalf("%d subscribers dropped, want the stalled one", h.SubscribersDropped)
	}
	assertMatches(t, r, c, "re-attached after the drop")
	if !errors.Is(stalled.Err(), agent.ErrSlowConsumer) || len(c.Reattached) != 1 {
		t.Fatalf("the stalled subscription ended with %v; the client re-attached for %q", stalled.Err(), c.Reattached)
	}
}

// TestCloseWithAFullPrimaryAndAnAttachedSubscriber (A8, §3.6 item 7) holds the
// LOG's documented behaviour at Close, never equality: with the primary full
// and unread, events still in the outbox are committed without reaching the
// primary (OutboxSkippedPrimary), the engine's model folds them, and the
// attached subscription is terminated with ErrClosed without a drain
// guarantee — what it delivered is a contiguous run from its cut, of whatever
// length. Attaching to the closed engine is ErrClosed.
func TestCloseWithAFullPrimaryAndAnAttachedSubscriber(t *testing.T) {
	r := newRigOn(t, Options{}, agent.EventLogOptions{})
	r.publish(textOf(10, "a"), textOf(10, "b"))
	c := mustAttach(t, r, AttachOptions{})
	cut := c.Seq()
	for len(r.e.Events()) < primaryCap {
		r.s.log.Publish(context.Background(), nil, textOf(8, "f"))
	}
	r.s.log.Enqueue(textOf(8, "x"), textOf(8, "y"), textOf(8, "z"))
	// The drainer holds the first of them on the full primary, uncommitted.
	if got := r.e.model.Seq(); got != primaryCap {
		t.Fatalf("the engine's model is at %d before Close, want %d", got, primaryCap)
	}
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	if h := r.e.log.Health(); h.OutboxSkippedPrimary != 3 {
		t.Fatalf("%d enqueued events were committed past the full primary, want the 3 the outbox held", h.OutboxSkippedPrimary)
	}
	if got := r.e.model.Seq(); got != primaryCap+3 {
		t.Fatalf("the engine's model is at %d, want every event the close committed (%d)", got, primaryCap+3)
	}
	var seqs []uint64
	for len(r.e.Events()) > 0 {
		seqs = append(seqs, (<-r.e.Events()).Seq)
	}
	if len(seqs) != primaryCap || seqs[0] != 1 || seqs[len(seqs)-1] != primaryCap {
		t.Fatalf("the primary holds %d events, %v…, want 1..%d", len(seqs), seqs[:min(len(seqs), 3)], primaryCap)
	}
	delivered := 0
	for rec := range c.Sub.Records() {
		if rec.Seq != cut+uint64(delivered)+1 {
			t.Fatalf("the attached subscription delivered %d after %d: a hole", rec.Seq, cut+uint64(delivered))
		}
		delivered++
	}
	if !errors.Is(c.Sub.Err(), agent.ErrClosed) || cut+uint64(delivered) > primaryCap+3 {
		t.Fatalf("the attached subscription ended with %v after %d records", c.Sub.Err(), delivered)
	}
	t.Logf("the attached subscription delivered %d of the %d records after its cut before Close ended it", delivered, primaryCap+3-cut)
	if _, err := r.e.Attach(watchdogCtx(t), AttachOptions{}); !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("attaching to a closed engine: %v, want agent.ErrClosed", err)
	}
}

// ------------------------------------------------------------------- A10

// TestTheEngineFoldsEveryCommittedEventInOrder (A10): the engine's model folds
// exactly the committed sequence, in order — its Seq is each event's own once
// the observer has returned, and a model folded from the committed records
// equals it — and a sub-agent's events reach the child's transcript and the
// roster, never the main transcript, while the engine's own flags still ignore
// them (a child's replay bracket does not make the engine replay).
func TestTheEngineFoldsEveryCommittedEventInOrder(t *testing.T) {
	r := newRig(t, Options{})
	evs := []agent.Event{
		{Type: agent.EventThought, Text: "main thinks"},
		textOf(10, "m"),
		{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned},
		{Type: agent.EventUser, Agent: "sub-1", Text: "CHILD-ONLY prompt"},
		{Type: agent.EventThought, Agent: "sub-1", Text: "CHILD-ONLY thought"},
		{Type: agent.EventTool, Agent: "sub-1", Tool: &agent.ToolEvent{ID: "child-tool", Status: "pending", Title: "CHILD-ONLY tool"}},
		{Type: agent.EventReplay, Agent: "sub-1", Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}},
		{Type: agent.EventText, Agent: "sub-1", Text: "CHILD-ONLY reply"},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "main-tool", Status: "pending"}},
		{Type: agent.EventSubagent, Agent: "sub-1", Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentCompleted, Output: "CHILD-ONLY output"}, SubagentChange: agent.SubagentChangeFinished},
	}
	for i, ev := range evs {
		if got := r.publish(ev); got != uint64(i+1) {
			t.Fatalf("after event %d committed the engine's model is at %d", i+1, got)
		}
	}
	if r.e.isReplaying() {
		t.Fatal("a child's replay bracket made the engine replay")
	}
	r.submit("a turn")
	r.until(lastEnding)
	n := r.quietSeq()
	for len(r.seen) == 0 || r.seen[len(r.seen)-1].Seq < n {
		r.next()
	}
	folded := transcript.New(transcript.Options{})
	for _, ev := range r.seen {
		folded.Fold(ev)
	}
	if got := folded.Seq(); got != n || len(r.seen) != int(n) {
		t.Fatalf("the committed records run to %d (%d of them), the engine's model is at %d", got, len(r.seen), n)
	}
	if d := DiffModels(folded, r.e.model, true); d != "" {
		t.Fatalf("the engine's model is not the committed sequence folded in order: %s", d)
	}
	for _, e := range r.e.model.History().Main.Entries {
		if strings.Contains(e.Text, "CHILD-ONLY") || e.Tool != nil && e.Tool.ID == "child-tool" {
			t.Fatalf("a sub-agent's event reached the main transcript: %s", describeEntry(e))
		}
	}
	child := r.e.model.Sub("sub-1")
	if child == nil || child.Len() != 4 {
		t.Fatalf("the child's transcript holds %v entries, want its prompt, thought, tool and reply", child)
	}
	if st := r.e.model.State(); len(st.Agents) != 1 || st.Agents[0].Status != agent.SubagentCompleted {
		t.Fatalf("the roster is %+v", st.Agents)
	}
}

// ------------------------------------------------------------------- A3

// outputCapTool is a tool report at internal/agent/tools.go's output caps —
// three 8 KiB tails, two 512 B heads, 512 B of raw input and 2 KiB of content:
// C2's worst-case fixture's main row.
func outputCapTool(id string, i int) *agent.ToolEvent {
	c := string(rune('a' + i%26))
	code := i % 3
	return &agent.ToolEvent{
		ID: id, Status: "completed", Kind: "execute", Title: "Run make",
		RawInput: strings.Repeat(c, 512), ContentText: strings.Repeat(c, 2<<10),
		Output: &agent.ToolOutput{ExitCode: &code,
			Stdout: strings.Repeat(c, 8<<10), Stderr: strings.Repeat(c, 8<<10), Content: strings.Repeat(c, 8<<10),
			StdoutHead: strings.Repeat(c, 512), StderrHead: strings.Repeat(c, 512)},
	}
}

// TestAttachOverAWorstCaseSessionFitsTheSubscription (A3, the third limit —
// the subscription's storage): C2's worst case (TestTheModelIsBoundedOnAWorst-
// CaseSession: 5,000 main tool reports at the output caps, an open reply past
// the stream cap, 33 children at their caps, 32 of them finished), committed
// through the engine's log, is attached to with a 1,024-record, 8 MiB
// subscription and no error.
//
// The schedule the promise holds under: nothing is published between the
// snapshot and the client's first Records read (the publisher is paused). Then
// the snapshot's Seq is the cutoff, the replay is empty, and the subscription
// starts with nothing charged against its budget: the snapshot, whatever it
// weighs (at most its own 4 MiB), travels beside the subscription and never
// through it. A burst committed while the client does not read is then held
// whole up to the budget — here 1,000 records — and the client folds it and
// holds the engine's suffix. (A publish racing the attach instead charges the
// replay against MaxBytes, and past it the attach retries with a fresh
// snapshot: TestAttachRetriesWhenTheRingMovedPastTheSnapshot.)
func TestAttachOverAWorstCaseSessionFitsTheSubscription(t *testing.T) {
	r := newRig(t, Options{})
	for i := range 5000 {
		r.s.emit(agent.Event{Type: agent.EventTool, Tool: outputCapTool(fmt.Sprintf("t%04d", i), i)})
	}
	r.s.emit(textOf(100<<10, "m"))
	for c := range 33 {
		id := fmt.Sprintf("sub-%02d", c)
		r.s.emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned})
		for j := range 1100 {
			r.s.emit(agent.Event{Type: agent.EventTool, Agent: id, Tool: &agent.ToolEvent{ID: fmt.Sprintf("c%04d", j), Status: "completed", RawInput: strings.Repeat("r", 1040)}})
		}
		if c < 32 {
			r.s.emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: agent.SubagentCompleted}, SubagentChange: agent.SubagentChangeFinished})
		} else {
			r.s.emit(agent.Event{Type: agent.EventThought, Agent: id, Text: strings.Repeat("s", 100<<10)})
		}
	}
	// The rig's own subscriber is not this test's: it fell behind long ago.
	r.sub.Close()
	cutoff := r.e.model.Seq()

	c, err := NewAttachClient(watchdogCtx(t), r.e, AttachOptions{MaxItems: 1024, MaxBytes: 8 << 20})
	if err != nil {
		t.Fatalf("attaching to the worst case: %v", err)
	}
	t.Cleanup(c.Close)
	s := c.Attachments[0].Snapshot
	b, err := transcript.EncodeSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seq != cutoff || !s.Main.Windowed || len(b) > transcript.DefaultSnapshotBytes {
		t.Fatalf("the snapshot is at %d of %d, windowed %v, %d bytes", s.Seq, cutoff, s.Main.Windowed, len(b))
	}
	for i := range 1000 {
		r.s.emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%04d", 4999-i%10), Status: fmt.Sprint(i)}})
	}
	n := r.e.model.Seq()
	if err := c.FoldThrough(watchdogCtx(t), n); err != nil {
		t.Fatal(err)
	}
	if len(c.Reattached) != 0 || c.Folded[0] != 1000 {
		t.Fatalf("the client re-attached (%q) or folded %d records: the subscription did not hold the burst", c.Reattached, c.Folded[0])
	}
	if d := DiffSuffix(r.e.model, c.Model, true); d != "" {
		t.Fatalf("the attached client is not the engine's suffix: %s", d)
	}
	t.Logf("the worst case: %d events; its snapshot %d bytes (main %d of %d entries, %d children)", cutoff, len(b), len(s.Main.Entries), r.e.model.Main.Len(), len(s.Subs))
}
