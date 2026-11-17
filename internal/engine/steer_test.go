package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

func begunTexts(s *fakeSession) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.begun)
}

// TestAPromptRequiredSteerIsSentAsAPrompt (plan 052 § B7): claude's steering
// answered promptRequired — the turn ended while the text was on its way —
// and the engine sends the text as the next prompt.
func TestAPromptRequiredSteerIsSentAsAPrompt(t *testing.T) {
	r := newRig(t, Options{})
	r.s.failInterjects(agent.ErrPromptRequired)
	if err := r.e.Interject(context.Background(), Command{}, "also"); err != nil {
		t.Fatal(err)
	}
	if got := begunTexts(r.s); !slices.Contains(got, "also") {
		t.Fatalf("begun %q", got)
	}
}

// TestAPromptRequiredSteerYieldsToAClear (review C3b 7): a queue clear or an
// accepted cancel that lands while the steer is in flight drops the work, and
// the delayed promptRequired then must not start or queue the old text: it is
// refused as an interjection with no turn, which leaves it with the client.
func TestAPromptRequiredSteerYieldsToAClear(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(r *rig)
	}{
		{"queue cleared", func(r *rig) {
			if _, err := r.e.ClearQueue(Command{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"cancelled", func(r *rig) {
			turn := r.s.script(held())
			r.submit("one")
			await(t, turn.opened, "the turn to open")
			if _, err := r.e.Cancel(context.Background(), Command{}, ""); err != nil {
				t.Fatalf("the cancel was refused: %v", err)
			}
			turn.release()
			r.until(lastEnding)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			r.s.failInterjects(agent.ErrPromptRequired)
			entered, release := r.s.holdNextInterject()
			t.Cleanup(release)
			errc := make(chan error, 1)
			go func() { errc <- r.e.Interject(context.Background(), Command{}, "also") }()
			<-entered
			tc.drop(r)
			release()
			if err := <-errc; !errors.Is(err, agent.ErrNotInTurn) {
				t.Fatalf("err %v, want ErrNotInTurn", err)
			}
			if got := begunTexts(r.s); slices.Contains(got, "also") {
				t.Fatalf("the dropped steer was sent: %q", got)
			}
			if st := r.e.State(); len(st.Queue) != 0 {
				t.Fatalf("the dropped steer was queued: %+v", st.Queue)
			}
		})
	}
}

// TestAPromptRequiredSteerSurvivesARefusedCancel (review C3c 5): a cancel the
// engine refuses — it names a turn that is not running, or there is nothing
// to cancel — drops nothing, so the steer's fallback still goes.
func TestAPromptRequiredSteerSurvivesARefusedCancel(t *testing.T) {
	for _, turn := range []string{"turn-9", ""} {
		t.Run("cancel "+turn, func(t *testing.T) {
			r := newRig(t, Options{})
			r.s.failInterjects(agent.ErrPromptRequired)
			entered, release := r.s.holdNextInterject()
			t.Cleanup(release)
			errc := make(chan error, 1)
			go func() { errc <- r.e.Interject(context.Background(), Command{}, "also") }()
			<-entered
			if _, err := r.e.Cancel(context.Background(), Command{}, turn); err == nil {
				t.Fatal("setup: the cancel was accepted")
			}
			release()
			if err := <-errc; err != nil {
				t.Fatalf("the fallback was refused: %v", err)
			}
			if got := begunTexts(r.s); !slices.Contains(got, "also") {
				t.Fatalf("begun %q", got)
			}
		})
	}
}
