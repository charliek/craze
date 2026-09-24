package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
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

// attachProbeCompareTimeout bounds how long the probe's comparison waits for
// the attached fold to catch up to the primary's own last folded seq (plan
// 024 §3.6 item 7): generous, since a live smoke session (§8 V1) is the
// flag's whole point, and a timeout writes DIFF with a reason rather than
// hanging the command.
const attachProbeCompareTimeout = 20 * time.Second

// attachProbePoll is how often the comparison looks at the attached fold's
// progress while it waits.
const attachProbePoll = 2 * time.Millisecond

// attachProbe is `craze prompt --attach-probe=PATH` (plan 024 §5 row C4,
// hidden): a second, independent fold of the engine's own model, compared
// against the CLI's own read of the primary at a common seq. It follows
// C3's AttachClient (internal/engine/attach_test.go) — the shape a client
// folds an attachment in — but cannot import it: that file is test code.
//
// first is folded on the primary's own reader (onEvent, called from
// consume — the whole of the hook prompt.go carries), one event at a time, in
// order. The attached fold runs on a goroutine of its own, started on the
// first EventText: Attach blocks inside the log's publishing boundary, and
// the CLI must keep draining the primary while it waits (plan 024 §3.6 item
// 4, SF-14).
type attachProbe struct {
	path   string
	eng    *engine.Engine
	ctx    context.Context
	stderr io.Writer

	// first is "the first client" (plan 024 §5 row C4): folded from seq 1,
	// stamped only from events (Options.Clock nil) and reading an error's
	// text the way the primary carries it — the publisher's own value, not
	// the codec's *agent.RemoteError a decoding client is handed (X14) — so
	// both sides account the same bytes for it.
	first *transcript.Model

	mu         sync.Mutex
	started    bool // the attached goroutine has been started
	done       bool // finish has already run; it runs at most once
	curModel   *transcript.Model
	curSub     *agent.Subscription
	attachErr  error  // sticky: the first fatal error the attached side hit
	windowed   string // sticky: set instead of attachErr for a windowed snapshot
	reattached []string

	stop chan struct{} // closed by finish to end the attached goroutine
	wg   sync.WaitGroup
}

// newAttachProbe builds a probe over eng, or nil when path is empty — the
// flag's own "off" state. Every other method treats a nil *attachProbe as a
// no-op, so the one call site consume carries (onEvent) costs nothing when
// the flag was not given: no fold, no goroutine, no attach.
func newAttachProbe(ctx context.Context, path string, eng *engine.Engine, stderr io.Writer) *attachProbe {
	if path == "" {
		return nil
	}
	return &attachProbe{
		path:   path,
		eng:    eng,
		ctx:    ctx,
		stderr: stderr,
		first: transcript.New(transcript.Options{
			ErrText: func(e error) string { return e.Error() },
		}),
		stop: make(chan struct{}),
	}
}

// onEvent folds ev into the probe's first client, starts the attached fold on
// the first EventText of the first turn, and — once, when ev is the chain's
// own end (nothing claimed and nothing queued behind this turn's ended,
// exactly the condition readChain's own turnEnded treats as nothing left to
// run) — compares both folds and writes the result. p may be nil.
func (p *attachProbe) onEvent(ev agent.Event) {
	if p == nil {
		return
	}
	p.first.Fold(ev)

	p.mu.Lock()
	if !p.started && ev.Type == agent.EventText {
		p.started = true
		p.wg.Add(1)
		go p.attachLoop(p.ctx)
	}
	started := p.started
	p.mu.Unlock()

	if !started {
		return
	}
	if ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnEnded &&
		ev.Turn.Next == "" && ev.Turn.Pending == 0 {
		p.finish()
	}
}

// finish runs at most once, synchronously on the primary's own reader (it is
// called from onEvent, which consume calls from readChain): it waits,
// bounded, for the attached fold to reach the primary's own last folded seq,
// compares both there, writes PATH, and only then closes the attached
// subscription and joins its goroutine — so both are done before readChain
// itself returns.
func (p *attachProbe) finish() {
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		return
	}
	p.done = true
	p.mu.Unlock()

	n := p.first.Seq()
	line, attached, haveAttached := p.waitAndCompare(n)

	close(p.stop)
	p.wg.Wait()

	p.write(line, n, attached, haveAttached)
}

