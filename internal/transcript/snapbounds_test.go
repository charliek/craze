package transcript

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

// The four limits of plan 024 §3.5, each its own test here but the third:
// retained model memory (TestTheModelIsBoundedOnAWorstCaseSession), the
// snapshot's encoded size (TestSnapshotStaysInsideItsByteBudget,
// TestMandatoryStateOverTheBudgetIsRefused, and every snapshotOf call), the
// subscription's storage (the event log's, C3's attach test), and the decoding
// peak (TestDecodingASnapshotPeaksNearItsSize).

// fourMiBFixture is budgetFixture padded so its unwindowed snapshot encodes to
// exactly 4 MiB, with that encoding.
func fourMiBFixture(t *testing.T) (*Model, *Snapshot, []byte) {
	t.Helper()
	const size = 4 << 20
	full0, _ := snapshotOf(t, budgetFixture(t, 0), 1<<40)
	extra := size - encodedLen(t, full0)
	if extra < 0 {
		t.Fatalf("the fixture encodes to %d without a pad, past %d", size-extra, size)
	}
	m := budgetFixture(t, extra)
	full, b := snapshotOf(t, m, 1<<40)
	if len(b) != size {
		t.Fatalf("the padded fixture encodes to %d, want exactly %d", len(b), size)
	}
	return m, full, b
}

// budgetFixture is a session whose unwindowed snapshot encodes to 4 MiB plus
// extra bytes minus a constant: the main transcript's tools and open reply,
// then two children — the second running, its oldest entry a 20 KiB reply —
// and a pad row whose length moves the size byte for byte.
func budgetFixture(t *testing.T, extra int) *Model {
	t.Helper()
	diff := func(n int) []agent.ToolDiff {
		return []agent.ToolDiff{{Path: "/w/a.go", NewText: strings.Repeat("+", n)}}
	}
	evs := []agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{Title: strp("budget"), Mode: strp("agent")}, At: at(0)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go"}, At: at(1)},
		{Type: agent.EventMeta, State: &agent.StateDelta{Detail: strings.Repeat("p", 1000+extra)}, At: at(1)},
	}
	for i := range 30 {
		evs = append(evs, agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%02d", i), Status: "completed", Diffs: diff(64 << 10)}, At: at(2)})
	}
	for c, child := range []string{"sub-1", "sub-2"} {
		evs = append(evs,
			agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: child, Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(3)},
			agent.Event{Type: agent.EventText, Agent: child, Text: strings.Repeat("r", 20<<10), At: at(4)},
		)
		for i := range 6 {
			evs = append(evs, agent.Event{Type: agent.EventTool, Agent: child, Tool: &agent.ToolEvent{ID: fmt.Sprintf("c%d-%d", c, i), Diffs: diff(32 << 10)}, At: at(5)})
		}
	}
	evs = append(evs,
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentCompleted, EndedAt: at(6)}, SubagentChange: agent.SubagentChangeFinished, At: at(6)},
		agent.Event{Type: agent.EventThought, Agent: "sub-2", Text: strings.Repeat("s", 70<<10), At: at(7)},
		agent.Event{Type: agent.EventText, Text: strings.Repeat("m", 100<<10), At: at(8)},
	)
	m := New(Options{})
	foldAll(t, m, false, sequenced(evs)...)
	return m
}

