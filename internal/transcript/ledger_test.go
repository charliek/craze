package transcript

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The ledger of a windowed snapshot (execution amendment X23): a restored
// model keeps one payload-free placeholder per omitted entry at the head of
// each transcript, counted in its caps, so it trims exactly when the first
// model trims the real rows.

// transcriptsByID is a model's transcripts, "" for the main one.
func transcriptsByID(m *Model) map[string]*Transcript {
	out := map[string]*Transcript{"": m.Main}
	for id, tr := range m.subs {
		out[id] = tr
	}
	return out
}

// assertSuffixOf fails unless r — restored from a windowed snapshot of first,
// or of a model restored from one, and folded on alongside it — is first's
// suffix: in every transcript its entries are the first model's newest N,
// equal and under the same ids, where N is its own count; its placeholders
// and entries are as many as the first model's entries and account the same
// bytes (so the next trim is the same on both); Trimmed and the continuation
// state agree; and the state projection is the first model's, but for Tools,
// which is the first model's restricted to the tools of the suffix (X23: a
// placeholder holds no payload).
func assertSuffixOf(t *testing.T, what string, first, r *Model) {
	t.Helper()
	wh, gh := first.History(), r.History()
	if len(wh.Subs) != len(gh.Subs) {
		t.Fatalf("%s: %d children, the first model has %d", what, len(gh.Subs), len(wh.Subs))
	}
	pairs := []struct {
		id   string
		w, g TranscriptHistory
	}{{"", wh.Main, gh.Main}}
	for i := range wh.Subs {
		if wh.Subs[i].ID != gh.Subs[i].ID {
			t.Fatalf("%s: child %d is %q, the first model's %q", what, i, gh.Subs[i].ID, wh.Subs[i].ID)
		}
		pairs = append(pairs, struct {
			id   string
			w, g TranscriptHistory
		}{wh.Subs[i].ID, wh.Subs[i].TranscriptHistory, gh.Subs[i].TranscriptHistory})
	}
	suffixTools := make(map[ToolKey]bool)
	fts, rts := transcriptsByID(first), transcriptsByID(r)
	for _, p := range pairs {
		n := len(p.g.Entries)
		if n > len(p.w.Entries) {
			t.Fatalf("%s: transcript %q holds %d entries, the first model %d", what, p.id, n, len(p.w.Entries))
		}
		if tail := p.w.Entries[len(p.w.Entries)-n:]; n > 0 && !reflect.DeepEqual(tail, p.g.Entries) {
			for j := range tail {
				if !reflect.DeepEqual(tail[j], p.g.Entries[j]) {
					t.Fatalf("%s: transcript %q is not the first model's suffix at %d of %d:\nwant %+v\n got %+v", what, p.id, j, n, tail[j], p.g.Entries[j])
				}
			}
		}
		if p.g.Trimmed != p.w.Trimmed || p.g.StreamOpen != p.w.StreamOpen || p.g.TodoPlanned != p.w.TodoPlanned || p.g.TodoDone != p.w.TodoDone {
			t.Fatalf("%s: transcript %q: trimmed %v (first %v), stream %v (first %v), todos %d/%v (first %d/%v)", what, p.id,
				p.g.Trimmed, p.w.Trimmed, p.g.StreamOpen, p.w.StreamOpen, p.g.TodoPlanned, p.g.TodoDone, p.w.TodoPlanned, p.w.TodoDone)
		}
		ft, rt := fts[p.id], rts[p.id]
		if rt.count() != ft.count() || rt.bytes != ft.bytes {
			t.Fatalf("%s: transcript %q counts %d (%d placeholders) and %d bytes, the first model %d and %d", what, p.id,
				rt.count(), rt.held(), rt.bytes, ft.count(), ft.bytes)
		}
		if ft.held() == 0 && rt.held() > 0 && n != len(p.w.Entries)-rt.held() {
			t.Fatalf("%s: transcript %q: %d entries and %d placeholders for the first model's %d", what, p.id, n, rt.held(), len(p.w.Entries))
		}
		for _, e := range p.g.Entries {
			if e.Kind == KindTool && e.Tool != nil && e.Tool.ID != "" {
				suffixTools[ToolKey{Agent: p.id, ID: e.Tool.ID}] = true
			}
		}
	}
	ws, gs := first.State(), r.State()
	var want map[ToolKey]*agent.ToolEvent
	for k, v := range ws.Tools {
		if suffixTools[k] {
			if want == nil {
				want = make(map[ToolKey]*agent.ToolEvent)
			}
			want[k] = v
		}
	}
	if !reflect.DeepEqual(want, gs.Tools) {
		t.Fatalf("%s: the tools are %v, the first model's over the suffix %v", what, gs.Tools, want)
	}
	ws.Tools, gs.Tools = nil, nil
	if !reflect.DeepEqual(ws, gs) {
		t.Fatalf("%s: the states differ:\nwant %+v\n got %+v", what, ws, gs)
	}
	if !reflect.DeepEqual(first.EndedAsks(), r.EndedAsks()) {
		t.Fatalf("%s: the last-ended lists differ", what)
	}
	checkInvariants(t, r)
}

