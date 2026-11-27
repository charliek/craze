package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/sessions"
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

const invalidTransportNote = "craze: config.toml openai_transport must be auto, websocket, or sse\n"

func TestCompatOpenAITransportReachesOptions(t *testing.T) {
	provider, err := agent.ProviderByName("native")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, mode, diagnostic string
	}{
		{"absent", "", "", ""},
		{"valid", `openai_transport = " WebSocket "`, "websocket", ""},
		{"invalid", `openai_transport = true`, "", invalidTransportNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeCrazeConfig(t, tc.body)
			var diag bytes.Buffer
			if got := openAITransport(&diag); got != tc.mode || diag.String() != tc.diagnostic {
				t.Fatalf("openAITransport = %q, %q; want %q, %q", got, diag.String(), tc.mode, tc.diagnostic)
			}
			diag.Reset()
			opts := sessionOptions(&tuiFlags{}, t.TempDir(), "", nil, &diag, nil, provider, provider, sessions.Row{})
			if opts.OpenAITransport != tc.mode || diag.String() != tc.diagnostic {
				t.Fatalf("sessionOptions = %q, %q; want %q, %q", opts.OpenAITransport, diag.String(), tc.mode, tc.diagnostic)
			}
			diag.Reset()
			prompt := promptOpts{stderr: &diag, json: true}
			opts = prompt.agentOptions(t.TempDir(), nil, &provider)
			if opts.OpenAITransport != tc.mode || diag.String() != tc.diagnostic {
				t.Fatalf("prompt options = %q, %q; want %q, %q", opts.OpenAITransport, diag.String(), tc.mode, tc.diagnostic)
			}
		})
	}
}

func TestCompatOpenAITransportPromptJSONKeepsDiagnosticsOnStderr(t *testing.T) {
	isolateRunEnv(t)
	t.Setenv("CRAZE_OPENAI_TRANSPORT", "")
	writeCrazeConfig(t, "openai_transport = false\n")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--json", "--provider", "native", "--workspace", t.TempDir(), "--model", "missing-transport-test-model", "hello"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("prompt with an unknown model succeeded")
	}
	if got := strings.Count(stderr.String(), invalidTransportNote); got != 1 {
		t.Fatalf("stderr = %q, want one %q", stderr.String(), invalidTransportNote)
	}
	events := parseJSONLines(t, stdout.String())
	if len(events) != 1 || events[0].m["type"] != "error" {
		t.Fatalf("stdout = %q, want a JSON error event", stdout.String())
	}
}

func TestCompatOpenAITransportInheritedByHostProcesses(t *testing.T) {
	t.Setenv("CRAZE_OPENAI_TRANSPORT", "sse")
	previous := hostspawn.Command
	t.Cleanup(func() { hostspawn.Command = previous })
	hostspawn.Command = func([]string) (*exec.Cmd, error) {
		// Write the child's actual environment value to the launcher pipe.
		return exec.Command("sh", "-c", `printf '%s' "$CRAZE_OPENAI_TRANSPORT" >&3`), nil
	}
	for _, name := range []string{"detached host", "review host"} {
		t.Run(name, func(t *testing.T) {
			var child *hostspawn.Child
			var pipe *os.File
			var err error
			if name == "review host" {
				child, pipe, err = startWithHubEnv(t.Context(), nil, "", "", nil)
			} else {
				child, pipe, err = hostspawn.Start(nil, nil, "", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			defer pipe.Close()
			got, err := io.ReadAll(pipe)
			<-child.Done()
			if err != nil || string(got) != "sse" {
				t.Fatalf("child CRAZE_OPENAI_TRANSPORT = %q, %v; want sse", got, err)
			}
		})
	}
}
