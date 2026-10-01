package tui

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// The message side of SF-102 (plan 032 §3.3 C6) through the model: a streaming
// reply's rows resume from a checkpoint and still draw what a full render
// draws; a replay paints on its cadence; and the work a paint does — counted,
// never timed — grows with the chunk count and not with the transcript.

// lastAssistant is the main pane's newest assistant row, nil when it has none.
func lastAssistant(m Model) *entry {
	for i := len(m.main.rows) - 1; i >= 0; i-- {
		if e := m.main.rows[i]; e.kind == entryAssistant {
			return e
		}
	}
	return nil
}

// assertFullRender holds the newest assistant row's rows to renderMarkdown's
// of its text at the model's width and theme.
func assertFullRender(t *testing.T, m Model, at string) *entry {
	t.Helper()
	e := lastAssistant(m)
	if e == nil {
		t.Fatalf("%s: no assistant row", at)
	}
	if want := renderMarkdown(e.text, m.width, m.theme); !slices.Equal(e.rendered, want) {
		t.Fatalf("%s: the row draws what a full render does not\ntext %q\n got %q\nwant %q", at, e.text, e.rendered, want)
	}
	return e
}

// TestStreamedRepliesDrawWhatAFullRenderDraws streams seeded random markdown
// through Update, live: every chunk's rows are the full render of the text,
// and most chunks resumed rather than rendered from the top. A resize and a
// theme change mid-stream start over and resume again; the run's close drops
// the checkpoint; past the stream cap every chunk is a full render (the head
// moves, and with it the generation); and a continuation after /clear resumes
// within its own text.
func TestStreamedRepliesDrawWhatAFullRenderDraws(t *testing.T) {
	t.Run("a reply", func(t *testing.T) {
		isolateSkillsHome(t)
		m := startStub(t, NewStub(), t.TempDir(), 100, 30)
		rng := rand.New(rand.NewSource(5))
		var doc strings.Builder
		for doc.Len() < 1500 {
			doc.WriteString(mdRandomDoc(rng))
			doc.WriteString("\n\n")
		}
		chunks := mdChunks(rng, doc.String())
		resumed := 0
		for i, c := range chunks {
			before := m.main.work.mdBytes
			m = feed(t, m, agent.Event{Type: agent.EventText, Text: c})
			e := assertFullRender(t, m, fmt.Sprintf("chunk %d", i))
			if m.main.work.mdBytes-before < len(e.text) {
				resumed++
			}
			switch i {
			case len(chunks) / 3:
				m = applyMsg(t, m, tea.WindowSizeMsg{Width: 77, Height: 30})
				assertFullRender(t, m, "after a resize")
			case 2 * len(chunks) / 3:
				m.applyTheme(Preset("gruvbox"))
				assertFullRender(t, m, "after a theme change")
			}
		}
		if resumed*2 < len(chunks) {
			t.Fatalf("%d of %d chunks resumed their render", resumed, len(chunks))
		}
		m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
		if e := assertFullRender(t, m, "the run closed"); e.md != nil || e.gen != 0 {
			t.Fatalf("the closed row keeps a checkpoint (%v) or a generation (%d)", e.md != nil, e.gen)
		}
	})

	t.Run("past the cap", func(t *testing.T) {
		isolateSkillsHome(t)
		m := startStub(t, NewStub(), t.TempDir(), 100, 30)
		const para = "a paragraph of words that wraps.\n\n" // 34 bytes
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: strings.Repeat(para, entryTextCap/len(para)-2)})
		for i := range 6 {
			before := m.main.work.mdBytes
			m = feed(t, m, agent.Event{Type: agent.EventText, Text: strings.Repeat("y", 39) + "\n"})
			e := assertFullRender(t, m, fmt.Sprintf("chunk %d", i))
			cut := strings.HasPrefix(e.text, "…")
			if read := m.main.work.mdBytes - before; cut != (read == len(e.text)) {
				t.Fatalf("chunk %d (cut %v) read %d of %d bytes: under the cap a chunk resumes, past it none does", i, cut, read, len(e.text))
			}
			if i == 5 && !cut {
				t.Fatal("the run never passed the cap")
			}
		}
	})

	t.Run("a continuation after /clear", func(t *testing.T) {
		isolateSkillsHome(t)
		m := sized(t)
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "before the clear\n\nmore before\n\n"})
		m = runSlash(t, m, "/clear")
		resumed := 0
		for i := range 40 {
			before := m.main.work.mdBytes
			m = feed(t, m, agent.Event{Type: agent.EventText, Text: "after " + strconv.Itoa(i) + "\n\n"})
			e := assertFullRender(t, m, fmt.Sprintf("chunk %d", i))
			if !e.cont || strings.Contains(e.text, "before") {
				t.Fatalf("chunk %d: the row is not the continuation of the clear (cont %v, text %q)", i, e.cont, e.text)
			}
			if m.main.work.mdBytes-before < len(e.text) {
				resumed++
			}
		}
		if resumed < 30 {
			t.Fatalf("the continuation resumed %d of 40 renders", resumed)
		}
	})
}

