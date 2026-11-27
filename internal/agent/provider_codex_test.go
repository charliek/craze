package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/agentproc"
	"github.com/charliek/craze/internal/codexrpc"
	"github.com/charliek/craze/internal/errkind"
)

func TestCodexProviderListedAndMetadata(t *testing.T) {
	p, err := ProviderByName("codex")
	if err != nil {
		t.Fatalf("ProviderByName(codex): %v", err)
	}
	if p.Name() != "codex" || p.DisplayName() != "codex" {
		t.Fatalf("name/display = %q/%q", p.Name(), p.DisplayName())
	}
	if p.Hidden() {
		t.Fatal("CodexProvider must be listed (not hidden) in M3")
	}
	if want := []string{"cursor", "grok", "claude", "antigravity", "codex", "native"}; !slices.Equal(ProviderNames(), want) {
		t.Fatalf("ProviderNames() = %v, want %v", ProviderNames(), want)
	}
	if p.Transport() != TransportCodex {
		t.Fatalf("Transport() = %q, want %q", p.Transport(), TransportCodex)
	}
	if !p.Resumable() || !p.RecordsModel() {
		t.Fatalf("Resumable=%v RecordsModel=%v, want both true", p.Resumable(), p.RecordsModel())
	}
	if p.InProcess() {
		t.Fatal("codex must not be InProcess")
	}
	if p.LoginHint() != "codex login" {
		t.Fatalf("LoginHint = %q, want codex login", p.LoginHint())
	}
	if got := p.Args(nil, true); !slices.Equal(got, []string{"app-server", "--listen", "stdio://"}) {
		t.Fatalf("Args(force=true) = %v", got)
	}
	if got := p.Args(nil, false); !slices.Equal(got, []string{"app-server", "--listen", "stdio://"}) {
		t.Fatalf("Args(force=false) = %v", got)
	}
	caps := p.Capabilities()
	if !caps.Effort || !caps.Modes || !caps.AskCards || !caps.Todos || !caps.Interject || !caps.SubagentRows || !caps.ParameterizedPicker {
		t.Fatalf("missing required Codex capabilities: %+v", caps)
	}
	if caps.FastToggle || caps.PlanCards || caps.SubagentTranscript || caps.SubagentCancel || caps.SubagentBackground {
		t.Fatalf("unexpected Codex capabilities enabled: %+v", caps)
	}
	if p.Aside() {
		t.Fatal("CodexProvider.Aside() must be false")
	}
	if len(p.SkillScan().RelRoots) != 0 || p.PluginScan() != (PluginScan{}) {
		t.Fatalf("CodexProvider must have zero skillScan/pluginScan")
	}

	// Verify Transport() across all providers.
	if CursorProvider().Transport() != TransportACP || GrokProvider().Transport() != TransportACP ||
		ClaudeProvider().Transport() != TransportACP || AntigravityProvider().Transport() != TransportACP {
		t.Fatal("ACP providers must report TransportACP")
	}
	if NativeProvider().Transport() != TransportInProcess {
		t.Fatal("NativeProvider must report TransportInProcess")
	}
}

func TestCodexSpawnEnvStripsParentMarkers(t *testing.T) {
	p := CodexProvider()
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/test",
		"CODEX_HOME=/tmp/codex-home",
		"OPENAI_API_KEY=sk-test",
		"CODEX_THREAD_ID=parent-thread",
		"CODEX_SESSION_ID=parent-session",
		"CODEX_CI=1",
		"CODEX_TOOL_CALL_ID=call-1",
		"CODEX_SANDBOX_NETWORK_DISABLED=1",
		"CODEX_SANDBOX_MODE=read-only",
		"CRAZE_SESSION_PROVIDER=claude",
	}
	env := p.spawnEnv(base, "", "/tmp/ws")
	kv := make(map[string]string, len(env))
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		kv[k] = v
	}
	for _, stripped := range []string{
		"CODEX_THREAD_ID",
		"CODEX_SESSION_ID",
		"CODEX_CI",
		"CODEX_TOOL_CALL_ID",
		"CODEX_SANDBOX_NETWORK_DISABLED",
		"CODEX_SANDBOX_MODE",
	} {
		if _, exists := kv[stripped]; exists {
			t.Errorf("expected %s to be stripped from codex child env", stripped)
		}
	}
	if kv["CODEX_HOME"] != "/tmp/codex-home" || kv["OPENAI_API_KEY"] != "sk-test" {
		t.Fatalf("expected CODEX_HOME and OPENAI_API_KEY preserved, got %v", kv)
	}
	if kv[SessionProviderEnv] != "codex" {
		t.Fatalf("%s = %q, want codex", SessionProviderEnv, kv[SessionProviderEnv])
	}
}

func TestCodexErrorClassificationAndEventCodec(t *testing.T) {
	if got := ErrorInfo(codexrpc.ErrClosed).Kind; got != errkind.Agent {
		t.Fatalf("ErrorInfo(codexrpc.ErrClosed) = %v, want agent", got)
	}
	wrappedExit := agentproc.WrapAgentExited(codexrpc.ErrAgentExited, nil)
	if !errors.Is(wrappedExit, ErrAgentExited) || !errors.Is(wrappedExit, codexrpc.ErrAgentExited) {
		t.Fatalf("wrapped codex exit must match both agent.ErrAgentExited and codexrpc.ErrAgentExited")
	}
	if got := ErrorInfo(wrappedExit).Kind; got != errkind.Agent {
		t.Fatalf("ErrorInfo(wrappedExit) = %v, want agent", got)
	}

	// EventCodec round-trips for codexrpc errors.
	for _, tc := range []struct {
		name  string
		err   error
		class EventErrClass
		code  int
		match error
	}{
		{"codex closed", codexrpc.ErrClosed, EventErrClosed, 0, codexrpc.ErrClosed},
		{"codex exited", wrappedExit, EventErrAgentExited, 0, ErrAgentExited},
		{"codex rpc", &codexrpc.RPCError{Code: -32600, Message: "bad request"}, EventErrRPC, -32600, nil},
		{"acp exited still matches", fmt.Errorf("%w: exit 0", acp.ErrAgentExited), EventErrAgentExited, 0, acp.ErrAgentExited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := EncodeEvent(Event{Type: EventError, Err: tc.err})
			if err != nil {
				t.Fatalf("EncodeEvent: %v", err)
			}
			got, err := DecodeEvent(body)
			if err != nil {
				t.Fatalf("DecodeEvent: %v", err)
			}
			remote, ok := got.Err.(*RemoteError)
			if !ok || remote.Class != tc.class || remote.Code != tc.code {
				t.Fatalf("decoded err = %#v, want class=%q code=%d", got.Err, tc.class, tc.code)
			}
			if tc.match != nil && !errors.Is(remote, tc.match) {
				t.Fatalf("errors.Is(remote, %v) = false", tc.match)
			}
		})
	}
}