// encodedLen is the length of s's encoding.
func encodedLen(t *testing.T, s *Snapshot) int {
	t.Helper()
	b, err := EncodeSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// windowedTo is full with each transcript's newest ks[i] entries.
func windowedTo(full *Snapshot, ks ...int) *Snapshot {
	s := *full
	s.Subs = slices.Clone(full.Subs)
	for i, ts := range transcriptsOf(&s) {
		*ts = windowOf(*ts, ks[i])
	}
	return &s
}

// TestSnapshotStaysInsideItsByteBudget (A3): a session whose snapshot encodes
// to exactly 4 MiB is carried whole at a 4 MiB budget; one byte short, it is
// windowed — the last child filled gives up its oldest entry, and the
// encoding is within the smaller budget. At the other end, a budget one byte
// short of the mandatory sections is refused, and so is one byte short of the
// mandatory sections with the main transcript's newest entry; at that exact
// size the snapshot is that entry and nothing else.
func TestSnapshotStaysInsideItsByteBudget(t *testing.T) {
	const budget = 4 << 20
	m, full, fb := fourMiBFixture(t)
	t.Logf("the fixture: %d bytes of encoding, main %d entries, children %d and %d", len(fb), len(full.Main.Entries), len(full.Subs[0].Entries), len(full.Subs[1].Entries))

	s, b := snapshotOf(t, m, budget)
	for i, ts := range transcriptsOf(s) {
		if ts.Windowed || len(ts.Entries) != len(transcriptsOf(full)[i].Entries) {
			t.Fatalf("at the exact budget transcript %d is windowed (%d entries)", i, len(ts.Entries))
		}
	}
	if len(b) != budget {
		t.Fatalf("at the exact budget the encoding is %d bytes, want %d", len(b), budget)
	}

	s, b = snapshotOf(t, m, budget-1)
	if len(b) > budget-1 {
		t.Fatalf("one byte short the encoding is %d bytes", len(b))
	}
	if s.Main.Windowed || s.Subs[0].Windowed || !s.Subs[1].Windowed || s.Subs[1].Dropped != 1 {
		t.Fatalf("one byte short: main windowed %v, sub-1 %v, sub-2 %v dropping %d; want sub-2's oldest entry alone dropped",
			s.Main.Windowed, s.Subs[0].Windowed, s.Subs[1].Windowed, s.Subs[1].Dropped)
	}
	assertTheWindowIsTight(t, "one byte short", full, s, budget-1)

	// The refusal edges.
	mandatory := encodedLen(t, windowedTo(full, 0, 0, 0))
	withNewest := encodedLen(t, windowedTo(full, 1, 0, 0))
	if _, err := m.Snapshot(mandatory - 1); !errors.Is(err, ErrSnapshotTooLarge) || !strings.Contains(err.Error(), "mandatory") {
		t.Fatalf("one byte short of the mandatory sections (%d): %v", mandatory, err)
	}
	if _, err := m.Snapshot(withNewest - 1); !errors.Is(err, ErrSnapshotTooLarge) || !strings.Contains(err.Error(), "newest") {
		t.Fatalf("one byte short of the newest main entry (%d): %v", withNewest, err)
	}
	s, b = snapshotOf(t, m, withNewest)
	if len(b) != withNewest || len(s.Main.Entries) != 1 || len(s.Subs[0].Entries)+len(s.Subs[1].Entries) != 0 {
		t.Fatalf("at the newest entry's exact size: %d bytes, main %d entries", len(b), len(s.Main.Entries))
	}
	t.Logf("mandatory sections %d bytes; with the newest main entry %d", mandatory, withNewest)
}

// planModel is a turn that proposed a plan whose body is n bytes of text (and
// an overview half as long), Auto or waiting on the user.
func planModel(t *testing.T, body string, auto bool) (*Model, *agent.PlanEvent) {
	t.Helper()
	p := &agent.PlanEvent{ID: "plan-1", Name: "Big", Overview: body[:len(body)/2], Plan: body, Auto: auto}
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "plan it"}, At: at(1)},
		{Type: agent.EventPlan, Plan: p, At: at(2)},
	})...)
	return m, p
}