// replayLine is cmd/craze-fake-agent's load-long chunk: one paragraph.
func replayLine(i int) agent.Event {
	return replayed(agent.Event{Type: agent.EventText, Text: "line " + strconv.Itoa(i) + "\n"})
}

// proseChunk is a token-sized chunk with a paragraph break every 60 tokens.
func proseChunk(i int) string {
	if i%60 == 59 {
		return "end.\n\n"
	}
	return "word" + strconv.Itoa(i%10) + " "
}

// TestAReplayPaintsEvery256thEventAndWhenForced is the replay's cadence (plan
// 032 §3.3 C6): while the model replays, finish paints the drawn pane on every
// 256th event folded and on a forced refresh — a resize, Ctrl+O, a theme, a
// view switch, a press into the transcript, a restore, the replay's failure or
// its end — and on nothing else: not on a tick, not on an event between
// boundaries. While the replay runs only the paints are counted: what a frame
// shows is read once the replay has ended or failed, and what the boundaries
// and the forced refreshes draw is held to painting every event by
// TestTheReplayCadenceDrawsWhatPaintingEveryEventDraws.
func TestAReplayPaintsEvery256thEventAndWhenForced(t *testing.T) {
	// step applies msg and reports whether the main pane was painted.
	step := func(t *testing.T, m *Model, msg tea.Msg) bool {
		t.Helper()
		before := m.main.work.paints
		*m = applyMsg(t, *m, msg)
		return m.main.work.paints > before
	}
	// replayTo folds replayed lines until n events have folded since the
	// start, each painting exactly on a boundary.
	replayTo := func(t *testing.T, m *Model, folded *int, line *int, n int) {
		t.Helper()
		for *folded < n {
			*line++
			painted := step(t, m, eventMsg{ev: replayLine(*line)})
			*folded++
			if painted != (*folded%replayPaintEvery == 0) {
				t.Fatalf("event %d of the replay: painted %v", *folded, painted)
			}
		}
	}
	start := func(t *testing.T) (Model, int, int) {
		t.Helper()
		m, _ := loadedStub(t, nil)
		if step(t, &m, eventMsg{ev: replayEvent(agent.ReplayStart)}) {
			t.Fatal("the replay's start painted")
		}
		return m, 1, 0
	}

	t.Run("the cadence and the forced refreshes", func(t *testing.T) {
		m, folded, line := start(t)
		replayTo(t, &m, &folded, &line, replayPaintEvery)
		if step(t, &m, tickMsg{gen: m.tickGen}) {
			t.Fatal("a tick painted mid-replay")
		}
		forced := []struct {
			name string
			do   func(m *Model) bool
		}{
			{"a resize", func(m *Model) bool { return step(t, m, tea.WindowSizeMsg{Width: 100, Height: 30}) }},
			{"Ctrl+O", func(m *Model) bool { return step(t, m, tea.KeyMsg{Type: tea.KeyCtrlO}) }},
			{"a theme", func(m *Model) bool {
				before := m.main.work.paints
				m.applyTheme(Preset("gruvbox"))
				return m.main.work.paints > before
			}},
			{"a view switch", func(m *Model) bool {
				before := m.main.work.paints
				m.enterView("child")
				m.leaveView()
				return m.main.work.paints > before
			}},
			{"a press into the transcript", func(m *Model) bool {
				tr := m.lay.Region(regionTranscript)
				return step(t, m, tea.MouseMsg{X: 2, Y: tr.Top, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			}},
			{"a restore", func(m *Model) bool {
				snap, err := m.shared.Snapshot(0)
				if err != nil {
					t.Fatal(err)
				}
				painted := step(t, m, restoreMsg{snap: snap, info: m.info()})
				if !m.replaying {
					t.Fatal("a restore of a replaying session ended the replay")
				}
				return painted
			}},
		}
		for _, f := range forced {
			// A line lands unpainted, then the refresh paints it.
			line++
			if step(t, &m, eventMsg{ev: replayLine(line)}) {
				t.Fatalf("before %s: an event between boundaries painted", f.name)
			}
			folded++
			if !f.do(&m) {
				t.Fatalf("%s did not paint mid-replay", f.name)
			}
			if m.cur().dirty {
				t.Fatalf("%s left the drawn pane unpainted", f.name)
			}
		}
		// The forced refreshes leave the cadence where it was.
		replayTo(t, &m, &folded, &line, 2*replayPaintEvery)
		line++
		if step(t, &m, eventMsg{ev: replayLine(line)}) {
			t.Fatal("an event after the boundary painted")
		}
		if !step(t, &m, eventMsg{ev: replayEvent(agent.ReplayEnd)}) {
			t.Fatal("the replay's end did not paint")
		}
		if v := plainView(m); !strings.Contains(v, "line "+strconv.Itoa(line)) || !strings.Contains(v, restoredNote) {
			t.Fatalf("the replay's end did not draw the restored transcript:\n%s", v)
		}
		// A live session paints every event again.
		if !step(t, &m, eventMsg{ev: agent.Event{Type: agent.EventText, Text: "live"}}) {
			t.Fatal("a live event after the replay did not paint")
		}
	})

	t.Run("a failed replay", func(t *testing.T) {
		m, folded, line := start(t)
		replayTo(t, &m, &folded, &line, 100)
		if !step(t, &m, errMsg{err: errors.New("the load failed")}) {
			t.Fatal("the replay's failure did not paint")
		}
		if v := plainView(m); !strings.Contains(v, "line 99") || !strings.Contains(v, "the load failed") {
			t.Fatalf("the failed replay's frame:\n%s", v)
		}
	})

	t.Run("a stream that ends mid-replay", func(t *testing.T) {
		m, folded, line := start(t)
		replayTo(t, &m, &folded, &line, 100)
		if !step(t, &m, endMsg{err: errors.New("the host went away")}) {
			t.Fatal("the stream's end left the replay unpainted")
		}
		if v := plainView(m); !strings.Contains(v, "line 99") {
			t.Fatalf("the frame the stream's end left:\n%s", v)
		}
	})
}

// TestAReplayedParagraphPaintsOnItsCadence is the one-paragraph bound: 2,400
// chunks of one growing paragraph during a replay, with two forced paints, are
// painted at most chunks/256 + forced + 1 times — each paint renders the whole
// paragraph (SF-105), so the count is the cost.
func TestAReplayedParagraphPaintsOnItsCadence(t *testing.T) {
	const chunks, forced = 2400, 2
	m, _ := loadedStub(t, nil)
	before := m.main.work.paints
	m = feed(t, m, replayEvent(agent.ReplayStart))
	for i := 1; i <= chunks; i++ {
		m = feed(t, m, replayLine(i))
		if i == 700 || i == 1500 {
			m = applyMsg(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
		}
	}
	m = feed(t, m, replayEvent(agent.ReplayEnd))
	paints := m.main.work.paints - before
	t.Logf("%d chunks of one paragraph replayed: %d paints, %d markdown bytes read", chunks, paints, m.main.work.mdBytes)
	if limit := chunks/replayPaintEvery + forced + 1; paints > limit {
		t.Fatalf("%d paints, the bound is %d", paints, limit)
	}
	if v := plainView(m); !strings.Contains(v, "line "+strconv.Itoa(chunks)) || !strings.Contains(v, restoredNote) {
		t.Fatalf("the restored frame:\n%s", v)
	}
}

// TestProseRenderWorkIsLinear: for many-paragraph content, live and replayed,
// the markdown bytes the renderer reads at 4,800 chunks are at most 4.5 times
// those at 1,200 — linear, where rendering every chunk's whole text is 16
// times.
func TestProseRenderWorkIsLinear(t *testing.T) {
	read := func(t *testing.T, replay bool, chunks int) int {
		t.Helper()
		var m Model
		if replay {
			m, _ = loadedStub(t, nil)
			m = feed(t, m, replayEvent(agent.ReplayStart))
		} else {
			isolateSkillsHome(t)
			m = startStub(t, NewStub(), t.TempDir(), 100, 30)
		}
		before := m.main.work.mdBytes
		for i := range chunks {
			ev := agent.Event{Type: agent.EventText, Text: proseChunk(i)}
			if replay {
				ev = replayed(ev)
			}
			m = feed(t, m, ev)
		}
		if replay {
			m = feed(t, m, replayEvent(agent.ReplayEnd))
		}
		return m.main.work.mdBytes - before
	}
	for _, replay := range []bool{false, true} {
		small, large := read(t, replay, 1200), read(t, replay, 4800)
		t.Logf("replay %v: %d bytes read at 1,200 chunks, %d at 4,800 (×%.2f)", replay, small, large, float64(large)/float64(small))
		if float64(large) > 4.5*float64(small) {
			t.Errorf("replay %v: %d bytes at 4,800 chunks against %d at 1,200: more than 4.5 times", replay, large, small)
		}
	}
}

// TestRowsAssembledPerChunkDoNotGrowWithTheTranscript: a chunk into a new
// reply assembles the same row spans after 20 closed replies as after 400 —
// the spans from the first entry that changed, never the transcript's.
func TestRowsAssembledPerChunkDoNotGrowWithTheTranscript(t *testing.T) {
	const chunks = 50
	perChunk := func(t *testing.T, replies int) float64 {
		t.Helper()
		isolateSkillsHome(t)
		m := startStub(t, NewStub(), t.TempDir(), 100, 30)
		body := strings.Repeat("some words in a sentence that wraps across the row. ", 4)
		for range replies {
			m = feed(t, m,
				agent.Event{Type: agent.EventText, Text: body},
				agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
		}
		before := m.main.work.spans
		for range chunks {
			m = feed(t, m, agent.Event{Type: agent.EventText, Text: "w "})
		}
		return float64(m.main.work.spans-before) / chunks
	}
	short, long := perChunk(t, 20), perChunk(t, 400)
	t.Logf("row spans assembled per chunk: %.2f after 20 replies, %.2f after 400", short, long)
	if long != short || long > 2 {
		t.Fatalf("row spans assembled per chunk: %.2f after 20 replies, %.2f after 400", short, long)
	}
}
