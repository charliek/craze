package transcript

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

func TestDefaultBoundsAreTheTUIs(t *testing.T) {
	want := Bounds{MainEntries: 5000, MainBytes: 8 << 20, SubEntries: 1000, SubBytes: 1 << 20, StreamText: 64 << 10, Agents: 32}
	if got := DefaultBounds(); got != want {
		t.Fatalf("DefaultBounds() = %+v, want %+v", got, want)
	}
	if maxEntries != want.MainEntries {
		t.Fatalf("the moved tests' maxEntries is %d, the bound %d", maxEntries, want.MainEntries)
	}
	if got := (Bounds{}).withDefaults(); got != want {
		t.Fatalf("zero bounds are the defaults: %+v", got)
	}
	if got := (Bounds{StreamText: 1}).withDefaults().StreamText; got != minStreamText {
		t.Fatalf("a stream cap below its ellipsis is raised to %d, got %d", minStreamText, got)
	}
}

func note(i int) agent.Event {
	return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: fmt.Sprintf("row %03d", i)}}
}

// TestTheEntryCapTrimsFromTheHead: past the entry cap the oldest entries go —
// in the main transcript and a child's, each at its own bound — the model
// says it trimmed, a trimmed tool's id is forgotten so a later update to it
// appends a new row (today's rule), and the counters stay equal to a recount
// after every fold.
func TestTheEntryCapTrimsFromTheHead(t *testing.T) {
	for _, who := range []string{"", "sub"} {
		t.Run("scope "+who, func(t *testing.T) {
			m := New(Options{Bounds: Bounds{MainEntries: 8, SubEntries: 5}})
			bound := 8
			if who != "" {
				bound = 5
			}
			tool := func(id, status string) agent.Event {
				return agent.Event{Type: agent.EventTool, Agent: who, Tool: &agent.ToolEvent{ID: id, Status: status}}
			}
			cmd := func(i int) agent.Event {
				return agent.Event{Type: agent.EventCommand, Agent: who, Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Qualified: fmt.Sprintf("p:%d", i)}}}
			}
			foldAll(t, m, true, tool("old", "pending"))
			for i := range bound - 1 {
				foldAll(t, m, true, cmd(i))
			}
			tr := m.ensureSub(who)
			if trimmed(tr) || len(tr.live()) != bound {
				t.Fatalf("at the cap nothing is trimmed yet: %v", facts(tr))
			}
			foldAll(t, m, true, cmd(99))
			if !trimmed(tr) || len(tr.live()) != bound || factsOf(tr, "tool") != nil {
				t.Fatalf("one past the cap drops the oldest: %v", facts(tr))
			}
			if _, ok := toolIndexed(tr, "old"); ok {
				t.Fatal("a trimmed tool's id is forgotten")
			}
			foldAll(t, m, true, tool("old", "completed"))
			if got := factsOf(tr, "tool"); len(got) != 1 || tr.live()[bound-1].Tool.Status != "completed" {
				t.Fatalf("an update to a trimmed id appends a new row: %v", facts(tr))
			}
			for i := range 200 {
				foldAll(t, m, true, cmd(i))
			}
			if len(tr.ents) > 2*bound+64 {
				t.Fatalf("the deque's backing array grew to %d for %d live entries: it is never compacted", len(tr.ents), bound)
			}
		})
	}
}