// detailRow is a delta's Detail: a note row of about n bytes.
func detailRow(i, n int) agent.Event {
	return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: fmt.Sprintf("row %03d %s", i, strings.Repeat("d", n))}, At: at(i)}
}

// ledgerFixture is the brief's minimal schedule: seq 1 a tool "old", pending;
// seq 2–5 four rows. It returns the first model and its twins restored from a
// snapshot sized to keep the newest two rows, so the ledger is old, row 2 and
// row 3.
func ledgerFixture(t *testing.T, o Options) (*Model, []*Model) {
	t.Helper()
	m := New(o)
	foldAll(t, m, true, sequenced([]agent.Event{
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "old", Status: "pending", RawInput: strings.Repeat("r", 100)}, At: at(1)},
		detailRow(2, 90), detailRow(3, 90), detailRow(4, 90), detailRow(5, 90),
	})...)
	if m.Main.trimmed || m.Main.len() != 5 {
		t.Fatalf("the fixture trimmed already: %d entries", m.Main.len())
	}
	full, _ := snapshotOf(t, m, 1<<40)
	s, b := snapshotOf(t, m, encodedLen(t, windowedTo(full, 2)))
	live := m.Main.live()
	want := []Omitted{{Bytes: live[0].Bytes, Tool: "old"}, {Bytes: live[1].Bytes}, {Bytes: live[2].Bytes}}
	if len(s.Main.Entries) != 2 || !reflect.DeepEqual(s.Main.Omitted, want) {
		t.Fatalf("the window: %d entries, ledger %v, want %v", len(s.Main.Entries), s.Main.Omitted, want)
	}
	r1, r2 := restoredBoth(t, s, b, o)
	rs := []*Model{r1, r2}
	for i, r := range rs {
		assertSuffixOf(t, fmt.Sprintf("restored (twin %d)", i), m, r)
		if r.Main.held() != 3 || r.Main.Len() != 2 {
			t.Fatalf("twin %d holds %d placeholders and %d entries", i, r.Main.held(), r.Main.Len())
		}
	}
	return m, rs
}

// foldSuffix folds ev into the first model and each twin, holds each twin to
// assertSuffixOf, and returns the first model's Change and the twins'.
func foldSuffix(t *testing.T, what string, m *Model, rs []*Model, ev agent.Event) (Change, []Change) {
	t.Helper()
	c := m.Fold(ev)
	checkInvariants(t, m)
	cs := make([]Change, len(rs))
	for i, r := range rs {
		cs[i] = r.Fold(ev)
		assertSuffixOf(t, fmt.Sprintf("%s (twin %d) after seq %d", what, i, ev.Seq), m, r)
	}
	return c, cs
}