// waitAndCompare polls the attached fold until it reaches seq n — a common
// seq, never "at exit" (plan 024 §3.6 item 7) — bounded by
// attachProbeCompareTimeout and p.ctx. It returns the result line (SAME,
// "DIFF: reason" or "ERROR: reason") and the attached side's view, when one
// was reached.
func (p *attachProbe) waitAndCompare(n uint64) (string, probeView, bool) {
	deadline := time.Now().Add(attachProbeCompareTimeout)
	tick := time.NewTicker(attachProbePoll)
	defer tick.Stop()
	for {
		p.mu.Lock()
		model, attachErr, windowed := p.curModel, p.attachErr, p.windowed
		p.mu.Unlock()

		switch {
		case windowed != "":
			return "DIFF: " + windowed, probeView{}, false
		case attachErr != nil:
			return "ERROR: " + attachErr.Error(), probeView{}, false
		case model != nil && model.Seq() >= n:
			got := viewOf(model)
			if d := diffViews(viewOf(p.first), got); d != "" {
				return "DIFF: " + d, got, true
			}
			return "SAME", got, true
		}
		if !time.Now().Before(deadline) {
			return p.stalled(model, fmt.Sprintf("timed out after %s waiting for the attached fold to reach seq %d", attachProbeCompareTimeout, n))
		}
		select {
		case <-tick.C:
		case <-p.ctx.Done():
			return p.stalled(model, fmt.Sprintf("the run's own context ended while waiting for the attached fold to reach seq %d: %v", n, p.ctx.Err()))
		}
	}
}

// stalled is waitAndCompare's answer when it gives up before the attached
// fold reached its target: DIFF, with the reason and, when the attached side
// ever produced a model at all, its seq and its view for the dump.
func (p *attachProbe) stalled(model *transcript.Model, reason string) (string, probeView, bool) {
	if model == nil {
		return "DIFF: " + reason + " (no attached fold was ever produced)", probeView{}, false
	}
	return fmt.Sprintf("DIFF: %s (at %d)", reason, model.Seq()), viewOf(model), true
}

// attachLoop is the attached fold's own goroutine: never the primary's own
// reader (plan 024 §3.6 item 4). It attaches with no cursor, folds every
// record its subscription delivers, and re-attaches — with no cursor, bounded
// by attachProbeReattachBound — on an Omitted record or a subscription that
// closed with an error, exactly as C3's AttachClient does.
func (p *attachProbe) attachLoop(ctx context.Context) {
	defer p.wg.Done()

	model, sub, windowed, err := p.attachOnce(ctx)
	switch {
	case err != nil:
		p.setErr(fmt.Errorf("attach: %w", err))
		return
	case windowed != "":
		p.setWindowed(windowed)
		return
	}
	p.setCur(model, sub)

	reattaches := 0
	giveUp := func(reason string) bool {
		reattaches++
		if reattaches > attachProbeReattachBound {
			p.setErr(fmt.Errorf("gave up after %d re-attaches (last: %s)", reattaches-1, reason))
			return true
		}
		p.noteReattach(reason)
		return false
	}

	for {
		select {
		case rec, ok := <-sub.Records():
			if !ok {
				subErr := sub.Err()
				if errors.Is(subErr, agent.ErrClosed) {
					return
				}
				if giveUp(subErr.Error()) {
					return
				}
			} else if rec.Omitted != nil {
				sub.Close()
				if giveUp(fmt.Sprintf("omitted %d", rec.Seq)) {
					return
				}
			} else {
				ev, evErr := rec.Event()
				if evErr != nil {
					sub.Close()
					p.setErr(fmt.Errorf("decode seq %d: %w", rec.Seq, evErr))
					return
				}
				model.Fold(ev)
				continue
			}
			model, sub, windowed, err = p.attachOnce(ctx)
			switch {
			case err != nil:
				p.setErr(fmt.Errorf("re-attach: %w", err))
				return
			case windowed != "":
				p.setWindowed(windowed)
				return
			}
			p.setCur(model, sub)
		case <-p.stop:
			sub.Close()
			return
		case <-ctx.Done():
			sub.Close()
			return
		}
	}
}