// TestTheByteCapTrimsFromTheHead: the retained-byte budget drops the oldest
// entries until the rest fit — on an append and on a chunk that grows the open
// entry, but never on a tool update in place (r2 finding 2), whose growth
// waits for the next append — and never the last entry, however large it is
// on its own.
func TestTheByteCapTrimsFromTheHead(t *testing.T) {
	m := New(Options{Bounds: Bounds{MainBytes: 100}})
	for i := range 3 {
		foldAll(t, m, true, note(i)) // "row 00N": 7 bytes each
	}
	big := strings.Repeat("x", 80)
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: big}})
	if got := factTexts(m.Main, "error"); len(got) != 3 || got[0] != "row 001" || !trimmed(m.Main) {
		t.Fatalf("80 more bytes over 21 drop the oldest row: %q", got)
	}
	// A chunk growing the open entry trims too: 7+7+80+3 fits, 7+7+80+8 does
	// not, and dropping the oldest row makes it fit.
	foldAll(t, m, true, agent.Event{Type: agent.EventText, Text: "abc"})
	if got := len(m.Main.live()); got != 4 {
		t.Fatalf("97 bytes fit: %v", facts(m.Main))
	}
	foldAll(t, m, true, agent.Event{Type: agent.EventText, Text: strings.Repeat("y", 5)})
	if got := facts(m.Main); len(got) != 3 || got[0].Text != "row 002" || got[2].Text != "abcyyyyy" {
		t.Fatalf("the open reply's growth trims the head: %v", got)
	}
	// The last entry stays, whatever its size.
	huge := strings.Repeat("z", 500)
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: huge}})
	if got := facts(m.Main); len(got) != 1 || got[0].Text != huge || m.Main.bytes != 500 {
		t.Fatalf("one entry over the budget is kept alone: %v", got)
	}
	// A tool update that grows its payload past the budget trims nothing: a
	// trim could drop the row it just updated, and the update re-applied
	// would then append it again (r2 finding 2). The next append enforces the
	// budget, dropping from the head until the rest fit.
	u := New(Options{Bounds: Bounds{MainBytes: 210}})
	foldAll(t, u, true,
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", RawInput: "ls"}},
		note(1), note(2),
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "u", RawInput: "x"}},
		// 1+196 bytes, beside 3+7+7 above it: 214, over the budget by the 195
		// the update added.
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "u", RawInput: strings.Repeat("q", 196)}},
	)
	if got := facts(u.Main); len(got) != 4 || u.Main.bytes != 214 || u.Main.grown != 195 || trimmed(u.Main) {
		t.Fatalf("an update in place trims nothing: %v (%d bytes, %d grown in place)", got, u.Main.bytes, u.Main.grown)
	}
	// 214 + 7 = 221: the tool t and both rows go (17 bytes), and 204 fit.
	foldAll(t, u, true, note(3))
	if got := facts(u.Main); len(got) != 2 || got[0].ToolID != "u" || got[1].Text != "row 003" || u.Main.bytes != 204 || u.Main.grown != 0 {
		t.Fatalf("the next append enforces the budget: %v (%d bytes)", got, u.Main.bytes)
	}
	if _, ok := toolIndexed(u.Main, "t"); ok {
		t.Fatal("the trimmed tool is forgotten")
	}
}

// fullTool is a tool report at every payload cap of internal/agent/tools.go:
// 512 B of raw input, 2 KiB of content, 8 locations, three 8 KiB output tails
// and two 512 B heads, and 8 diffs of 64 KiB old and new text.
func fullTool(id string) *agent.ToolEvent {
	code := 1
	tl := &agent.ToolEvent{
		ID: id, Name: "edit", Status: "completed", Kind: "edit", Title: "Edit main.go", ToolName: "edit",
		RawInput:    strings.Repeat("r", 512),
		ContentText: strings.Repeat("c", 2048),
		Output: &agent.ToolOutput{
			ExitCode: &code, Stdout: strings.Repeat("o", 8<<10), Stderr: strings.Repeat("e", 8<<10),
			Content: strings.Repeat("n", 8<<10), StdoutHead: strings.Repeat("h", 512), StderrHead: strings.Repeat("H", 512),
		},
		Task: &agent.TaskInfo{Description: "d", Prompt: "p", Model: "m", AgentID: "a", SubagentType: "s", Status: "completed"},
	}
	for i := range 8 {
		tl.Locations = append(tl.Locations, fmt.Sprintf("/w/f%d.go", i))
		tl.Diffs = append(tl.Diffs, agent.ToolDiff{Path: fmt.Sprintf("/w/f%d.go", i), OldText: strings.Repeat("-", 64<<10), NewText: strings.Repeat("+", 64<<10)})
	}
	return tl
}