// ledgerBounds are the minimal schedule's bounds: by count, the brief's
// MainEntries 6; by bytes, a budget that the fifth row fits and the sixth
// does not, so the first model trims "old" by bytes at seq 6.
func ledgerBounds(t *testing.T) map[string]Options {
	t.Helper()
	probe, _ := ledgerFixture(t, Options{})
	live := probe.Main.live()
	return map[string]Options{
		"by count": {Bounds: Bounds{MainEntries: 6}},
		"by bytes": {Bounds: Bounds{MainBytes: probe.Main.bytes + live[1].Bytes/2}},
	}
}

// TestAWindowedRestoreTrimsItsPlaceholdersWithTheFirstModel (X23, the
// brief's minimal schedule and its byte-cap twin): a snapshot keeps the newest
// two of five rows, so its ledger names the tool "old"; both models fold
// three more rows, and the first trims "old" — by the entry cap, or by bytes
// — and forgets its id; the restored model drops the placeholder at the same
// fold, and forgets it too. So seq 9's update to "old" appends a new tool row
// on both, and the restored model stays the first's suffix throughout. The
// fold reports a placeholder's drop as Change.State (it named a tool) and not
// in Change.Dropped (no reader held it).
func TestAWindowedRestoreTrimsItsPlaceholdersWithTheFirstModel(t *testing.T) {
	for name, o := range ledgerBounds(t) {
		t.Run(name, func(t *testing.T) {
			m, rs := ledgerFixture(t, o)
			for seq := 6; seq <= 8; seq++ {
				_, hadOld := m.Main.tools["old"]
				held := make([]int, len(rs))
				for i, r := range rs {
					held[i] = r.Main.held()
				}
				c, cs := foldSuffix(t, "a row", m, rs, sequencedAt(detailRow(seq, 90), seq))
				for i, r := range rs {
					// A dropped placeholder is not in Dropped: no reader held it.
					if gone := held[i] - r.Main.held(); cs[i].Dropped != c.Dropped-gone {
						t.Fatalf("twin %d at seq %d dropped %d placeholders and reports %+v; the first model %+v", i, seq, gone, cs[i], c)
					}
				}
				if _, has := m.Main.tools["old"]; hadOld && !has {
					// The first model dropped the row of "old" at this fold.
					for i, r := range rs {
						if _, ok := r.Main.ptools["old"]; ok || !cs[i].State || !c.State {
							t.Fatalf("twin %d at seq %d: placeholder kept %v, change %+v; the first model's %+v", i, seq, ok, cs[i], c)
						}
					}
				}
			}
			if _, ok := m.Main.tools["old"]; ok || !m.Main.trimmed {
				t.Fatal("the first model did not trim the row of \"old\"")
			}
			foldSuffix(t, "the update to old", m, rs, sequencedAt(agent.Event{Type: agent.EventTool,
				Tool: &agent.ToolEvent{ID: "old", Status: "completed", ContentText: "the receipt"}, At: at(9)}, 9))
			for i, r := range append([]*Model{m}, rs...) {
				last := r.Main.lastEntry()
				if last.Kind != KindTool || last.Tool.ID != "old" || last.Tool.Status != "completed" {
					t.Fatalf("model %d: the update to a trimmed tool did not append its row: %v", i, facts(r.Main))
				}
			}
		})
	}
}

// sequencedAt is ev with Seq seq.
func sequencedAt(ev agent.Event, seq int) agent.Event {
	ev.Seq = uint64(seq)
	return ev
}

