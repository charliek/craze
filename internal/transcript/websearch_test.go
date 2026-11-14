package transcript

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The turn's web tally (plan 051 F.4.3): the fold stamps a cumulative
// webSearch section with the turn running in the log, replaces whatever it
// held, resets it at each start, keeps it once the turn has ended, and every
// projection hands out a copy of its own.

func webDelta(searches int, urls ...string) agent.Event {
	return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{WebSearch: &agent.WebSearchState{Searches: searches, URLs: urls}}}
}

func turnStarted(id string) agent.Event {
	return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}}
}

func turnEnded(id string) agent.Event {
	return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Phase: agent.TurnEnded, StopReason: "end_turn"}}
}

func bracket(id string, running bool) agent.Event {
	return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: id, Running: running, Reason: agent.ReasonSubagentWake}}
}

// webOf is the model's turn tally, nil for none.
func webOf(m *Model) *TurnWebSearch { return m.State().Turn.WebSearch }

func wantWeb(t *testing.T, what string, got *TurnWebSearch, turn string, searches int, urls ...string) {
	t.Helper()
	want := &TurnWebSearch{TurnID: turn, WebSearchState: agent.WebSearchState{Searches: searches, URLs: urls}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: tally %+v, want %+v", what, got, want)
	}
}

// TestTheFoldStampsTheWebSectionWithTheRunningTurn: during an engine turn a
// section is that turn's; during a running bracket the bracket's, even with
// an engine turn's id still set (bracket first); a later section replaces the
// held one whole, never adds to it.
func TestTheFoldStampsTheWebSectionWithTheRunningTurn(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		turnStarted("turn-1"),
		webDelta(1, "https://a.example"),
		webDelta(2, "https://a.example", "https://b.example"),
	})...)
	wantWeb(t, "engine turn", webOf(m), "turn-1", 2, "https://a.example", "https://b.example")

	// A running bracket wins over an engine turn id still set
	// (RetryForeignTurn's bypass): the wake's deltas are the wake's.
	m = New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		turnStarted("turn-1"),
		bracket("wake-1", true),
		webDelta(3, "https://w.example"),
	})...)
	if m.State().Turn.ID != "turn-1" {
		t.Fatal("fixture: the engine turn is not current")
	}
	wantWeb(t, "running bracket", webOf(m), "wake-1", 3, "https://w.example")

	// A wake with no engine turn.
	m = New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{bracket("wake-2", true), webDelta(1)})...)
	wantWeb(t, "wake alone", webOf(m), "wake-2", 1)
}

// TestTheWebTallyResetsAtEachStartAndOutlivesItsEnd: kept once its turn has
// ended — Retries' lifetime — until the next start, a started or a running
// bracket, resets it.
func TestTheWebTallyResetsAtEachStartAndOutlivesItsEnd(t *testing.T) {
	m := New(Options{})
	evs := sequenced([]agent.Event{
		turnStarted("turn-1"),
		webDelta(2, "https://a.example"),
		turnEnded("turn-1"),
		turnStarted("turn-2"),
		webDelta(1, "https://b.example"),
		turnEnded("turn-2"),
		bracket("wake-1", true),
		webDelta(4),
		bracket("wake-1", false),
		bracket("wake-2", true),
	})
	foldAll(t, m, true, evs[:3]...)
	wantWeb(t, "after turn-1 ended", webOf(m), "turn-1", 2, "https://a.example")
	foldAll(t, m, true, evs[3])
	if w := webOf(m); w != nil {
		t.Fatalf("a started kept the last turn's tally: %+v", w)
	}
	foldAll(t, m, true, evs[4:6]...)
	wantWeb(t, "after turn-2 ended", webOf(m), "turn-2", 1, "https://b.example")
	foldAll(t, m, true, evs[6])
	if w := webOf(m); w != nil {
		t.Fatalf("a running bracket kept the last turn's tally: %+v", w)
	}
	foldAll(t, m, true, evs[7:9]...)
	wantWeb(t, "after the wake ended", webOf(m), "wake-1", 4)
	foldAll(t, m, true, evs[9])
	if w := webOf(m); w != nil {
		t.Fatalf("the next bracket kept the last wake's tally: %+v", w)
	}
}

// TestAWebSectionWithNoTurnRunningIsIgnored: neither an engine turn nor a
// running bracket — before any turn, after a turn's end, after a bracket's
// end — a section changes nothing.
func TestAWebSectionWithNoTurnRunningIsIgnored(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{webDelta(5, "https://x.example")})...)
	if w := webOf(m); w != nil {
		t.Fatalf("a section before any turn was stamped: %+v", w)
	}
	m = New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		turnStarted("turn-1"), webDelta(1), turnEnded("turn-1"), webDelta(9, "https://late.example"),
	})...)
	wantWeb(t, "after the end", webOf(m), "turn-1", 1)
	m = New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		bracket("wake-1", true), webDelta(1), bracket("wake-1", false), webDelta(9),
	})...)
	wantWeb(t, "after the bracket's end", webOf(m), "wake-1", 1)
}

