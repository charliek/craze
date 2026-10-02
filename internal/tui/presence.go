package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// Presence (plan 032 §3.14, SF-64): how many clients are attached to the
// session, this one included, as an accent chip in status row 2 — `N
// attached`, shown only when N is 2 or more, so a session nobody else has
// open looks exactly as it always did.
//
// The count reaches the model two ways:
//
//   - over the socket, as the backend's stream's Presence item
//     (backend.ItemPresence, the host's presence notification): a presenceMsg,
//     placed by the command gate as any stream item is, and kept for the
//     backend generation it came in (Model.attached). A restore, the
//     stream's end and another backend's adoption clear it, and so does the
//     stream's own Presence of 0 when its subscription ends — a reset, a
//     reconnect, a fall behind — so the chip hides until the next count;
//   - in process, from the TUI-hosted server that serves this TUI's session
//     to other clients (Config.LocalPresence): its count includes this TUI's
//     own seat (control.Options.LocalClient), and arrives on a latest-value
//     channel a command of the model's own reads (presenceCmd), since the
//     in-process backend's stream carries only the engine's events. It is
//     the server's for its whole life — a picker's new engine behind it
//     changes nothing — and is kept in Model.hostAttached.

// presenceMsg is the backend stream's Presence item (backend.ItemPresence):
// n clients attached, 0 for not known. gen is the stream generation it was
// read in, bgen the backend generation (waitEvent).
type presenceMsg struct {
	n    int
	gen  uint64
	bgen uint64
}

func (m presenceMsg) backendGen() uint64 { return m.bgen }

// localPresenceMsg is a count the host TUI's own server gave
// (Config.LocalPresence), its own seat included.
type localPresenceMsg struct{ n int }

// presenceCmd reads the next count from the host TUI's own server
// (Config.LocalPresence): nil with none to read, and no message once the
// channel is closed (the server has closed). The model arms it again after
// every count it applies, so exactly one read is ever waiting.
func presenceCmd(ch <-chan int) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		n, ok := <-ch
		if !ok {
			return nil
		}
		return localPresenceMsg{n: n}
	}
}

// attachedNow is how many clients are attached to the session the model
// shows, this one included, as far as it knows: its own server's count in
// the host TUI, its stream's anywhere else; 0 when it does not know.
func (m Model) attachedNow() int {
	if m.localPresence != nil {
		return m.hostAttached
	}
	return m.attached
}

// presenceChip is status row 2's `N attached`, or "" below 2.
func (m Model) presenceChip() string {
	n := m.attachedNow()
	if n < 2 {
		return ""
	}
	return fmt.Sprintf("%d attached", n)
}
