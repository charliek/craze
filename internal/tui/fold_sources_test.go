package tui

import (
	"io"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
)

// The two facts the fold did not carry until PR 4's prelude (plan 027 X46 3,
// SF-55 and SF-54), each held here against a real engine — whose own fold is
// what a snapshot is cut from — and against this client's fold and mirror: a
// restore has to come back with what the session shows, not with a gap the
// TUI used to paper over on its side.

// restoredEngineFold is the engine's snapshot of its main transcript, through
// the codec, restored: what an attaching client starts from.
func restoredEngineFold(t *testing.T, eng *engine.Engine) *transcript.Model {
	t.Helper()
	s, err := eng.TranscriptSnapshot("", 0)
	if err != nil {
		t.Fatalf("TranscriptSnapshot: %v", err)
	}
	b, err := transcript.EncodeSnapshot(s)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	d, err := transcript.DecodeSnapshot(b)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	return transcript.Restore(d, transcript.Options{})
}

// TestAFiredSendNowIsNotArmedInASnapshot (SF-55's snapshot twin): another
// client arms a send-now over a running turn; the arm's own cancel ends the
// turn, and the send fires — which publishes no delta, only its send_now
// started. Past the fire the engine holds no arm, and so says this client's
// fold, its mirror, and a snapshot restored through the codec.
func TestAFiredSendNowIsNotArmedInASnapshot(t *testing.T) {
	m := sizedLikeInit(t)
	stub := stubOf(t, m)
	eng := engineOf(t, m)
	m = pumpSettled(t, m)

	open := stub.HangNext()
	m = pumpEnter(t, m, "first")
	awaitBarrier(t, open, "the first turn opening")
	if res, err := eng.Submit(otherClient(t, m), "NOW", engine.SubmitSendNow, ""); err != nil || !res.Armed {
		t.Fatalf("fixture: the send-now came back %+v, %v", res, err)
	}
	m = pumpSettled(t, m)
	if st := eng.State(); st.SendNow != nil {
		t.Fatalf("fixture: the engine still holds the arm %+v", st.SendNow)
	}
	if tu := m.shared.State().Turn; tu.Origin != agent.TurnOriginSendNow || tu.Text != "NOW" {
		t.Fatalf("fixture: the last turn started is %+v, want the fired send", tu)
	}

	if sn := m.shared.State().Settings.SendNow; sn.Armed {
		t.Fatalf("this client's fold holds the fired send armed: %+v", sn)
	}
	if m.sendNowPending() {
		t.Fatal("the mirror shows the fired send armed")
	}
	r := restoredEngineFold(t, eng)
	if sn := r.State().Settings.SendNow; sn != (agent.SendNowState{}) {
		t.Fatalf("a snapshot cut after the fire restores the send-now %+v", sn)
	}
}

// TestAChildsActivityIsInASnapshot (SF-54's snapshot twin): over the fake
// grok agent, two children spawn and each runs a tool; the session holds its
// turn open after the last roster event, which carries sub-1's row and not
// sub-2's, so sub-2's activity is the one only its tool call set (agent's
// tools.go). Mid-child, every child's activity in this client's fold, in its
// mirror, and in a snapshot restored through the codec is the live session's.
func TestAChildsActivityIsInASnapshot(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := t.TempDir()
	prov := agent.GrokProvider()
	sess := agent.New(agent.Options{
		Binary:      bin,
		ExtraArgs:   []string{"-script=grok-subagent-two-hold"},
		Workspace:   ws,
		Force:       true,
		Interactive: true,
		Stderr:      io.Discard,
		Provider:    &prov,
	})
	t.Cleanup(func() { _ = sess.Close() })
	m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true, Provider: prov, ProviderLocked: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = startedLikeInit(t, tm.(Model))
	eng := engineOf(t, m)

	m = pumpEnter(t, m, "go")
	// sub-1's progress is the last event before the hold.
	m = pumpUntil(t, m, func(m Model) bool {
		info, ok := liveInfo(m.snap.Subagents, "sub-1")
		return ok && info.TokensUsed == 4740
	})
	m = pumpDrained(t, m)

	live := eng.State().Subagents
	if len(live) != 2 {
		t.Fatalf("fixture: the live roster is %+v", live)
	}
	if info, _ := liveInfo(live, "sub-2"); info.Activity == "" {
		t.Fatalf("fixture: the live session gives sub-2 no activity: %+v", info)
	}
	restored := restoredEngineFold(t, eng).State().Agents
	for _, want := range live {
		for _, side := range []struct {
			name string
			rows []agent.SubagentInfo
		}{
			{"this client's fold", m.shared.State().Agents},
			{"the mirror", m.snap.Subagents},
			{"a restored snapshot", restored},
		} {
			got, ok := liveInfo(side.rows, want.ID)
			if !ok || got.Activity != want.Activity {
				t.Fatalf("%s: %s's activity is %q, the live session's %q", side.name, want.ID, got.Activity, want.Activity)
			}
		}
	}
}