// olderWeb is a body as a build that predates the webSearch section reads it:
// the key renamed to one no decoder knows, which every decoder skips.
func olderWeb(body []byte) []byte {
	return bytes.ReplaceAll(body, []byte(`"webSearch":`), []byte(`"webSearchOfALaterBuild":`))
}

// TestADecoderThatIgnoresWebSearchFoldsTheSameState (plan 051 F.4.3, the
// slash precedent's TestADecoderThatIgnoresSlashFoldsTheSameState): a client
// that predates the section folds every other part of the session exactly as
// one that knows it — the whole session script with web sections beside other
// sections and alone, decoded both ways — and a snapshot of it, decoded both
// ways and restored, the same again, but for the turn's tally. Negative
// control: a WebSearch fold row that also touched Retries goes red.
func TestADecoderThatIgnoresWebSearchFoldsTheSameState(t *testing.T) {
	evs := sessionScript()
	// turn-2 is running at the script's end: a section alone, and one beside
	// a title.
	evs = append(evs,
		sequencedAt(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{WebSearch: &agent.WebSearchState{Searches: 1, URLs: []string{"https://a.example"}}}, At: at(39)}, len(evs)+1),
		sequencedAt(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Title: strp("web"), WebSearch: &agent.WebSearchState{Searches: 2, Fetches: 1, Failed: 1,
			URLs: []string{"https://a.example", "https://b.example"}, URLsTruncated: true}}, At: at(39)}, len(evs)+2),
	)
	current, older := New(Options{}), New(Options{})
	sawAlone := false
	for _, ev := range evs {
		body, err := agent.EncodeEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		cur, err := agent.DecodeEvent(body)
		if err != nil {
			t.Fatal(err)
		}
		old, err := agent.DecodeEvent(string(olderWeb([]byte(body))))
		if err != nil {
			t.Fatalf("an older decoder refused %s: %v", body, err)
		}
		if old.State != nil && old.State.WebSearch == nil && cur.State.WebSearch != nil && cur.State.Title == nil {
			sawAlone = true
		}
		cur.Seq, old.Seq = ev.Seq, ev.Seq
		current.Fold(cur)
		older.Fold(old)
	}
	if !sawAlone {
		t.Fatal("fixture: the script has no web-only delta")
	}
	same := func(what string, cur, old *Model) {
		t.Helper()
		cs, os := cur.State(), old.State()
		if cs.Turn.WebSearch == nil || cs.Turn.WebSearch.TurnID != "turn-2" || cs.Turn.WebSearch.Searches != 2 {
			t.Fatalf("%s: fixture: the current client folded no tally: %+v", what, cs.Turn.WebSearch)
		}
		if os.Turn.WebSearch != nil {
			t.Fatalf("%s: the older client holds a tally: %+v", what, os.Turn.WebSearch)
		}
		cs.Turn.WebSearch = nil
		if !reflect.DeepEqual(cs, os) {
			t.Fatalf("%s: the states differ but for the tally:\ncurrent %+v\nolder   %+v", what, cs, os)
		}
		if !reflect.DeepEqual(cur.History(), old.History()) {
			t.Fatalf("%s: the histories differ", what)
		}
		if !reflect.DeepEqual(cur.EndedAsks(), old.EndedAsks()) {
			t.Fatalf("%s: the last-ended lists differ", what)
		}
	}
	same("folded", current, older)

	_, b := snapshotOf(t, current, 0)
	cur, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	old, err := DecodeSnapshot(olderWeb(b))
	if err != nil {
		t.Fatalf("an older decoder refused the snapshot: %v", err)
	}
	same("restored", Restore(cur, Options{}), Restore(old, Options{}))
}

// webModel is a model with turn-1 running and a tally of two URLs.
func webModel(t *testing.T) *Model {
	t.Helper()
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		turnStarted("turn-1"),
		{Type: agent.EventMeta, State: &agent.StateDelta{WebSearch: &agent.WebSearchState{Searches: 2, Fetches: 1, Failed: 1,
			URLs: []string{"https://a.example", "https://b.example"}, URLsTruncated: true}}},
	})...)
	return m
}

