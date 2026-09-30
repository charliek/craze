package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
)

// Saved sessions from the list (plan 030 §3.12, C12): enter on a saved row
// resumes it in place — Sessions.Open of the row's saved ref, whose backend is
// switched to as a load — or the holder's session when a host serves it
// after all; a row the launcher cannot run is refused on the hint line in the
// list's own words; ctrl+x does nothing on a saved row. Each schedule runs
// through the switch rig (switch_test.go), one message at a time.

// savedLane is a saved session's index row for lane b's session, titled
// title, of provider grok, in b's workspace.
func savedLane(b *laneBackend, title string) sessions.Row {
	return sessions.Row{SessionID: "p-" + b.info.CrazeSessionID, Provider: "grok", CWD: b.info.Workspace,
		Title: title, CrazeID: b.info.CrazeSessionID, UpdatedAt: sessNow.Add(-time.Hour)}
}

// savedKey is the list's line of a saved row with craze id id.
func savedKey(id string) sessKey { return sessKey{id: "\x00saved:" + id} }

// openSavedList opens the list over running and saved, and expands the
// saved group (enter on its line).
func (r *switchRig) openSavedList(running []roster.Row, saved ...sessions.Row) {
	r.t.Helper()
	r.send(tea.KeyMsg{Type: tea.KeyLeft})
	r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: running, Saved: saved}})
	r.m = selectKey(r.t, r.m, sessSavedLine)
	r.send(enter())
	if !r.m.sessList.savedOpen {
		r.t.Fatalf("enter on the saved line did not expand it:\n%s", plainView(r.m))
	}
}

// TestSessionsEnterResumesASavedSession (§3.12, AC8): enter on a saved row
// asks Sessions.Open for its saved ref — the index row itself, no host — the
// hint line saying `resuming <title>…` meanwhile, and the answer is switched
// to as a load: the model is replaying (`restoring…`) from the switch until
// the replay's end, whichever says it first, the stream's restore or its
// event. A saved row that is running after all is the holder's session: its
// first restore, a session up with its turns folded, brings it up at once.
func TestSessionsEnterResumesASavedSession(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "loaded"
		if held {
			name = "held"
		}
		t.Run(name, func(t *testing.T) {
			a, s := newLane(t, "a", "alpha"), newLane(t, "s", "sierra")
			r, fs := laneModel(t, a, map[string][]*laneBackend{})
			fs.open = func(ref roster.Ref) (backend.Backend, error) {
				if ref.Saved == nil || ref.Saved.CrazeID != "s" {
					return nil, errors.New("not the saved row")
				}
				return s, nil
			}
			row := savedLane(s, "the saved one")
			r.openSavedList([]roster.Row{laneRow(a, "session a", time.Minute)}, row)
			r.m = selectKey(t, r.m, savedKey("s"))
			if !strings.Contains(plainView(r.m), "enter resume") {
				t.Fatalf("the saved row's hint:\n%s", plainView(r.m))
			}
			before := len(r.opens)
			r.send(enter())
			if len(r.opens) != before+1 || !r.m.sessList.open || !strings.Contains(plainView(r.m), "resuming the saved one…") {
				t.Fatalf("enter on the saved row (%d opens asked):\n%s", len(r.opens)-before, plainView(r.m))
			}
			r.send(r.pop(&r.opens, "dial"))
			got := fs.opened()
			switch {
			case len(got) != 1 || got[0].Saved == nil || *got[0].Saved != row || got[0].Host != (roster.Host{}):
				t.Fatalf("Open was asked for %+v, want the saved row alone", got)
			case r.m.eng != s || r.m.sessList.open:
				t.Fatalf("the resume did not switch: backend s %v, list open %v", r.m.eng == s, r.m.sessList.open)
			case !r.m.replaying || r.m.sessionReady() || !strings.Contains(plainView(r.m), "restoring…"):
				t.Fatalf("a resumed session is not restoring: replaying %v, ready %v:\n%s", r.m.replaying, r.m.sessionReady(), plainView(r.m))
			case r.m.cwd != s.info.Workspace:
				t.Fatalf("the workspace is %q, want the saved session's %q", r.m.cwd, s.info.Workspace)
			}
			if held {
				// Up already on its host: its first restore folded its turn.
				r.up(s, seqd(1, agent.Event{Type: agent.EventText, Text: "the holder's reply"})...)
				return
			}
			// The start answers first, and the replay is still running.
			r.send(r.pop(&r.starts, "start"))
			r.stream(s, s.restoreItem(t, 1, seqd(1, agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})...))
			if !r.m.replaying || r.m.sessionReady() || !strings.Contains(plainView(r.m), "restoring…") {
				t.Fatalf("mid-replay: replaying %v, ready %v:\n%s", r.m.replaying, r.m.sessionReady(), plainView(r.m))
			}
			r.stream(s, backend.Item{Kind: backend.ItemEvent, Gen: 1,
				Event: seqd(2, agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}})[0]})
			if r.m.replaying || !r.m.sessionReady() {
				t.Fatalf("after the replay's end: replaying %v, ready %v", r.m.replaying, r.m.sessionReady())
			}
		})
	}
}