// TestMandatoryStateOverTheBudgetIsRefused (A3): an open ask's body is
// carried whole up to ItemCap and as its head past it, marked Truncated —
// its plan entry, which is transcript, stays whole — and what is left over the
// budget is refused, never dropped: a 5 MiB open plan at a 4 MiB budget is
// ErrSnapshotTooLarge, because the plan's own entry is the main transcript's
// newest; the same plan answered headless (Auto, no entry) is carried as its
// head; open asks that alone outgrow the budget are refused as mandatory
// state. A roster row, a question and a permission are capped the same way,
// and a head is cut back to a rune boundary.
func TestMandatoryStateOverTheBudgetIsRefused(t *testing.T) {
	const budget = 4 << 20
	t.Run("a 5 MiB open plan", func(t *testing.T) {
		m, _ := planModel(t, strings.Repeat("p", 5<<20), false)
		if _, err := m.Snapshot(budget); !errors.Is(err, ErrSnapshotTooLarge) || !strings.Contains(err.Error(), "newest") {
			t.Fatalf("a 5 MiB plan entry at a 4 MiB budget: %v", err)
		}
		// With room for the entry (5 MiB of plan, 2.5 MiB of overview), the
		// ask beside it is still its head.
		s, _ := snapshotOf(t, m, 16<<20)
		if a := s.Asks[0]; !a.Truncated || len(a.Body.Plan.Plan) != ItemCap || len(a.Body.Plan.Overview) != ItemCap {
			t.Fatalf("the ask's body is its head: %d/%d bytes, truncated %v", len(a.Body.Plan.Plan), len(a.Body.Plan.Overview), a.Truncated)
		}
		if e := s.Main.Entries[len(s.Main.Entries)-1]; e.Kind != KindPlan || len(e.Plan.Plan) != 5<<20 {
			t.Fatalf("the plan entry is carried whole: %v, %d bytes", e.Kind, len(e.Plan.Plan))
		}
		auto, _ := planModel(t, strings.Repeat("p", 5<<20), true)
		s, _ = snapshotOf(t, auto, budget)
		if a := s.Asks[0]; !a.Truncated || len(a.Body.Plan.Plan) != ItemCap || len(s.Main.Entries) != 1 {
			t.Fatalf("an Auto plan is an ask alone, carried as its head: %d bytes, truncated %v, %d entries", len(a.Body.Plan.Plan), a.Truncated, len(s.Main.Entries))
		}
	})

	t.Run("a 200 KiB plan is carried whole", func(t *testing.T) {
		m, p := planModel(t, strings.Repeat("p", 200<<10), false)
		s, b := snapshotOf(t, m, budget)
		if a := s.Asks[0]; a.Truncated || a.Body.Plan != p {
			t.Fatalf("a plan under the cap is the event's own, whole: truncated %v", a.Truncated)
		}
		for _, r := range func() []*Model { x, y := restoredBoth(t, s, b, Options{}); return []*Model{x, y} }() {
			assertSameModel(t, "a whole plan restored", m, r)
		}
	})

	t.Run("a 300 KiB plan is truncated to its head", func(t *testing.T) {
		body := strings.Repeat("p", 300<<10)
		m, p := planModel(t, body, false)
		s, b := snapshotOf(t, m, budget)
		a := s.Asks[0]
		if !a.Truncated || a.Body.Plan.Plan != body[:ItemCap] || a.Body.Plan.Overview != body[:150<<10] {
			t.Fatalf("the ask carries the plan's 256 KiB head: %d bytes, truncated %v", len(a.Body.Plan.Plan), a.Truncated)
		}
		if p.Plan != body || s.Main.Entries[len(s.Main.Entries)-1].Plan != p {
			t.Fatal("the event's payload was written, or the plan entry does not carry it whole")
		}
		r1, r2 := restoredBoth(t, s, b, Options{})
		for _, r := range []*Model{r1, r2} {
			st := r.State()
			if !st.Asks[0].Truncated || len(st.Asks[0].Body.Plan.Plan) != ItemCap {
				t.Fatalf("a restored client cannot tell the ask was truncated: %+v", st.Asks[0])
			}
			if h := r.History(); len(h.Main.Entries[len(h.Main.Entries)-1].Plan.Plan) != 300<<10 {
				t.Fatal("the restored plan entry is not whole")
			}
		}
	})

	t.Run("a head is cut back to a rune boundary", func(t *testing.T) {
		body := "a" + strings.Repeat("😀", 100<<10)
		m, _ := planModel(t, body, true)
		s, _ := snapshotOf(t, m, budget)
		head := s.Asks[0].Body.Plan.Plan
		if !strings.HasPrefix(body, head) || !utf8.ValidString(head) || len(head) > ItemCap || len(head) < ItemCap-3 {
			t.Fatalf("a head of %d bytes: valid %v", len(head), utf8.ValidString(head))
		}
	})

	t.Run("roster rows, questions and permissions", func(t *testing.T) {
		big := strings.Repeat("x", ItemCap+10)
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub", Status: agent.SubagentRunning, Prompt: big, Output: "short"}, SubagentChange: agent.SubagentChangeSpawned},
			{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "fine", Status: agent.SubagentRunning, Prompt: "p"}, SubagentChange: agent.SubagentChangeSpawned},
			{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "q", Questions: []agent.Question{
				{ID: "q1", Prompt: "short", Options: []agent.Option{{ID: "o", Label: big, Description: big}}},
				{ID: "q2", Prompt: big},
			}}},
			{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm", Tool: big}},
		})...)
		s, _ := snapshotOf(t, m, budget)
		if r := s.Agents[0]; !r.Truncated || len(r.Info.Prompt) != ItemCap || r.Info.Output != "short" || s.Agents[1].Truncated {
			t.Fatalf("roster rows: %+v / %+v", r.Truncated, s.Agents[1].Truncated)
		}
		q := s.Asks[0].Body.Question
		if !s.Asks[0].Truncated || len(q.Questions[0].Options[0].Label) != ItemCap || len(q.Questions[0].Options[0].Description) != ItemCap ||
			q.Questions[0].Prompt != "short" || len(q.Questions[1].Prompt) != ItemCap {
			t.Fatalf("the question's prompts and options are capped: %+v", q)
		}
		if p := s.Asks[1].Body.Permission; !s.Asks[1].Truncated || len(p.Tool) != ItemCap {
			t.Fatal("the permission's tool text is capped")
		}
		if st := m.State(); len(st.Agents[0].Prompt) != len(big) || len(st.Asks[1].Body.Permission.Tool) != len(big) {
			t.Fatal("capping wrote the model's own payloads")
		}
	})

	t.Run("open asks alone over the budget", func(t *testing.T) {
		m := New(Options{})
		var evs []agent.Event
		for i := range 20 {
			evs = append(evs, agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: fmt.Sprintf("ask-%d", i),
				Questions: []agent.Question{{ID: "q", Prompt: strings.Repeat("?", 300<<10)}}}})
		}
		foldAll(t, m, true, sequenced(evs)...)
		if _, err := m.Snapshot(budget); !errors.Is(err, ErrSnapshotTooLarge) || !strings.Contains(err.Error(), "mandatory") {
			t.Fatalf("20 capped asks (5 MiB) at a 4 MiB budget: %v", err)
		}
		snapshotOf(t, m, 6<<20)
	})

	// TestMandatoryStateOverTheBudgetIsRefused's turn case (r3 finding 3): the
	// running turn's own Text is mandatory state, exactly the way an open
	// ask's body is. A 5 MiB prompt no longer blocks every snapshot the way an
	// uncapped Text did (mandatory alone, over budget, whatever else the
	// session holds): the mandatory turn is its head, capped and Truncated,
	// same as the ask above. What is left uncapped is the user row addUser
	// wrote to the main transcript at TurnStarted — a transcript entry like
	// any other, so it follows the window's own rule: carried whole when it
	// fits, and if it is the newest entry and does not, ErrSnapshotTooLarge
	// names it, not the (now-capped) mandatory turn.
	t.Run("a 5 MiB prompt: the turn's own text is capped, its user row follows the window", func(t *testing.T) {
		body := strings.Repeat("p", 5<<20)
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: body}, At: at(1)},
		})...)
		if _, err := m.Snapshot(budget); !errors.Is(err, ErrSnapshotTooLarge) || !strings.Contains(err.Error(), "newest") {
			t.Fatalf("the mandatory turn is capped, but its 5 MiB user row is the main transcript's newest entry and does not fit beside it in a 4 MiB budget: %v", err)
		}
		// With room for the row, the mandatory turn is still only its head:
		// the row alone is required to carry the prompt whole.
		s, b := snapshotOf(t, m, 16<<20)
		if !s.Turn.Truncated || len(s.Turn.Text) != ItemCap || !strings.HasPrefix(body, s.Turn.Text) {
			t.Fatalf("the turn's text is its head: %d bytes, truncated %v", len(s.Turn.Text), s.Turn.Truncated)
		}
		if e := s.Main.Entries[len(s.Main.Entries)-1]; e.Kind != KindUser || e.Text != body {
			t.Fatalf("the user row is carried whole: %v, %d bytes", e.Kind, len(e.Text))
		}
		for _, r := range func() []*Model { x, y := restoredBoth(t, s, b, Options{}); return []*Model{x, y} }() {
			st := r.State()
			if !st.Turn.Truncated || len(st.Turn.Text) != ItemCap {
				t.Fatalf("a restored client cannot tell the turn was truncated: %+v", st.Turn)
			}
			if h := r.History(); h.Main.Entries[len(h.Main.Entries)-1].Text != body {
				t.Fatal("the restored user row is not whole")
			}
			r.Fold(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnStarted, Text: "next"}, At: at(2), Seq: 2})
			if st := r.State(); st.Turn.Truncated {
				t.Fatalf("a new turn started replaces Text whole: %+v", st.Turn)
			}
		}
	})
}

