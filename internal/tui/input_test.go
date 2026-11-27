package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/creack/pty"

	"github.com/charliek/craze/internal/agent"
)

// The adapter reserves room for the largest literal expansion in the
// 4 KiB buffer supplied by Bubble Tea’s scanner.
const terminalReadRoom = 4096 / 5

// terminalEvents sends bytes through raw mode and Bubble Tea's actual reader.
// The terminating F12 is separate from the captured sequence.
func terminalEvents(t *testing.T, text string) []keyInput {
	t.Helper()
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no PTY: %v", err)
	}
	defer func() { _ = master.Close(); _ = tty.Close() }()
	rec := &inputRecorder{ready: make(chan struct{})}
	p := tea.NewProgram(rec, tea.WithInput(newGraphemeInput(tty)), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	defer func() { p.Kill() }()
	select {
	case <-rec.ready:
	case err := <-done:
		t.Fatalf("starting PTY: %v", err)
	case <-time.After(keyBound):
		t.Fatal("PTY startup timeout")
	}
	if _, err = master.WriteString(text + "\x1b[24~"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(keyBound):
		t.Fatal("PTY input timeout")
	}
	return rec.events
}

type inputRecorder struct {
	ready  chan struct{}
	events []keyInput
}

func (r *inputRecorder) Init() tea.Cmd  { close(r.ready); return nil }
func (r *inputRecorder) View() tea.View { return tea.View{} }
func (r *inputRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.Code == tea.KeyF12 {
			return r, tea.Quit
		}
		r.events = append(r.events, msg)
	case tea.PasteMsg:
		r.events = append(r.events, msg)
	}
	return r, nil
}

func TestShiftEnterReachesTheComposerAsANewline(t *testing.T) {
	for _, seq := range []string{"\x1b[27;2;13~", "\x1b[13;2u"} {
		m := sized(t)
		for _, k := range terminalEvents(t, "hello"+seq+"world") {
			m, _ = press(m, k)
		}
		if got := m.input.Value(); got != "hello\nworld" || turnsStarted(stubOf(t, m)) != 0 {
			t.Fatalf("%q produced draft %q, status %s", seq, got, m.status)
		}
	}
}

func TestTerminalGraphemesCannotAnswerPermission(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
	for _, k := range terminalEvents(t, keycapOne+decomposed+pasteStart+"1"+pasteEnd) {
		m, _ = press(m, k)
	}
	if headAsk(m) != "perm-1" || len(stub.Asks().Resolved()) != 0 {
		t.Fatal("composed or pasted text answered the permission card")
	}
	keys := terminalEvents(t, "1")
	if len(keys) != 1 {
		t.Fatalf("ordinary key: %#v", keys)
	}
	m, _ = press(m, keys[0])
	if headAsk(m) != "" || len(stub.Asks().Resolved()) != 1 {
		t.Fatal("ordinary 1 did not answer the permission card")
	}

	m = sized(t)
	for _, k := range terminalEvents(t, keycapOne+decomposed) {
		m, _ = press(m, k)
	}
	if got := m.input.Value(); got != keycapOne+decomposed {
		t.Fatalf("composed draft %q", got)
	}
}

// A single queued terminal write can straddle multiple reader buffers. Move
// the ASCII base and the first bytes of its mark across that boundary.
func TestTerminalGraphemeAtReadBoundary(t *testing.T) {
	for before := 0; before < 3; before++ {
		text := strings.Repeat("x", terminalReadRoom-1-before) + keycapOne
		var got strings.Builder
		for _, event := range terminalEvents(t, text) {
			switch k := event.(type) {
			case tea.KeyPressMsg:
				if shortcut(k) == "1" {
					t.Fatalf("offset %d dispatched the keycap's shortcut", before)
				}
				got.WriteString(k.Text)
			case tea.PasteMsg:
				got.WriteString(k.Content)
			}
		}
		if got.String() != text {
			t.Fatalf("offset %d lost literal input", before)
		}
	}
}

// Focus notifications put the beginning of the OSC in one terminal read and
// its BEL terminator in the next. Only the following keycap is keyboard input.
func TestTerminalKeycapAfterSplitColorReport(t *testing.T) {
	const focusIn = "\x1b[I"
	prefix := strings.Repeat(focusIn, terminalReadRoom/len(focusIn)-1)
	got := terminalEvents(t, prefix+"\x1b]11;rgb:1111/2222/3333\a"+keycapOne)
	if len(got) != 1 || got[0] != (tea.PasteMsg{Content: keycapOne}) {
		t.Fatalf("keycap after color report: %#v", got)
	}
}

func TestTerminalBracketedPasteRemainsAtomic(t *testing.T) {
	text := "up\n" + keycapOne + " " + decomposed
	got := terminalEvents(t, pasteStart+text+pasteEnd+pasteStart+pasteEnd+"\r\n")
	if len(got) != 4 || got[0] != (tea.PasteMsg{Content: text}) || got[1] != (tea.PasteMsg{}) || shortcut(got[2]) != "enter" || shortcut(got[3]) != "ctrl+j" {
		t.Fatalf("paste, empty paste, Enter, Ctrl+J: %#v", got)
	}
}

// Terminal reports, mouse coordinates and bracketed paste can contain the
// same bytes as a keycap. Once a report ends, the next keycap is literal again.
func TestGraphemeInputPreservesControlSequencesAcrossReads(t *testing.T) {
	for _, report := range []string{
		pasteStart + keycapOne + pasteEnd,
		"\x1b]11;rgb:1111/2222/3333\a",
		"\x1b]0;" + keycapOne + "\x1b\\",
		"\x1bP1;2q" + keycapOne + "\x1b\\",
		"\x1b[M" + decomposed,
	} {
		for split := range len(report) + 1 {
			in := newGraphemeInput(nil)
			got := in.literalClusters(nil, []byte(report[:split]))
			got = in.literalClusters(got, []byte(report[split:]+keycapOne))
			if want := report + pasteStart + keycapOne + pasteEnd; string(got) != want {
				t.Fatalf("report %q split at %d: got %q, want %q", report, split, got, want)
			}
		}
	}
}