// TestSessionsASavedRowItCannotRun (§3.12): an Open of a saved row that
// answers an error — the launcher's refusal of a row whose workspace is gone
// or whose provider this craze cannot resume — leaves the list up, the
// session behind it attached, and the refusal on the hint line as
// `could not resume <title>: <why>` (the launcher's "craze: " dropped).
func TestSessionsASavedRowItCannotRun(t *testing.T) {
	for _, why := range []string{
		"that session ran in /gone/sierra, which is no longer a directory",
		`that session's provider "zed" is not one this craze can resume`,
	} {
		a, s := newLane(t, "a", "alpha"), newLane(t, "s", "sierra")
		r, fs := laneModel(t, a, map[string][]*laneBackend{})
		fs.open = func(roster.Ref) (backend.Backend, error) {
			return nil, &Refusal{Err: errors.New("craze: " + why)}
		}
		r.openSavedList([]roster.Row{laneRow(a, "session a", time.Minute)}, savedLane(s, "the saved one"))
		r.m = selectKey(t, r.m, savedKey("s"))
		r.send(enter())
		r.send(r.pop(&r.opens, "dial"))
		want := "could not resume the saved one: " + why
		if r.m.eng != a || !r.m.sessList.open || r.m.sessList.dialing != 0 || r.m.sessList.note != want {
			t.Fatalf("a refused resume: backend a %v, list open %v, dialing %d, note %q, want %q",
				r.m.eng == a, r.m.sessList.open, r.m.sessList.dialing, r.m.sessList.note, want)
		}
		if !strings.Contains(plainView(r.m), "could not resume the saved one") {
			t.Fatalf("the hint line:\n%s", plainView(r.m))
		}
	}
}

// TestSessionsCtrlXDoesNothingOnASavedRow (§3.12): a saved session runs
// nowhere, so ctrl+x on its row — once or twice — cancels nothing, arms no
// close, says nothing and calls nothing.
func TestSessionsCtrlXDoesNothingOnASavedRow(t *testing.T) {
	m, fs, _ := sessModel(t, 100, 30)
	m = richList(t, m)
	m = selectKey(t, m, sessSavedLine)
	m, _ = press(m, enter())
	m = selectKey(t, m, savedKey("wrap"))
	for range 2 {
		var cmd tea.Cmd
		m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
		if cmd != nil || !m.sessList.armed.zero() || m.sessList.note != "" {
			t.Fatalf("ctrl+x on a saved row: command %v, armed %+v, note %q", cmd != nil, m.sessList.armed, m.sessList.note)
		}
	}
	if _, cancels, stops := fs.calls(); cancels != 0 || stops != 0 {
		t.Fatalf("ctrl+x on a saved row called Cancel %d, Stop %d times", cancels, stops)
	}
}