// outputCapTool is a tool report at internal/agent/tools.go's output caps —
// three 8 KiB tails, two 512 B heads, 512 B of raw input and 2 KiB of content
// — every string an allocation of its own, as the events' payloads are.
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

// TestTheModelIsBoundedOnAWorstCaseSession (A3, the first limit): 5,000 main
// tool reports at the output caps, an open reply past the stream cap, and 33
// children — 32 finished (the roster's cap) and one running mid-thought — each
// sent 1,100 tool reports of 1 KiB, so each is held at both its entry cap and
// its byte budget. The model's accounting holds every transcript to its
// bounds, the whole to 8 MiB + 33 × 1 MiB, and the heap it really retains is
// within 2× of what it accounts.
func TestTheModelIsBoundedOnAWorstCaseSession(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	m := New(Options{})
	var seq uint64
	fold := func(ev agent.Event) {
		seq++
		ev.Seq = seq
		m.Fold(ev)
	}
	for i := range 5000 {
		fold(agent.Event{Type: agent.EventTool, Tool: outputCapTool(fmt.Sprintf("t%04d", i), i), At: at(1)})
	}
	fold(agent.Event{Type: agent.EventText, Text: strings.Repeat("m", 100<<10), At: at(2)})
	for c := range 33 {
		id := fmt.Sprintf("sub-%02d", c)
		fold(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(3)})
		for j := range 1100 {
			fold(agent.Event{Type: agent.EventTool, Agent: id, Tool: &agent.ToolEvent{ID: fmt.Sprintf("c%04d", j), Status: "completed", RawInput: strings.Repeat("r", 1040)}, At: at(4)})
		}
		if c < 32 {
			fold(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: agent.SubagentCompleted, EndedAt: at(5 + c)}, SubagentChange: agent.SubagentChangeFinished, At: at(5 + c)})
		} else {
			fold(agent.Event{Type: agent.EventThought, Agent: id, Text: strings.Repeat("s", 100<<10), At: at(40)})
		}
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	checkInvariants(t, m)
	b := DefaultBounds()
	accounted := m.Main.bytes
	if m.Main.bytes > b.MainBytes || m.Main.len() > b.MainEntries || !m.Main.trimmed {
		t.Fatalf("the main transcript holds %d entries, %d bytes", m.Main.len(), m.Main.bytes)
	}
	if len(m.subOrder) != 33 {
		t.Fatalf("%d children, want 33 (32 finished, 1 running)", len(m.subOrder))
	}
	for _, id := range m.subOrder {
		// At its caps: within them, and one of them binding — the entry cap,
		// or the byte budget to within one report (the running child's 64 KiB
		// open tail takes the room of ~60 of its reports).
		tr := m.subs[id]
		if tr.bytes > b.SubBytes || tr.len() > b.SubEntries || (tr.len() < b.SubEntries && tr.bytes < b.SubBytes-1100) {
			t.Fatalf("child %s holds %d entries, %d bytes: not at its caps", id, tr.len(), tr.bytes)
		}
		accounted += tr.bytes
	}
	// The plan's A3 says 8 MiB + 32 × 1 MiB; the 33rd, running child is a
	// transcript of its own under its own 1 MiB (running rows are never
	// evicted), so the bound for this fixture is 8 + 33 MiB.
	if bound := b.MainBytes + 33*b.SubBytes; accounted > bound {
		t.Fatalf("the model accounts %d bytes, over %d", accounted, bound)
	}
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	ratio := float64(heap) / float64(accounted)
	t.Logf("accounted %d bytes (main %d in %d entries, children %d); heap delta %d bytes (%.2f× the accounting)",
		accounted, m.Main.bytes, m.Main.len(), accounted-m.Main.bytes, heap, ratio)
	if ratio < 0.5 || ratio > 2 {
		t.Fatalf("the heap retains %d bytes for %d accounted: %.2f×, outside 2×", heap, accounted, ratio)
	}
	runtime.KeepAlive(m)

	// The same session snapshots inside the default budget.
	s, bb := snapshotOf(t, m, 0)
	t.Logf("its default snapshot: %d bytes, main %d of %d entries", len(bb), len(s.Main.Entries), m.Main.len())
	// The ledger's share of it (X23): every omitted entry's record.
	records, ledger := 0, 0
	for _, ts := range transcriptsOf(s) {
		records += len(ts.Omitted)
		if len(ts.Omitted) > 0 {
			lb, err := appendTranscript(newJSONWriter(), nil, transcriptScalars{}, &TranscriptSnap{Omitted: ts.Omitted})
			if err != nil {
				t.Fatal(err)
			}
			ledger += len(lb) - len(`{}`)
		}
	}
	t.Logf("its ledger: %d records in %d bytes of the snapshot (main %d records)", records, ledger, len(s.Main.Omitted))
	if ledger > len(bb)/4 {
		t.Fatalf("the ledger takes %d of the snapshot's %d bytes", ledger, len(bb))
	}

	// When the running child finishes too, the roster holds 33 finished rows,
	// one past its cap: the oldest finish goes with its transcript, and the
	// plan's own bound, 8 MiB + 32 × 1 MiB, holds.
	fold(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-32", Status: agent.SubagentCompleted, EndedAt: at(60)}, SubagentChange: agent.SubagentChangeFinished, At: at(60)})
	checkInvariants(t, m)
	accounted = m.Main.bytes
	for _, id := range m.subOrder {
		accounted += m.subs[id].bytes
	}
	if len(m.subOrder) != 32 || m.subs["sub-00"] != nil || accounted > b.MainBytes+32*b.SubBytes {
		t.Fatalf("after the 33rd finish: %d children (sub-00 kept %v), %d bytes accounted", len(m.subOrder), m.subs["sub-00"] != nil, accounted)
	}
	t.Logf("with every child finished: %d children, %d bytes accounted", len(m.subOrder), accounted)
}

