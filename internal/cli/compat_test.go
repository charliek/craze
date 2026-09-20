package cli

import (
	"bytes"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// compatClaude is where a run turns [compat.claude] into the session's own
// Options (plan 022 §3.5). The table's parsing is tui.ConfigCompatClaude's and
// is pinned there; what is pinned here is the lane — a value craze could not
// read is one line on craze's own stream and the class left on, never a
// session quietly started without the user's instructions.
func TestCompatClaudeReachesTheOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want agent.ClaudeCompat
		why  string
	}{
		{name: "no table at all", body: "theme = \"dark\"\n"},
		{
			name: "a class turned off",
			body: "[compat.claude]\nplugins = false\n",
			want: agent.ClaudeCompat{NoPlugins: true},
		},
		{
			name: "a value that is not a bool",
			body: "[compat.claude]\nskills = \"no\"\n",
			why:  "craze: config.toml compat.claude.skills is not a bool; leaving it on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeCrazeConfig(t, tc.body)
			var diag bytes.Buffer
			if got := compatClaude(&diag); got != tc.want {
				t.Fatalf("compatClaude = %+v, want %+v", got, tc.want)
			}
			lines := diagLines(diag.String())
			switch {
			case tc.why == "" && len(lines) != 0:
				t.Fatalf("compatClaude said %q, want nothing", lines)
			case tc.why != "":
				if len(lines) != 1 || lines[0] != tc.why {
					t.Fatalf("diagnostics %q, want exactly %q", lines, tc.why)
				}
			}
		})
	}
}

// TestCompatClaudeSurvivesANilDiag: craze frame builds sessions with no lane
// of its own, and a run must not panic over a config file it could not read.
func TestCompatClaudeSurvivesANilDiag(t *testing.T) {
	writeCrazeConfig(t, "[compat.claude]\ncommands = 1\n")
	if got := compatClaude(nil); got != (agent.ClaudeCompat{}) {
		t.Fatalf("compatClaude = %+v, want every class on", got)
	}
}