// TestAToolUpdateAtTheOutputCapReaccounts: a tool's payload is what dominates a
// retained entry (§2.6), and an update swaps the payload and re-accounts it —
// up and down — with the counter equal to a recount throughout.
func TestAToolUpdateAtTheOutputCapReaccounts(t *testing.T) {
	m := New(Options{})
	full := fullTool("t")
	foldAll(t, m, true, agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", RawInput: "x"}})
	small := m.Main.bytes
	foldAll(t, m, true, agent.Event{Type: agent.EventTool, Tool: full})
	want := 8*(2*(64<<10)+len("/w/f0.go")) + 3*(8<<10) + 2*512 + 512 + 2048 + 8*len("/w/f0.go") +
		len("t") + len("edit") + len("completed") + len("edit") + len("Edit main.go") + len("edit") +
		len("dpmascompleted")
	if got := m.Main.live()[0].Bytes; got != want || m.Main.bytes != want {
		t.Fatalf("a tool at the output cap accounts %d (counter %d), want %d", got, m.Main.bytes, want)
	}
	foldAll(t, m, true, agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", RawInput: "x"}})
	if m.Main.bytes != small {
		t.Fatalf("the update back to a small payload re-accounts to %d, want %d", m.Main.bytes, small)
	}
}

// TestTheAccountingCountsEveryPayloadString holds toolBytes and planBytes
// against their types by reflection: every string reachable from a ToolEvent
// or a PlanEvent — through pointers, slices and nested structs — is filled
// with a distinct length, and the accounting must count all of them. A string
// field added to either type fails here until it is counted.
func TestTheAccountingCountsEveryPayloadString(t *testing.T) {
	var tool agent.ToolEvent
	n := 0
	wantTool := fillStrings(reflect.ValueOf(&tool).Elem(), &n)
	if got := toolBytes(&tool); got != wantTool {
		t.Fatalf("toolBytes counts %d of the %d string bytes a ToolEvent carries: a string field is not accounted", got, wantTool)
	}
	var plan agent.PlanEvent
	wantPlan := fillStrings(reflect.ValueOf(&plan).Elem(), &n)
	if got := planBytes(&plan); got != wantPlan {
		t.Fatalf("planBytes counts %d of the %d string bytes a PlanEvent carries", got, wantPlan)
	}
	// An error whose text the fold read accounts that text; one it could not
	// read is charged errValueBytes.
	if got := entryBytes(&Entry{Text: "abc", Tool: &tool, Plan: &plan, Err: fmt.Errorf("x")}, 64); got != 3+wantTool+wantPlan {
		t.Fatalf("entryBytes with a read error %d", got)
	}
	if got := entryBytes(&Entry{Tool: &tool, Plan: &plan, Err: fmt.Errorf("x")}, 64); got != wantTool+wantPlan+errValueBytes {
		t.Fatalf("entryBytes with an unread error %d", got)
	}
	// A cut stream entry's text accounts the stream cap, whatever its tail
	// kept (X25).
	if got := entryBytes(&Entry{Text: "…é", Cut: true}, 64); got != 64 {
		t.Fatalf("entryBytes of a cut tail %d, want the cap", got)
	}
}

// fillStrings gives every string reachable from v a distinct length (and
// every slice two elements), returning the total string bytes.
func fillStrings(v reflect.Value, n *int) int {
	total := 0
	switch v.Kind() {
	case reflect.String:
		*n++
		v.SetString(strings.Repeat("s", *n))
		return *n
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return fillStrings(v.Elem(), n)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			return 0
		}
		for i := range v.NumField() {
			total += fillStrings(v.Field(i), n)
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range 2 {
			total += fillStrings(v.Index(i), n)
		}
	}
	return total
}

// TestTheBuilderKeepsTodaysTail: the open run's tail, at every chunk, and the
// closed entry's Text are exactly today's capEntryText of the whole run — the
// "…" prefix, the cut moved forward to a rune start — whatever the chunk
// sizes (empty, one byte, around the cap, past 2 × the cap) and wherever a
// multi-byte rune straddles the cut, including bytes that are not UTF-8 at
// all and long runs of continuation bytes, where the cut has no rune start to
// land on for a whole chunk or several (r2 finding 4); the builder never
// holds more than 2 × the cap; and the entry, open and closed, accounts
// min(bytes streamed, StreamText) and is Cut exactly when the run is longer
// than the cap (X25) — not its tail's length, which a cut inside a rune makes
// up to three bytes shorter.
func TestTheBuilderKeepsTodaysTail(t *testing.T) {
	runes := []string{"a", "é", "⤷", "😀", "\x80", "\xe2\x82", "\n"}
	leads := []string{"", "", "\xe2", "\xf0", "a"}
	for _, limit := range []int{4, 5, 7, 16, 64, 64 << 10} {
		for seed := range uint64(12) {
			r := rand.New(rand.NewPCG(seed, uint64(limit)))
			m := New(Options{Bounds: Bounds{StreamText: limit, MainBytes: 1 << 30}})
			tr := m.Main
			var whole strings.Builder
			steps := 60
			if limit > 1024 {
				steps = 25
			}
			for step := range steps {
				var chunk strings.Builder
				size := r.IntN(3 * limit)
				switch r.IntN(6) {
				case 0:
					size = 0
				case 1:
					size = limit - 3 + r.IntN(7)
				case 2:
					size = 2*limit + r.IntN(limit)
				}
				if size > 0 && r.IntN(3) == 0 {
					// A run of continuation bytes, maybe behind a lead byte.
					chunk.WriteString(leads[r.IntN(len(leads))])
					chunk.WriteString(strings.Repeat("\x80", size))
				}
				for chunk.Len() < size {
					chunk.WriteString(runes[r.IntN(len(runes))])
				}
				m.Fold(agent.Event{Type: agent.EventThought, Text: chunk.String()})
				whole.WriteString(chunk.String())
				if whole.Len() == 0 {
					continue
				}
				want := capText(whole.String(), limit)
				if got := tr.tail(); got != want {
					t.Fatalf("limit %d seed %d step %d: tail %q, want %q", limit, seed, step, clip(got), clip(want))
				}
				if e := tr.current(tr.live()[0]); e.Bytes != min(whole.Len(), limit) || e.Cut != (whole.Len() > limit) {
					t.Fatalf("limit %d: the open entry accounts %d (cut %v) after %d bytes", limit, e.Bytes, e.Cut, whole.Len())
				}
				if len(tr.buf) > 2*limit {
					t.Fatalf("limit %d: the builder holds %d bytes", limit, len(tr.buf))
				}
				checkInvariants(t, m)
			}
			if whole.Len() == 0 {
				continue
			}
			m.Fold(agent.Event{Type: agent.EventDone})
			if got, want := tr.live()[0].Text, capText(whole.String(), limit); got != want {
				t.Fatalf("limit %d seed %d: the closed entry %q, want %q", limit, seed, clip(got), clip(want))
			}
			if e := tr.live()[0]; e.Bytes != min(whole.Len(), limit) || e.Cut != (whole.Len() > limit) {
				t.Fatalf("limit %d seed %d: the closed entry accounts %d (cut %v) after %d bytes", limit, seed, e.Bytes, e.Cut, whole.Len())
			}
			checkInvariants(t, m)
		}
	}
}

// TestTheBuilderOutlivesItsRunButNotAFinishedChild (r2 finding 7): a closed
// run leaves its builder's capacity for the transcript's next run — at most
// 2 × StreamText — so a long reply does not cost a new builder each time;
// a child's goes when its roster row finishes, since children are many.
func TestTheBuilderOutlivesItsRunButNotAFinishedChild(t *testing.T) {
	m := New(Options{})
	long := strings.Repeat("x", 200<<10)
	foldAll(t, m, true,
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "a", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned},
		agent.Event{Type: agent.EventText, Agent: "a", Text: long},
		agent.Event{Type: agent.EventText, Text: long},
		agent.Event{Type: agent.EventDone, StopReason: "end_turn"},
	)
	limit := 2 * DefaultBounds().StreamText
	if c := cap(m.Main.buf); c == 0 || c > limit {
		t.Fatalf("the main builder's capacity after its run closed is %d, want kept and at most %d", c, limit)
	}
	child := m.Sub("a")
	if c := cap(child.buf); c == 0 || c > limit {
		t.Fatalf("the running child's builder capacity is %d, want at most %d", c, limit)
	}
	foldAll(t, m, true, agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "a", Status: agent.SubagentCompleted}, SubagentChange: agent.SubagentChangeFinished})
	if child.buf != nil || !strings.HasPrefix(factsOf(child, "assistant")[0].Text, "…") {
		t.Fatalf("a finished child's builder is let go (cap %d), its closed reply kept", cap(child.buf))
	}
}