// lockFixture is the lock test's worst case (§3.4): 5,000 main entries and 33
// running children of 1,000 entries each, every one of the 34 transcripts in
// the middle of a run past the stream cap, so a cut copies 38,000 pointers and
// 34 full tails.
func lockFixture(t *testing.T) *Model {
	t.Helper()
	m := New(Options{})
	var seq uint64
	fold := func(ev agent.Event) {
		seq++
		ev.Seq = seq
		m.Fold(ev)
	}
	for i := range 5000 {
		fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%04d", i), Status: "completed"}, At: at(1)})
	}
	for c := range 33 {
		id := fmt.Sprintf("sub-%02d", c)
		fold(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned, At: at(2)})
		for j := range 1000 {
			fold(agent.Event{Type: agent.EventTool, Agent: id, Tool: &agent.ToolEvent{ID: fmt.Sprintf("c%04d", j), Status: "completed"}, At: at(3)})
		}
		fold(agent.Event{Type: agent.EventThought, Agent: id, Text: strings.Repeat("s", 100<<10), At: at(4)})
	}
	fold(agent.Event{Type: agent.EventThought, Text: strings.Repeat("m", 100<<10), At: at(5)})
	if m.Main.len() != 5000 || len(m.subOrder) != 33 {
		t.Fatalf("the fixture holds %d main entries and %d children", m.Main.len(), len(m.subOrder))
	}
	for _, id := range m.subOrder {
		if tr := m.subs[id]; tr.len() != 1000 || !tr.streamOpen || !tr.cutRun() {
			t.Fatalf("child %s: %d entries, open %v", id, tr.len(), tr.streamOpen)
		}
	}
	return m
}

