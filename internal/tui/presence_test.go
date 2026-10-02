package tui

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The `N attached` chip (plan 032 §3.14, SF-64; presence.go).

// rowTwo is status row 2 as it draws, plain.
func rowTwo(m Model) string { return statusText(m.statusRow2(m.lay)) }

// showsAttached says status row 2 shows `n attached`.
func showsAttached(m Model, n string) bool { return strings.Contains(rowTwo(m), n+" attached") }

// TestThePresenceChipShowsFromTwo: the stream's count shows as `N attached`
// from 2 — never at 1, this client alone, nor at 0, not known — and it is
// the current backend's: a count another backend's stream left behind is
// dropped, and a restore, a count of 0 and the stream's end each clear it.
func TestThePresenceChipShowsFromTwo(t *testing.T) {
	m := sized(t)
	if strings.Contains(rowTwo(m), "attached") {
		t.Fatalf("a fresh model's row 2: %q", rowTwo(m))
	}
	m = deliver(t, m, presenceMsg{n: 1})
	if strings.Contains(rowTwo(m), "attached") {
		t.Fatalf("one attached client — this one — shows a chip: %q", rowTwo(m))
	}
	m = deliver(t, m, presenceMsg{n: 2})
	if !showsAttached(m, "2") {
		t.Fatalf("two attached clients show no chip: %q", rowTwo(m))
	}
	m = deliver(t, m, presenceMsg{n: 5, bgen: m.bgen + 1})
	if !showsAttached(m, "2") {
		t.Fatalf("another backend's count moved the chip: %q", rowTwo(m))
	}
	m = deliver(t, m, presenceMsg{n: 0})
	if strings.Contains(rowTwo(m), "attached") {
		t.Fatalf("a count of 0 — not known — left the chip: %q", rowTwo(m))
	}
	m = deliver(t, m, presenceMsg{n: 3})
	m = deliver(t, m, restoreOf(m, snapshotFolded(t, "inc-presence"), 1))
	if strings.Contains(rowTwo(m), "attached") || m.attached != 0 {
		t.Fatalf("a restore left the chip: %q", rowTwo(m))
	}
	m = deliver(t, m, presenceMsg{n: 3})
	m = deliver(t, m, endMsg{})
	if m.attached != 0 {
		t.Fatalf("the stream's end left the count %d", m.attached)
	}
}

// TestThePresenceChipDropsFirst: on a row too narrow for everything, `N
// attached` goes before anything else — the mode's hint among them. The
// negative control: one cell wider, the chip is there.
func TestThePresenceChipDropsFirst(t *testing.T) {
	m := sized(t)
	m = deliver(t, m, presenceMsg{n: 2})
	m.width = 200
	full := strings.TrimRight(rowTwo(m), " ")
	if !strings.Contains(full, "2 attached") || !strings.Contains(full, modeHint) {
		t.Fatalf("the premise: the wide row is %q", full)
	}
	m.width = lipgloss.Width(full)
	if !showsAttached(m, "2") {
		t.Fatalf("the row as wide as it needs drops the chip: %q", rowTwo(m))
	}
	m.width--
	row := rowTwo(m)
	if strings.Contains(row, "attached") || !strings.Contains(row, modeHint) {
		t.Fatalf("one cell narrower: %q, want the chip gone and the hint kept", row)
	}
}

// TestTheReaderDeliversPresence: the reader hands the stream's Presence up as
// a presenceMsg, with its count and generation and the backend generation it
// was read under; and the command gate places it as a stream item, so the
// next read is armed after it.
func TestTheReaderDeliversPresence(t *testing.T) {
	b := &fakeBackend{read: func(context.Context) (backend.Item, error) {
		return backend.Item{Kind: backend.ItemPresence, Attached: 2, Gen: 3}, nil
	}}
	if got, want := waitEvent(b, 7)(), (presenceMsg{n: 2, gen: 3, bgen: 7}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the reader answered %#v, want %#v", got, want)
	}
	if !fromStream(presenceMsg{}) {
		t.Fatal("a presence is not placed as a stream item")
	}
}