// TestAnUpdateToAPlaceholderToolReaccountsIt (X23): an update to "old" while
// the first model still holds its row — a payload three times the size —
// draws no row on the restored model and updates the first's in place, and
// neither trims (X5 revised); the placeholder is re-accounted to the new
// payload's bytes, so the next rows trim at the same fold on both, and the
// later update after the trim appends on both.
func TestAnUpdateToAPlaceholderToolReaccountsIt(t *testing.T) {
	for name, o := range ledgerBounds(t) {
		t.Run(name, func(t *testing.T) {
			m, rs := ledgerFixture(t, o)
			before := make([]modelView, len(rs))
			for i, r := range rs {
				before[i] = viewOf(r)
			}
			foldSuffix(t, "the update to old", m, rs, sequencedAt(agent.Event{Type: agent.EventTool,
				Tool: &agent.ToolEvent{ID: "old", Status: "running", RawInput: strings.Repeat("r", 300)}, At: at(6)}, 6))
			if m.Main.len() != 5 || m.Main.live()[0].Tool.Status != "running" {
				t.Fatalf("the first model did not update its row in place: %v", facts(m.Main))
			}
			for i, r := range rs {
				after := viewOf(r)
				after.S.Seq = before[i].S.Seq
				if !reflect.DeepEqual(before[i], after) {
					t.Fatalf("twin %d: an update to a placeholder tool changed what the model shows", i)
				}
				if got, want := r.Main.ledger[0].Bytes, m.Main.live()[0].Bytes; got != want {
					t.Fatalf("twin %d: the placeholder accounts %d bytes, the first model's row %d", i, got, want)
				}
			}
			for seq := 7; seq <= 10; seq++ {
				foldSuffix(t, "a row", m, rs, sequencedAt(detailRow(seq, 90), seq))
			}
			if _, ok := m.Main.tools["old"]; ok {
				t.Fatal("the rows did not trim the row of \"old\"")
			}
			foldSuffix(t, "the update after the trim", m, rs, sequencedAt(agent.Event{Type: agent.EventTool,
				Tool: &agent.ToolEvent{ID: "old", Status: "completed"}, At: at(11)}, 11))
			if last := rs[1].Main.lastEntry(); last.Kind != KindTool || last.Tool.ID != "old" {
				t.Fatalf("the update after the trim drew no row: %v", facts(rs[1].Main))
			}
		})
	}
}

