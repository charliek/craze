package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestBuiltinsOutsideRunsWhileWorkingAreRefusedVisibly (plan 046 A-2): a typed
// /model, /plan, /ask or /agent while a turn runs (own or the agent's) adds
// exactly one error row naming the command, and the draft stays in the
// composer; the builtins that run mid-turn are not refused.
func TestBuiltinsOutsideRunsWhileWorkingAreRefusedVisibly(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		for _, name := range []string{"model", "plan", "ask", "agent"} {
			m, stub := sizedCards(t)
			if foreign {
				stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "wake-1", Running: true})
				m = applyPending(t, m)
			} else {
				m.status = statusWorking
			}
			m, _ = typeCommand(t, m, "/"+name)
			want := "Finish or stop the running work first, then /" + name + "."
			if got := texts(m, entryError); !slices.Equal(got, []string{want}) {
				t.Fatalf("/%s (foreign %v): error rows %q, want exactly %q", name, foreign, got, want)
			}
			if m.input.Value() != "/"+name {
				t.Fatalf("/%s: the draft was not kept: %q", name, m.input.Value())
			}
			if m.dialog != dialogNone {
				t.Fatalf("/%s opened dialog %v mid-turn", name, m.dialog)
			}
			if !strings.Contains(plainView(m), want) {
				t.Fatalf("/%s: the refusal is not on screen:\n%s", name, plainView(m))
			}
		}
	}

	// One row per refused submit: a second Enter adds a second row, never more.
	m := sized(t)
	m.status = statusWorking
	m, _ = typeCommand(t, m, "/plan")
	m, _ = press(m, enter())
	if got := len(texts(m, entryError)); got != 2 {
		t.Fatalf("two refused submits left %d error rows, want 2", got)
	}

	// /connect's own refusal is the same sentence.
	if connectBusyText != busyRefusalText("connect") {
		t.Fatalf("connectBusyText %q is not busyRefusalText(connect) %q", connectBusyText, busyRefusalText("connect"))
	}
}
