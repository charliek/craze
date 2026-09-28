package tui

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
)

// Two TUIs on one host (plan 027 §7 A2, A3; §5's C29 row): two models, each
// over its own remote.Session to one host, each driven by its own pump — the
// pump reading the session's stream (pumpFor), not RunFrameScript, whose runs
// serialise on one process-wide HOME (CodeRabbit 16). What either client does
// reaches the other through the host, and the two end on the same transcript.

// servedHost is a host over sess — started — on a short socket path, as
// newAttachHost's is over a Stub: the session is built NoPrimary by the
// caller, since nothing in the host's process reads its primary.
func servedHost(t *testing.T, sess agent.Session) *attachHost {
	t.Helper()
	h := &attachHost{}
	if stub, ok := sess.(*Stub); ok {
		h.stub = stub
	}
	var err error
	h.eng, err = engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.eng.Close() })
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "czt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h.path = filepath.Join(dir, "s")
	srv := control.New(control.Options{Workspace: "/work"})
	srv.SetEngine(h.eng)
	l, err := net.Listen("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("server close: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return h
}

// remoteClient is a TUI over a session of its own to h — the host TUI over a
// remote backend, as the socket goldens run it — started under its own pump,
// its first restore applied and its start answered. The session is registered
// as h's (registerSocketHost), which is what gives it a pump of its own and
// lets the pump ask the host's engine for its head.
func remoteClient(t *testing.T, h *attachHost, yolo bool) Model {
	t.Helper()
	s := attachSession(t, h, "")
	t.Cleanup(registerSocketHost(s, h.eng))
	m := New(Config{Backend: s, Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: yolo})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	m = pumpCmd(t, m, m.startCmd())
	return pumpUntil(t, m, func(m Model) bool { return m.started && m.shared != nil && m.shared.Incarnation() != "" })
}

// sendFrom types text into m's composer key by key, as a terminal does, and
// presses Enter.
func sendFrom(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m = pumpKey(t, m, runeKey(r))
	}
	return pumpKey(t, m, enter())
}

// drawnRows is m's main transcript as it draws it — each row's kind and text
// (mainRows without its local mark: the sender's own prompt is the optimistic
// row it drew, where the other client draws the one the started carries,
// plan 024 X26).
func drawnRows(m Model) []string {
	var out []string
	for _, r := range mainRows(m) {
		out = append(out, strings.TrimPrefix(r, "local "))
	}
	return out
}

// rowCount is how many of m's drawn rows are exactly row.
func rowCount(m Model, row string) int {
	n := 0
	for _, r := range drawnRows(m) {
		if r == row {
			n++
		}
	}
	return n
}

// sameTranscripts fails t unless a and b draw the same main transcript, row
// for row.
func sameTranscripts(t *testing.T, a, b Model) {
	t.Helper()
	if ra, rb := drawnRows(a), drawnRows(b); !slices.Equal(ra, rb) {
		t.Fatalf("the two clients' transcripts differ\n--- A\n%s\n--- B\n%s", strings.Join(mainRows(a), "\n"), strings.Join(mainRows(b), "\n"))
	}
}

// TestAPromptFromEitherClientAppearsInBoth (A2): a prompt typed in either
// client is a turn on the host, and its row and its reply show in both
// transcripts — once each: the sender draws its own row and skips the
// started it caused, the other draws the row from that started — and the two
// transcripts end the same, row for row.
func TestAPromptFromEitherClientAppearsInBoth(t *testing.T) {
	isolateSkillsHome(t)
	h := servedHost(t, NewStubNoPrimary())
	a, b := remoteClient(t, h, true), remoteClient(t, h, true)

	a = sendFrom(t, a, "from a")
	a = pumpUntil(t, a, func(m Model) bool { return isIdle(m) && viewHas("echo: from a")(m) })
	b = pumpUntil(t, b, func(m Model) bool { return isIdle(m) && viewHas("echo: from a")(m) })

	b = sendFrom(t, b, "from b")
	b = pumpUntil(t, b, func(m Model) bool { return isIdle(m) && viewHas("follow-up: from b")(m) })
	a = pumpUntil(t, a, func(m Model) bool { return isIdle(m) && viewHas("follow-up: from b")(m) })

	a, b = pumpSettled(t, a), pumpSettled(t, b)
	for name, m := range map[string]Model{"A": a, "B": b} {
		for _, row := range []string{"user:from a", "user:from b"} {
			if n := rowCount(m, row); n != 1 {
				t.Fatalf("client %s shows %q %d times, want once:\n%s", name, row, n, strings.Join(mainRows(m), "\n"))
			}
		}
		for _, row := range []string{"assistant:echo: from a", "assistant:follow-up: from b"} {
			if n := rowCount(m, row); n != 1 {
				t.Fatalf("client %s shows %q %d times, want once:\n%s", name, row, n, strings.Join(mainRows(m), "\n"))
			}
		}
	}
	sameTranscripts(t, a, b)
	if got := h.stub.Prompts(); !slices.Equal(got, []string{"from a", "from b"}) {
		t.Fatalf("the host was prompted %q", got)
	}
}