// TestASaturatedMalformedRunKeepsTodaysTail is r2 finding 4's schedule: a run
// already 2 × StreamText of continuation bytes, grown one continuation byte at
// a time — where the cut never finds a rune start — and then by bytes that
// give it one. The tail is capText's at every step.
func TestASaturatedMalformedRunKeepsTodaysTail(t *testing.T) {
	const limit = 64 << 10
	m := New(Options{Bounds: Bounds{StreamText: limit, MainBytes: 1 << 30}})
	tr := m.Main
	var whole strings.Builder
	add := func(s string) {
		t.Helper()
		m.Fold(agent.Event{Type: agent.EventText, Text: s})
		whole.WriteString(s)
		if got, want := tr.tail(), capText(whole.String(), limit); got != want {
			t.Fatalf("after %d bytes: tail %q, want %q", whole.Len(), clip(got), clip(want))
		}
		checkInvariants(t, m)
	}
	add(strings.Repeat("\x80", 2*limit))
	for range 300 {
		add("\x80")
	}
	add("a")
	for range 300 {
		add("\x80")
	}
	add("é" + strings.Repeat("\x80", limit))
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:40] + "…" + s[len(s)-40:]
	}
	return s
}

// TestCapTextIsTodaysCapEntryText pins the port against the TUI's rule on the
// shapes that matter at the 64 KiB cap.
func TestCapTextIsTodaysCapEntryText(t *testing.T) {
	const limit = 64 << 10
	fits := strings.Repeat("a", limit)
	if capText(fits, limit) != fits {
		t.Fatal("text at the cap is kept whole")
	}
	over := "b" + fits
	if got := capText(over, limit); got != "…"+fits[3:] || len(got) != limit {
		t.Fatalf("one byte over keeps the last %d bytes after the ellipsis (%d)", limit-3, len(got))
	}
	// A 3-byte rune straddling the cut moves it forward to the next rune.
	s := strings.Repeat("⤷", limit)
	got := capText(s, limit)
	if !strings.HasPrefix(got, "…⤷") || !utf8.ValidString(got) || len(got) > limit {
		t.Fatalf("the cut lands on a rune start: %q…", got[:12])
	}
}