// TestTheHostTUIReadsItsOwnCount: the host TUI's count comes from its own
// server (Config.LocalPresence), one read at a time: each count is applied
// and the next read armed, a closed channel ends the reads, and the chip
// shows from 2. The negative control: with no channel nothing is read.
func TestTheHostTUIReadsItsOwnCount(t *testing.T) {
	if presenceCmd(nil) != nil {
		t.Fatal("no channel, and still a read")
	}
	ch := make(chan int, 1)
	isolateSkillsHome(t)
	m := New(Config{Session: NewStub(), Theme: "tokyo-night", Workspace: t.TempDir(), LocalPresence: ch})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	ch <- 2
	msg := runWatched(t, presenceCmd(m.localPresence))
	if msg != (localPresenceMsg{n: 2}) {
		t.Fatalf("the read answered %#v", msg)
	}
	tm, cmd := m.Update(msg)
	m = tm.(Model)
	if !showsAttached(m, "2") {
		t.Fatalf("the host TUI's count shows no chip: %q", rowTwo(m))
	}
	if cmd == nil {
		t.Fatal("no next read armed")
	}
	close(ch)
	if msg := runWatched(t, presenceCmd(m.localPresence)); msg != nil {
		t.Fatalf("a closed channel answered %#v", msg)
	}
}

// listenForTUI is the host TUI's half of the TUI-hosted server, as
// internal/cli's bindControl builds it: srv's attachments listener keeping a
// one-slot latest-value channel the host TUI reads (Config.LocalPresence).
func listenForTUI(srv *control.Server) (<-chan int, func()) {
	ch := make(chan int, 1)
	remove := srv.AddAttachmentsListener(func(n int) {
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- n:
		default:
		}
	})
	return ch, func() { remove(); close(ch) }
}