// TestTheWebTallyRoundTripsThroughASnapshot: a snapshot carries turn.webSearch
// with its turnId, the codec gives it back equal, and a model restored from
// it — in process or decoded — reports it, its turnId included.
func TestTheWebTallyRoundTripsThroughASnapshot(t *testing.T) {
	m := webModel(t)
	s, b := snapshotOf(t, m, 0)
	want := &TurnWebSearch{TurnID: "turn-1", WebSearchState: agent.WebSearchState{Searches: 2, Fetches: 1, Failed: 1,
		URLs: []string{"https://a.example", "https://b.example"}, URLsTruncated: true}}
	if !reflect.DeepEqual(s.Turn.WebSearch, want) {
		t.Fatalf("the snapshot's tally %+v, want %+v", s.Turn.WebSearch, want)
	}
	if !bytes.Contains(b, []byte(`"webSearch":{"turnId":"turn-1","searches":2,"fetches":1,"failed":1,"urls":["https://a.example","https://b.example"],"urlsTruncated":true}`)) {
		t.Fatalf("the encoding: %s", b)
	}
	d, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Turn.WebSearch, want) {
		t.Fatalf("decoded: %+v", d.Turn.WebSearch)
	}
	r1, r2 := restoredBoth(t, s, b, Options{})
	for _, r := range []*Model{r1, r2} {
		if got := webOf(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("restored: %+v", got)
		}
	}
	assertSameModel(t, "restored", m, r2)
	// A tally of zero counts still names its turn, and the members are
	// omitted when zero, empty or false.
	z := New(Options{})
	foldAll(t, z, true, sequenced([]agent.Event{turnStarted("turn-7"), {Type: agent.EventMeta, State: &agent.StateDelta{WebSearch: &agent.WebSearchState{}}}})...)
	_, zb := snapshotOf(t, z, 0)
	if !bytes.Contains(zb, []byte(`"webSearch":{"turnId":"turn-7"}`)) {
		t.Fatalf("an empty tally's encoding: %s", zb)
	}
}

// TestASnapshotCutBeforeALaterWebDeltaIsUnchangedByIt: the fold never writes
// through a tally it holds, so a snapshot cut before a later delta keeps the
// tally it was cut with.
func TestASnapshotCutBeforeALaterWebDeltaIsUnchangedByIt(t *testing.T) {
	m := webModel(t)
	s, _ := snapshotOf(t, m, 0)
	keep := s.Turn.WebSearch.clone()
	ev := webDelta(9, "https://c.example")
	ev.Seq = 3
	m.Fold(ev)
	if !reflect.DeepEqual(s.Turn.WebSearch, keep) {
		t.Fatalf("a later delta reached an earlier snapshot: %+v, want %+v", s.Turn.WebSearch, keep)
	}
	wantWeb(t, "the model", webOf(m), "turn-1", 9, "https://c.example")
}

// TestWritingThroughAProjectionsWebTallyChangesNothing (plan 051 F.4.3,
// ownership): a caller writing through a returned Snapshot's, Mirror's or
// State's Turn.WebSearch and its URLs changes nothing the model next reports;
// one writing through the snapshot it passed to Restore changes nothing in
// the restored model. Negative control: any of the three boundaries handing
// the pointer, or the URL list, through goes red.
func TestWritingThroughAProjectionsWebTallyChangesNothing(t *testing.T) {
	m := webModel(t)
	want := webOf(m).clone()
	scribble := func(w *TurnWebSearch) {
		w.URLs[0] = "https://scribbled.example"
		w.URLs = append(w.URLs, "https://more.example")
		w.Searches, w.TurnID = 99, "turn-99"
	}

	s, _ := snapshotOf(t, m, 0)
	scribble(s.Turn.WebSearch)
	if got := webOf(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("writing through a snapshot reached the model: %+v", got)
	}
	mr := m.Mirror()
	scribble(mr.Turn.WebSearch)
	if got := m.Mirror().Turn.WebSearch; !reflect.DeepEqual(got, want) {
		t.Fatalf("writing through a mirror reached the model: %+v", got)
	}
	st := m.State()
	scribble(st.Turn.WebSearch)
	if got, _ := snapshotOf(t, m, 0); !reflect.DeepEqual(got.Turn.WebSearch, want) {
		t.Fatalf("writing through a state reached the next snapshot: %+v", got.Turn.WebSearch)
	}

	src, _ := snapshotOf(t, m, 0)
	r := Restore(src, Options{})
	scribble(src.Turn.WebSearch)
	if got := webOf(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("writing through the snapshot Restore was given reached the restored model: %+v", got)
	}
	if got := r.Mirror().Turn.WebSearch; !reflect.DeepEqual(got, want) {
		t.Fatalf("the restored model's mirror: %+v", got)
	}
}