// attachOnce attaches with no cursor and Restores the snapshot it is handed.
// AttachOptions{} always asks for a snapshot (no cursor to honour), so unlike
// C3's AttachClient — whose adopt keeps the model when a resumed cursor is
// honoured — this never needs that branch: every attach the probe makes,
// first or re-attach alike, is with no cursor (plan 024 §3.6 items 3 and 6).
//
// A snapshot that comes back windowed is reported, not folded: a probe
// attaching to a short live session is never windowed (plan 024 brief C4;
// execution amendment X23's windowing is for a session whose backlog since
// the cut outgrew a snapshot's budget, not a fresh attach's own cut).
func (p *attachProbe) attachOnce(ctx context.Context) (model *transcript.Model, sub *agent.Subscription, windowed string, err error) {
	a, err := p.eng.Attach(ctx, engine.AttachOptions{})
	if err != nil {
		return nil, nil, "", err
	}
	if a.Snapshot == nil {
		if a.Sub != nil {
			a.Sub.Close()
		}
		return nil, nil, "", errors.New("attach answered with neither a snapshot nor an honoured cursor")
	}
	if a.Snapshot.Main.Windowed {
		a.Sub.Close()
		return nil, nil, fmt.Sprintf("the main transcript's snapshot at seq %d came back windowed", a.Snapshot.Seq), nil
	}
	for _, s := range a.Snapshot.Subs {
		if s.Windowed {
			a.Sub.Close()
			return nil, nil, fmt.Sprintf("child %s's snapshot at seq %d came back windowed", s.ID, a.Snapshot.Seq), nil
		}
	}
	return transcript.Restore(a.Snapshot, transcript.Options{}), a.Sub, "", nil
}

func (p *attachProbe) setCur(model *transcript.Model, sub *agent.Subscription) {
	p.mu.Lock()
	p.curModel, p.curSub = model, sub
	p.mu.Unlock()
}

func (p *attachProbe) setErr(err error) {
	p.mu.Lock()
	if p.attachErr == nil {
		p.attachErr = err
	}
	p.mu.Unlock()
}

func (p *attachProbe) setWindowed(reason string) {
	p.mu.Lock()
	if p.windowed == "" {
		p.windowed = reason
	}
	p.mu.Unlock()
}

func (p *attachProbe) noteReattach(reason string) {
	p.mu.Lock()
	p.reattached = append(p.reattached, reason)
	p.mu.Unlock()
}

func (p *attachProbe) reattachReasons() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reattached...)
}

// write is finish's last step: PATH's first line is line (SAME, "DIFF: …" or
// "ERROR: …"), then both projections as a readable JSON dump so a DIFF can be
// diagnosed, then the V6 measurement line: the first client's model size
// (plan 024 §5 row C4).
//
// It never writes to stdout or stderr except here, on a failure to write
// PATH itself — an IO error — and only because the flag was given (the hard
// stop: with the flag absent the command's behaviour is exactly today's).
func (p *attachProbe) write(line string, n uint64, attached probeView, haveAttached bool) {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, line)
	fmt.Fprintf(&buf, "--- first, at seq %d ---\n", n)
	writeProbeJSON(&buf, dumpOf(viewOf(p.first)))
	buf.WriteString("--- attached ---\n")
	if haveAttached {
		writeProbeJSON(&buf, dumpOf(attached))
	} else {
		buf.WriteString("(none: the attached fold never produced one)\n")
	}
	if reasons := p.reattachReasons(); len(reasons) > 0 {
		fmt.Fprintf(&buf, "reattached: %s\n", strings.Join(reasons, "; "))
	}
	buf.WriteString(p.retainedLine())
	buf.WriteString("\n")

	if err := os.WriteFile(p.path, buf.Bytes(), 0o600); err != nil {
		fmt.Fprintf(p.stderr, "craze: --attach-probe: writing %s: %v\n", p.path, err)
	}
}

// retainedLine is the plan's V6 measurement: the first client's model size
// (Transcript.Len, Transcript.Bytes), e.g.
// "retained: main 412 entries 1843022 bytes; subs 3 / 57 entries / 90211 bytes".
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