// TestTheRosterEvictsThe33rdFinishedRowOldestFirst is the live session's rule
// (subagents.go evictFinishedLocked): once more than Bounds.Agents rows have
// finished, the oldest FINISH goes — by EndedAt, ties broken by finish order,
// never the row that just finished, never a running row — and its child
// transcript goes with it.
func TestTheRosterEvictsThe33rdFinishedRowOldestFirst(t *testing.T) {
	m := New(Options{})
	sub := func(id string, status agent.SubagentStatus, change string, ended time.Time) agent.Event {
		return agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: status, EndedAt: ended}, SubagentChange: change}
	}
	// A long-runner spawned first, and 32 children that finish, each with a
	// streamed transcript. Two share an EndedAt, so finish order decides.
	foldAll(t, m, true, sub("runner", agent.SubagentRunning, agent.SubagentChangeSpawned, time.Time{}))
	var ids []string
	for i := range 32 {
		id := fmt.Sprintf("c%02d", i)
		ids = append(ids, id)
		ended := at(100 + i)
		if i == 1 {
			ended = at(100) // ties with c00, finishes after it
		}
		foldAll(t, m, true,
			sub(id, agent.SubagentRunning, agent.SubagentChangeSpawned, time.Time{}),
			agent.Event{Type: agent.EventText, Agent: id, Text: "work of " + id},
			sub(id, agent.SubagentCompleted, agent.SubagentChangeFinished, ended),
		)
	}
	if got := len(m.State().Agents); got != 33 {
		t.Fatalf("32 finished and one running are all kept: %d", got)
	}
	// The 33rd finish evicts the oldest finish: c00 (EndedAt 100, finished
	// before c01 at the same instant).
	foldAll(t, m, true, sub("c32", agent.SubagentRunning, agent.SubagentChangeSpawned, time.Time{}))
	foldAll(t, m, true, sub("c32", agent.SubagentCompleted, agent.SubagentChangeFinished, at(200)))
	roster := func() (out []string) {
		for _, a := range m.State().Agents {
			out = append(out, a.ID)
		}
		return out
	}
	if got := roster(); slices.Contains(got, "c00") || !slices.Contains(got, "c01") || !slices.Contains(got, "runner") || len(got) != 33 {
		t.Fatalf("c00 is the oldest finish and goes: %v", got)
	}
	if m.Sub("c00") != nil || slices.Contains(m.Subs(), "c00") {
		t.Fatal("the evicted row's transcript goes with it")
	}
	if m.Sub("c01") == nil || len(m.Sub("c01").live()) != 1 {
		t.Fatal("a kept row keeps its transcript")
	}
	// A restated finish is not a new one; the row that just finished is never
	// the one evicted, even when its EndedAt is the oldest.
	foldAll(t, m, true, sub("c01", agent.SubagentCompleted, agent.SubagentChangeFinished, at(100)))
	if !slices.Contains(roster(), "c01") {
		t.Fatal("a restated finish evicted a row")
	}
	foldAll(t, m, true,
		sub("late", agent.SubagentRunning, agent.SubagentChangeSpawned, time.Time{}),
		sub("late", agent.SubagentFailed, agent.SubagentChangeFinished, at(1)),
	)
	if got := roster(); !slices.Contains(got, "late") || slices.Contains(got, "c01") {
		t.Fatalf("the oldest other finish (c01) goes, never the one that just finished: %v", got)
	}
	// The running row outlives every eviction.
	if !slices.Contains(roster(), "runner") {
		t.Fatal("a running row was evicted")
	}
	_ = ids
}