// lockHoldBound is what TestSnapshotHoldsTheModelLockBriefly asserts of the
// fastest of its cuts. The plan's bound is 1 ms at this worst case; the
// assertion is 5× that without the race detector, whose instrumentation of
// every copy slows the cut several times over, and 25× with it: a CI runner
// shared with the rest of the gate's packages (they run concurrently) can be
// slowed by its neighbours well past a quiet machine's number, and the test is
// here to catch a cut that grew a scan or a copy of bytes — an order of
// magnitude — not to benchmark it. The measured numbers are logged for the PR.
func lockHoldBound() time.Duration {
	if raceEnabled {
		return 25 * time.Millisecond
	}
	return 5 * time.Millisecond
}

// TestSnapshotHoldsTheModelLockBriefly (plan 024 §3.4): on the worst case a
// fold can wait behind — 38,000 entries and 34 open tails — a snapshot holds
// the model's lock for the cut alone. The critical section is timed from
// inside (Model.lockHeld, from just after the lock is taken to just before it
// is released) over whole Snapshot calls and bare cuts, and the fastest is
// held to lockHoldBound: the fastest, because a single sample can be
// preempted for longer than the whole bound.
func TestSnapshotHoldsTheModelLockBriefly(t *testing.T) {
	m := lockFixture(t)
	var held []time.Duration
	m.lockHeld = func(d time.Duration) { held = append(held, d) }
	for range 5 {
		if _, err := m.Snapshot(0); err != nil {
			t.Fatal(err)
		}
	}
	for range 25 {
		_ = m.snapshotCut()
	}
	m.lockHeld = nil
	if len(held) != 30 {
		t.Fatalf("%d cuts timed", len(held))
	}
	slices.Sort(held)
	fastest, median, slowest := held[0], held[len(held)/2], held[len(held)-1]
	t.Logf("the lock is held %v at the fastest, %v at the median, %v at the slowest (race detector %v; bound %v)",
		fastest, median, slowest, raceEnabled, lockHoldBound())
	if fastest > lockHoldBound() {
		t.Fatalf("a snapshot held the model's lock for %v at the fastest of %d cuts, past %v", fastest, len(held), lockHoldBound())
	}
}

// TestDecodingASnapshotPeaksNearItsSize (plan 024 §3.5, the fourth limit): a
// client decoding a snapshot holds the snapshot and what it allocated to
// decode it — measured on the 4 MiB fixture, the whole of what DecodeSnapshot
// allocates is within 4× the encoding, and what the decoded snapshot retains
// within 2×. The client then folds one record at a time (C3).
func TestDecodingASnapshotPeaksNearItsSize(t *testing.T) {
	_, _, b := fourMiBFixture(t)
	var before, mid, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s, err := DecodeSnapshot(b)
	runtime.ReadMemStats(&mid)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	allocated := mid.TotalAlloc - before.TotalAlloc
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("decoding %d bytes allocated %d (%.2f×) and retains %d (%.2f×)",
		len(b), allocated, float64(allocated)/float64(len(b)), retained, float64(retained)/float64(len(b)))
	if allocated > uint64(4*len(b)) || retained > int64(2*len(b)) || retained < int64(len(b)/2) {
		t.Fatalf("decoding %d bytes allocated %d and retains %d", len(b), allocated, retained)
	}
	runtime.KeepAlive(s)
	runtime.KeepAlive(b)
}