// TestAHostTUIAndAnAttachBothShowTwoAttached (A15, R3-1): an in-process host
// TUI — its session served by the TUI-hosted server, which counts the host
// TUI's own seat (control.Options.LocalClient) — and one `craze attach`-style
// client both show `2 attached`, and the attach's own sessions.list row says
// attached: 2. When the attach leaves, the host TUI's chip goes. The negative
// control: before the attach, the host TUI alone shows none.
func TestAHostTUIAndAnAttachBothShowTwoAttached(t *testing.T) {
	isolateSkillsHome(t)
	dir, err := os.MkdirTemp("/tmp", "czp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	srv := control.New(control.Options{Workspace: "/work", Presence: true, LocalClient: true})
	l, err := net.Listen("unix", path)
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
		<-served
	})
	counts, stop := listenForTUI(srv)

	host := New(Config{Session: NewStub(), Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: true,
		LocalPresence: counts, OnEngine: srv.SetEngine})
	tm, _ := host.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	host = tm.(Model)
	host = pumpCmd(t, host, host.startCmd())
	// Registered after the host's pump, so it runs before the pump joins its
	// goroutines: the host TUI's read of its count ends with the channel.
	t.Cleanup(stop)
	host = pumpCmd(t, host, presenceCmd(host.localPresence))
	host = pumpUntil(t, host, func(m Model) bool { return m.started })
	if strings.Contains(rowTwo(host), "attached") {
		t.Fatalf("the host TUI alone shows a chip: %q", rowTwo(host))
	}

	eng := engineOf(t, host)
	s := attachSession(t, &attachHost{eng: eng, path: path}, "")
	t.Cleanup(registerSocketHost(s, eng))
	view := New(Config{Backend: s, Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: true})
	tm, _ = view.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view = tm.(Model)
	view = pumpCmd(t, view, view.startCmd())
	pumpUntil(t, view, func(m Model) bool { return showsAttached(m, "2") })
	host = pumpUntil(t, host, func(m Model) bool { return showsAttached(m, "2") })

	var list protocol.SessionsListResult
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	if err := s.Client().Call(ctx, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Attached != 2 || !list.Sessions[0].Capabilities.Presence {
		t.Fatalf("the attach's row: %+v", list.Sessions)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	host = pumpUntil(t, host, func(m Model) bool { return m.hostAttached == 1 })
	if strings.Contains(rowTwo(host), "attached") {
		t.Fatalf("the host TUI alone again still shows a chip: %q", rowTwo(host))
	}
}

// TestTheChipClearsWhileTheConnectionIsDown (review r29 item 4): a client
// showing `2 attached` loses its connection (EOF) while its redial is held
// behind a barrier: the chip goes at once — the count died with the
// connection — and comes back once the redial goes through and the client is
// attached again. The negative control is the premise: the redial had begun
// and was still held when the chip was gone, so it was not the reconnect's
// re-attach that cleared it.
func TestTheChipClearsWhileTheConnectionIsDown(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStubNoPrimary()
	h := newAttachHostWith(t, stub, stub, true, control.Options{Workspace: "/work", Presence: true})
	var mu sync.Mutex
	var first net.Conn
	redialing, release := make(chan struct{}), make(chan struct{})
	var redialOnce, releaseOnce sync.Once
	dial := func(ctx context.Context, path string) (net.Conn, error) {
		mu.Lock()
		redial := first != nil
		mu.Unlock()
		if redial {
			redialOnce.Do(func() { close(redialing) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		var d net.Dialer
		nc, err := d.DialContext(ctx, "unix", path)
		if err == nil && !redial {
			mu.Lock()
			first = nc
			mu.Unlock()
		}
		return nc, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	s, err := remote.DialSession(ctx, h.path, remote.SessionOptions{
		Client: remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "tui_presence"}, Dial: dial},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Registered after the session's close, so it runs first: a failure
	// with the redial held lets it go.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	t.Cleanup(registerSocketHost(s, h.eng))
	m := New(Config{Backend: s, Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	m = pumpCmd(t, m, m.startCmd())
	remoteClient(t, h, true)
	m = pumpUntil(t, m, func(m Model) bool { return showsAttached(m, "2") })

	mu.Lock()
	nc := first
	mu.Unlock()
	_ = nc.Close()
	m = pumpUntil(t, m, func(m Model) bool { return !strings.Contains(rowTwo(m), "attached") })
	select {
	case <-redialing:
	case <-time.After(pumpWatchdog):
		t.Fatal("the premise: the client never began to redial")
	}
	if strings.Contains(rowTwo(m), "attached") || m.attached != 0 {
		t.Fatalf("with the redial held: %q (attached %d)", rowTwo(m), m.attached)
	}

	releaseOnce.Do(func() { close(release) })
	pumpUntil(t, m, func(m Model) bool { return showsAttached(m, "2") })
}

// TestAnOlderHostShowsNoChip (§3.15): two clients of a host without presence
// show no chip once each has folded an event sent more than a presence
// interval (500 ms) after both attached — by then a presence host owes each
// its count, and a writer hands an owed count over ahead of any line queued
// after it — while two clients of a presence host, the same way, both show
// `2 attached`.
func TestAnOlderHostShowsNoChip(t *testing.T) {
	isolateSkillsHome(t)
	for _, tc := range []struct {
		name     string
		presence bool
	}{{"an older host", false}, {"a presence host", true}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := NewStubNoPrimary()
			h := newAttachHostWith(t, stub, stub, true, control.Options{Workspace: "/work", Presence: tc.presence})
			a := remoteClient(t, h, true)
			b := remoteClient(t, h, true)
			time.Sleep(600 * time.Millisecond)
			stub.Emit(agent.Event{Type: agent.EventText, Text: "after both attached"})
			folded := func(m Model) bool { return strings.Contains(strings.Join(mainRows(m), "\n"), "after both attached") }
			for _, m := range []Model{a, b} {
				m = pumpUntil(t, m, folded)
				if got := showsAttached(m, "2"); got != tc.presence {
					t.Fatalf("row 2 is %q (attached %d)", rowTwo(m), m.attached)
				}
				if !tc.presence && m.attached != 0 {
					t.Fatalf("a client of an older host holds a count of %d", m.attached)
				}
			}
		})
	}
}

// TestFrameGoldenPresence100x30 (plan 032 §3.14, §3.17): the `N attached`
// chip at the end of status row 2 — a TUI-hosted session with one other
// client attached. In process it is the host TUI, reading its own server's
// count (Config.LocalPresence, here a channel holding 2); over the socket it
// is that host's other client, told 2 on its stream by a host that counts the
// host TUI's seat (frame_socket_test.go). Both draw the same frame. Spinners
// are frozen at ✳.
func TestFrameGoldenPresence100x30(t *testing.T) {
	isolateSkillsHome(t)
	var counts []chan int
	t.Cleanup(func() {
		for _, ch := range counts {
			close(ch)
		}
	})
	got, _, err := runFrameModes(t, func() Config {
		ch := make(chan int, 1)
		ch <- 2
		counts = append(counts, ch)
		return Config{
			Session:       frameStub(),
			Theme:         "tokyo-night",
			Workspace:     frameWorkspace(t),
			Model:         "grok",
			Yolo:          true,
			LocalPresence: ch,
		}
	}, 100, 30, "<wait:idle><wait:text:2 attached>", FrameOpts{Timeout: 10 * time.Second, Freeze: true})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertGolden(t, "presence-100x30", 100, 30, got)
	if !strings.Contains(got, "◆ agent · shift+tab · ▸▸ bypass permissions on · 2 attached") {
		t.Fatalf("row 2 has no chip at its end:\n%s", got)
	}
}
