package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// TestShellCarriesTheCallerMarker (plan 052 § J1, J6): a native session's
// shell commands are told they run under native — CRAZE_SESSION_PROVIDER,
// once, over the one craze itself inherited from whoever started it — so a
// `craze new` run from there knows it would be calling native from native.
func TestShellCarriesTheCallerMarker(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	t.Setenv(tool.SessionProviderEnv, "cursor")
	s := f.open(f.options())
	cmd := `printf '[%s] ' "$` + tool.SessionProviderEnv + `"; env | grep -c '^` + tool.SessionProviderEnv + `='`
	f.models["test/a"].push(callStep(callParts("c0", "bash", input(t, map[string]any{"command": cmd}))), answerWith("done"))
	var ev events
	if res, err := s.Run(context.Background(), "which provider", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	fin := of[ToolFinished](ev.list())
	if len(fin) != 1 {
		t.Fatalf("%d calls finished, want 1", len(fin))
	}
	if got := strings.TrimSpace(fin[0].Result.Text); got != "["+NativeProvider+"] 1" {
		t.Fatalf("the shell saw %q, want the marker native, once", got)
	}
}
