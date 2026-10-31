package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
)

// TestALastTurnSaysWhatBeganItsClose: lastTurn on the wire carries the cause of
// the close that ended the turn, as the schema says, and nothing for any other.
func TestALastTurnSaysWhatBeganItsClose(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	closed := lastTurn(engine.State{LastTurn: &engine.LastTurn{Outcome: engine.TurnCancelled, StopReason: "closing", Cause: agent.StopCauseSignal, EndedAt: at, TurnID: "turn-1"}})
	b, err := json.Marshal(closed)
	if err != nil {
		t.Fatal(err)
	}
	if err := wiretest.Default().Document("info.json", "#/$defs/lastTurn", b); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if !strings.Contains(string(b), `"cause":"signal"`) {
		t.Fatalf("lastTurn: %s", b)
	}
	var back protocol.LastTurn
	if err := json.Unmarshal(b, &back); err != nil || back.Cause != agent.StopCauseSignal {
		t.Fatalf("read back: %+v, %v", back, err)
	}
	withdrawn := lastTurn(engine.State{LastTurn: &engine.LastTurn{Outcome: engine.TurnCancelled, StopReason: "cancelled", Withdrawn: true, EndedAt: at, TurnID: "turn-1"}})
	b, err = json.Marshal(withdrawn)
	if err != nil {
		t.Fatal(err)
	}
	if err := wiretest.Default().Document("info.json", "#/$defs/lastTurn", b); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if !strings.Contains(string(b), `"withdrawn":true`) {
		t.Fatalf("lastTurn: %s", b)
	}
	done := lastTurn(engine.State{LastTurn: &engine.LastTurn{Outcome: engine.TurnDone, StopReason: "end_turn", EndedAt: at, TurnID: "turn-1"}})
	if b, _ := json.Marshal(done); strings.Contains(string(b), "cause") {
		t.Fatalf("a turn that ran to its end: %s", b)
	}
}