// TestAnAskAnsweredInOneClosesInTheOther (A3): an ask both clients show a
// card for, answered in one, closes in the other — which writes the rows
// applyAskEnded writes for an answer it did not give, the ones the answering
// client wrote for its own: for a question on the Stub, the answer notes; for
// a permission over the fake agent (A3's live permissions are SF-30's), none.
// The two transcripts end the same, row for row.
func TestAnAskAnsweredInOneClosesInTheOther(t *testing.T) {
	t.Run("a question on the Stub", func(t *testing.T) {
		isolateSkillsHome(t)
		h := servedHost(t, NewStubNoPrimary())
		a, b := remoteClient(t, h, true), remoteClient(t, h, true)

		h.stub.Emit(agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
		a = pumpUntil(t, a, Model.cardOpen)
		b = pumpUntil(t, b, Model.cardOpen)

		for _, k := range []tea.KeyMsg{runeKey('1'), runeKey(' '), runeKey('3'), enter()} {
			a = pumpKey(t, a, k)
		}
		const one, many = "? Pick one → A", "? Pick any → X, Z"
		a = pumpUntil(t, a, func(m Model) bool { return !m.cardOpen() && viewHas(one)(m) && viewHas(many)(m) })
		b = pumpUntil(t, b, func(m Model) bool { return !m.cardOpen() && viewHas(one)(m) && viewHas(many)(m) })
		a, b = pumpDrained(t, a), pumpDrained(t, b)
		calls := h.stub.Calls()
		if len(calls) != 1 || calls[0].ID != "ask-1" || !slices.Equal(calls[0].Answers["q2"], []string{"opt-x", "opt-z"}) {
			t.Fatalf("the host's ask was answered %+v", calls)
		}
		for name, m := range map[string]Model{"A": a, "B": b} {
			for _, note := range []string{one, many} {
				if n := strings.Count(plainView(m), note); n != 1 {
					t.Fatalf("client %s shows %q %d times, want once:\n%s", name, note, n, plainView(m))
				}
			}
		}
		sameTranscripts(t, a, b)
	})

	t.Run("a permission over the fake agent", func(t *testing.T) {
		isolateSkillsHome(t)
		sess := agent.New(agent.Options{
			Binary:      buildFakeAgent(t),
			ExtraArgs:   []string{"-script=permission"},
			Workspace:   frameWorkspace(t),
			Interactive: true,
			Stderr:      io.Discard,
			NoPrimary:   true,
		})
		h := servedHost(t, sess)
		a, b := remoteClient(t, h, false), remoteClient(t, h, false)

		a = sendFrom(t, a, "go")
		a = pumpUntil(t, a, Model.cardOpen)
		b = pumpUntil(t, b, Model.cardOpen)

		b = pumpKey(t, b, runeKey('A'))
		b = pumpUntil(t, b, func(m Model) bool { return !m.cardOpen() && isIdle(m) && viewHas("decision:opt-always")(m) })
		a = pumpUntil(t, a, func(m Model) bool { return !m.cardOpen() && isIdle(m) && viewHas("decision:opt-always")(m) })
		a, b = pumpSettled(t, a), pumpSettled(t, b)
		sameTranscripts(t, a, b)
		for name, m := range map[string]Model{"A": a, "B": b} {
			if strings.Contains(plainView(m), "Allow always") || slices.ContainsFunc(mainRows(m), func(r string) bool { return strings.HasPrefix(r, "local ") && strings.Contains(r, "lways") }) {
				t.Fatalf("client %s wrote a row for a permission answer:\n%s", name, plainView(m))
			}
		}
	})
}
