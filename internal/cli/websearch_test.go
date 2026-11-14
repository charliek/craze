package cli

import (
	"io"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

// TestSessionOptionsCarryWebSearch (plan 051 §C.1): both ways a session is
// described — the TUI's and craze serve's (sessionOptions), and craze
// prompt's (agentOptions) — carry config.toml's `websearch` reading as it is
// at the time, for the native session to decide from when it opens; with no
// key, the reading is absent.
func TestSessionOptionsCarryWebSearch(t *testing.T) {
	native := agent.NativeProvider()
	describe := func() (agent.Options, agent.Options) {
		_, f := parseTUIFlags(t)
		po := &promptOpts{force: true, stderr: io.Discard}
		return sessionOptions(f, t.TempDir(), "", io.Discard, io.Discard, nil, native, native, sessions.Row{}),
			po.agentOptions(t.TempDir(), headlessApproval(true), &native)
	}
	for body, want := range map[string]agent.WebConfig{
		"theme = \"dark\"\n":                {},
		"websearch = false\n":               {Setting: agent.WebSettingOff},
		"[websearch]\nprovider = \"exa\"\n": {Setting: agent.WebSettingOn, Provider: "exa"},
		"websearch = true\n":                {Setting: agent.WebSettingInvalid},
		"websearch = false\nnot toml [[[\n": {Setting: agent.WebSettingInvalid},
	} {
		writeCrazeConfig(t, body)
		tuiOpts, promptOpts := describe()
		for name, got := range map[string]agent.WebConfig{"sessionOptions": tuiOpts.WebSearch, "agentOptions": promptOpts.WebSearch} {
			if got.Setting != want.Setting || got.Provider != want.Provider || (want.Setting == agent.WebSettingInvalid) != (got.Why != "") {
				t.Errorf("%s with %q: %+v; want %+v", name, body, got, want)
			}
		}
	}
}