// TestAnOmittedRunsPlaceholderFollowsItsTail (X23's one approximation): a
// child the window emptied mid-run keeps the run as its last placeholder,
// whose bytes follow the first model's open entry chunk by chunk — exactly for
// ASCII, at the cap and past it, and within three bytes when multi-byte text
// is cut at the cap, where only the run's bytes can say where the first
// model's tail starts. The run's placeholder is never trimmed while it is
// open, and closing the run keeps it at what it accounts.
func TestAnOmittedRunsPlaceholderFollowsItsTail(t *testing.T) {
	for _, unit := range []string{"ab", "é", "😀", "a😀é"} {
		t.Run(unit, func(t *testing.T) {
			const limit = 32
			o := Options{Bounds: Bounds{StreamText: limit}}
			m := New(o)
			evs := sequenced([]agent.Event{
				{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(1)},
				{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c0", Status: "pending"}, At: at(2)},
				{Type: agent.EventThought, Agent: "sub", Text: unit, At: at(3)},
				{Type: agent.EventText, Text: "main", At: at(4)},
			})
			foldAll(t, m, true, evs...)
			full, _ := snapshotOf(t, m, 1<<40)
			s, b := snapshotOf(t, m, encodedLen(t, windowedTo(full, 1, 0)))
			if s.Subs[0].OmittedRun != KindThought || len(s.Subs[0].Omitted) != 2 {
				t.Fatalf("the window: child run %v, ledger %v", s.Subs[0].OmittedRun, s.Subs[0].Omitted)
			}
			r1, r2 := restoredBoth(t, s, b, o)
			seq := len(evs)
			exact := unit == "ab"
			for i := range 3 * limit {
				seq++
				ev := sequencedAt(agent.Event{Type: agent.EventThought, Agent: "sub", Text: unit, At: at(seq)}, seq)
				m.Fold(ev)
				want := m.Sub("sub").current(m.Sub("sub").lastEntry()).Bytes
				for j, r := range []*Model{r1, r2} {
					r.Fold(ev)
					checkInvariants(t, r)
					tr := r.Sub("sub")
					got := tr.ledger[len(tr.ledger)-1].Bytes
					if d := got - want; exact && d != 0 || d < -3 || d > 3 {
						t.Fatalf("twin %d after %d chunks: the run's placeholder accounts %d bytes, the first model's entry %d", j, i+1, got, want)
					}
					if exact && tr.bytes != m.Sub("sub").bytes {
						t.Fatalf("twin %d: %d bytes, the first model %d", j, tr.bytes, m.Sub("sub").bytes)
					}
				}
			}
			seq++
			closing := sequencedAt(agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c1"}, At: at(seq)}, seq)
			m.Fold(closing)
			for _, r := range []*Model{r1, r2} {
				before := append([]Omitted(nil), r.Sub("sub").ledger...)
				r.Fold(closing)
				checkInvariants(t, r)
				tr := r.Sub("sub")
				if tr.omittedRun != 0 || tr.held() != 2 || tr.ledger[1] != before[1] || tr.Len() != 1 {
					t.Fatalf("closing the omitted run: run %v, %d placeholders, %d entries", tr.omittedRun, tr.held(), tr.Len())
				}
				if exact {
					assertSuffixOf(t, "closed", m, r)
				}
			}
		})
	}
}

// ledgerSession draws the property test's random session, an event at a
// time (next): every kind
// that appends, chunks into either stream of the main transcript or a child,
// new tools and updates to any id ever used — retained, windowed out or
// trimmed alike — asks, todos, errors, children spawned and finished. Every
// chunk is ASCII, so the ledger is exact (X23's approximation needs a
// multi-byte cut at the stream cap).
type ledgerSession struct {
	r        *rand.Rand
	seq      int
	tools    map[string][]string // per transcript ("" main), every id used
	children []string
	running  map[string]bool
}

func (g *ledgerSession) next() agent.Event {
	g.seq++
	r := g.r
	where := ""
	if len(g.children) > 0 && r.Intn(3) == 0 {
		where = g.children[r.Intn(len(g.children))]
	}
	text := func() string { return strings.Repeat(string(rune('a'+r.Intn(26))), 1+r.Intn(24)) }
	ev := agent.Event{Agent: where, At: at(g.seq)}
	switch k := r.Intn(16); {
	case k < 3:
		ev.Type, ev.Text = agent.EventText, text()
	case k < 5:
		ev.Type, ev.Text = agent.EventThought, text()
	case k < 7:
		id := fmt.Sprintf("t%d", g.seq)
		g.tools[where] = append(g.tools[where], id)
		ev.Type, ev.Tool = agent.EventTool, &agent.ToolEvent{ID: id, Status: "pending", RawInput: strings.Repeat("r", r.Intn(60))}
	case k < 10 && len(g.tools[where]) > 0:
		ids := g.tools[where]
		ev.Type, ev.Tool = agent.EventTool, &agent.ToolEvent{ID: ids[r.Intn(len(ids))], Status: fmt.Sprint("s", g.seq), ContentText: strings.Repeat("c", r.Intn(80))}
	case k == 10:
		ev.Type, ev.Text = agent.EventUser, text()
	case k == 11:
		ev.Agent = ""
		ev.Type, ev.State = agent.EventMeta, &agent.StateDelta{Detail: text()}
	case k == 12:
		ev.Agent = ""
		ev.Type, ev.Err = agent.EventError, remoteErr(text())
	case k == 13:
		ev.Agent = ""
		n := 1 + r.Intn(3)
		todos := make([]agent.Todo, n)
		for i := range todos {
			todos[i] = agent.Todo{ID: fmt.Sprint(i), Content: "x", Status: []string{"pending", "completed"}[r.Intn(2)]}
		}
		ev.Type, ev.Todos = agent.EventTodos, todos
	case k == 14 && len(g.children) < 3:
		id := fmt.Sprintf("sub-%d", g.seq)
		g.children = append(g.children, id)
		g.running[id] = true
		ev.Agent = ""
		ev.Type, ev.Subagent, ev.SubagentChange = agent.EventSubagent, &agent.SubagentInfo{ID: id, Status: agent.SubagentRunning}, agent.SubagentChangeSpawned
	case k == 15 && where != "" && g.running[where]:
		g.running[where] = false
		ev.Agent = ""
		ev.Type, ev.Subagent, ev.SubagentChange = agent.EventSubagent, &agent.SubagentInfo{ID: where, Status: agent.SubagentCompleted, EndedAt: at(g.seq)}, agent.SubagentChangeFinished
	default:
		ev.Agent = ""
		ev.Type, ev.Text = agent.EventText, text()
	}
	ev.Seq = uint64(g.seq)
	return ev
}

// windowedSnapshot is m's snapshot at a random budget between its mandatory
// sections and its whole, the budget raised until it is not refused.
func windowedSnapshot(t *testing.T, r *rand.Rand, m *Model) (*Snapshot, []byte) {
	t.Helper()
	_, fb := snapshotOf(t, m, 1<<40)
	budget := len(fb)/4 + r.Intn(len(fb)-len(fb)/4+1)
	for {
		if _, err := m.Snapshot(budget); !errors.Is(err, ErrSnapshotTooLarge) {
			return snapshotOf(t, m, budget)
		}
		budget += (len(fb)-budget)/2 + 1
	}
}

// TestAWindowedRestoreStaysTheFirstModelsSuffix is X23's property: over
// random sessions with small caps, a snapshot windowed at a random cut and a
// random budget restores — in process and through the codec — to a model
// that, folding on beside the first, is at every later seq the first model's
// suffix (assertSuffixOf: its real entries are exactly the first model's
// newest N, its placeholders and entries count and account what the first
// model's entries do, Trimmed agrees — so from the moment the first model has
// trimmed past the window, the restored model says so too — and the state is
// the first's over the suffix). At a random later seq the restored model is
// snapshotted again, windowed, and its restored twin — its ledger carried
// forward — joins them.
func TestAWindowedRestoreStaysTheFirstModelsSuffix(t *testing.T) {
	// The race detector slows every fold and projection several times over,
	// and the package's race runs are for its concurrent paths, not this.
	seeds := 400
	if testing.Short() || raceEnabled {
		seeds = 40
	}
	windowed, placeholdersDropped, rechained := 0, 0, 0
	for seed := range seeds {
		r := rand.New(rand.NewSource(int64(seed)))
		o := Options{Bounds: Bounds{
			MainEntries: 4 + r.Intn(8), MainBytes: 150 + r.Intn(500),
			SubEntries: 2 + r.Intn(5), SubBytes: 80 + r.Intn(200),
			StreamText: 8 + r.Intn(40), Agents: 1 + r.Intn(2),
		}}
		g := &ledgerSession{r: r, tools: make(map[string][]string), running: make(map[string]bool)}
		m := New(o)
		cut := 5 + r.Intn(60)
		for range cut {
			m.Fold(g.next())
		}
		checkInvariants(t, m)
		s, b := windowedSnapshot(t, r, m)
		held := 0
		for _, ts := range transcriptsOf(s) {
			held += len(ts.Omitted)
		}
		if held > 0 {
			windowed++
		}
		r1, r2 := restoredBoth(t, s, b, o)
		rs := []*Model{r1, r2}
		what := fmt.Sprintf("seed %d (cut %d, %+v)", seed, cut, o.Bounds)
		for i, rm := range rs {
			assertSuffixOf(t, fmt.Sprintf("%s: restored (twin %d)", what, i), m, rm)
		}
		rechainAt := cut + 1 + r.Intn(60)
		for range 120 {
			ev := g.next()
			m.Fold(ev)
			for i, rm := range rs {
				rm.Fold(ev)
				assertSuffixOf(t, fmt.Sprintf("%s: twin %d after seq %d", what, i, ev.Seq), m, rm)
			}
			if g.seq == rechainAt {
				s2, b2 := windowedSnapshot(t, r, r1)
				d, err := DecodeSnapshot(b2)
				if err != nil {
					t.Fatal(err)
				}
				r3, r4 := Restore(s2, o), Restore(d, o)
				assertSuffixOf(t, what+": restored again in process", m, r3)
				assertSuffixOf(t, what+": restored again through the codec", m, r4)
				rs = append(rs, r3, r4)
				rechained++
			}
		}
		for _, tr := range transcriptsByID(r1) {
			if tr.held() == 0 && tr.windowed {
				placeholdersDropped++
			}
		}
	}
	t.Logf("%d of %d sessions windowed; %d windowed transcripts trimmed every placeholder; %d restored again", windowed, seeds, placeholdersDropped, rechained)
	if windowed < seeds/2 || placeholdersDropped == 0 || rechained == 0 {
		t.Fatalf("the sessions do not exercise the ledger: %d windowed, %d emptied, %d rechained", windowed, placeholdersDropped, rechained)
	}
}

// TestTheLedgerIsCountedInTheBudget (X23, the codec): on a restored model
// whose own ledger carries 0, 1 and 5,000 records — beside the records its
// window adds — every budget from the refusal edge to the whole encodes to
// exactly the size the window counted, within the budget, each transcript
// being the window rule's (windowOf, the carried records first); and a
// 5,000-record ledger is tens of KB.
func TestTheLedgerIsCountedInTheBudget(t *testing.T) {
	for _, carried := range []int{0, 1, 5000} {
		t.Run(fmt.Sprint(carried), func(t *testing.T) {
			o := Options{Bounds: Bounds{MainEntries: 5000 + 40}}
			m := New(o)
			var evs []agent.Event
			for i := range carried + 40 {
				if i%2 == 0 {
					evs = append(evs, agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("toolu_%05d\"é", i), Status: "completed", RawInput: strings.Repeat("r", i%97)}, At: at(1)})
				} else {
					evs = append(evs, detailRow(i, i%53))
				}
			}
			foldAll(t, m, false, sequenced(evs)...)
			full, _ := snapshotOf(t, m, 1<<40)
			w := windowedTo(full, 40)
			s, b := snapshotOf(t, m, encodedLen(t, w))
			if len(s.Main.Omitted) != carried {
				t.Fatalf("the ledger holds %d records, want %d", len(s.Main.Omitted), carried)
			}
			if carried == 5000 {
				lb, _ := appendTranscript(newJSONWriter(), nil, transcriptScalars{}, &TranscriptSnap{Omitted: s.Main.Omitted})
				t.Logf("a 5,000-record ledger (half tools, ids of 12 bytes) encodes to %d bytes", len(lb))
				if len(lb) > 100<<10 {
					t.Fatalf("a 5,000-record ledger encodes to %d bytes", len(lb))
				}
			}
			d, err := DecodeSnapshot(b)
			if err != nil {
				t.Fatal(err)
			}
			r := Restore(d, o)
			checkInvariants(t, r)
			rfull, rfb := snapshotOf(t, r, 1<<40)
			if !reflect.DeepEqual(rfull.Main.Omitted, s.Main.Omitted) {
				t.Fatal("the restored model's whole snapshot does not carry its ledger")
			}
			// From just under the mandatory sections — every entry a record —
			// to the whole.
			mandatory := encodedLen(t, windowedTo(rfull, 0))
			refused, kept, steps := 0, 0, 150
			if raceEnabled {
				steps = 40
			}
			for budget := mandatory - 64; budget <= len(rfb)+64; budget += max(1, (len(rfb)-mandatory)/steps) {
				s2, size, err := r.snapshotSized(budget)
				if errors.Is(err, ErrSnapshotTooLarge) {
					refused++
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				kept++
				b2, err := EncodeSnapshot(s2)
				if err != nil {
					t.Fatal(err)
				}
				if len(b2) != size || size > budget {
					t.Fatalf("budget %d: the window counted %d, the encoding has %d", budget, size, len(b2))
				}
				assertTheWindowIsTight(t, fmt.Sprintf("carried %d at %d", carried, budget), rfull, s2, budget)
			}
			if refused == 0 || kept == 0 {
				t.Fatalf("the sweep never crossed the refusal edge: %d refused, %d kept", refused, kept)
			}
		})
	}
}
