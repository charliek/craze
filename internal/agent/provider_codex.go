package agent

import (
	"slices"
	"strings"
)

const (
	codexName      = "codex"
	codexBin       = "codex"
	codexLoginHint = "codex login"
	codexAgentMode = "agent"
	codexPlanMode  = "plan"
	codexAskMode   = "ask"
	codexEffortID  = "effort"
)

// codexStartModes are the three modes Codex supports (Plan 054 § C3):
// agent (workspace-write), plan (read-only + collaborationMode plan),
// ask (OS-enforced read-only + collaborationMode default).
var codexStartModes = []string{codexAgentMode, codexPlanMode, codexAskMode}

var codexModeKinds = map[string]ModeKind{
	codexAgentMode: ModeImplement,
	"code":         ModeImplement,
	"default":      ModeImplement,
	codexPlanMode:  ModePlan,
	"architect":    ModePlan,
	codexAskMode:   ModeReadOnly,
}

var codexModesInfo = []ModeInfo{
	{ID: codexAgentMode, Name: "Agent", Description: "Implement changes in the workspace"},
	{ID: codexPlanMode, Name: "Plan", Description: "Plan without editing the workspace"},
	{ID: codexAskMode, Name: "Ask", Description: "Read-only sandbox (no workspace writes)"},
}

// codexStrippedEnv lists parent Codex shell markers stripped from child Codex
// app-server processes so nested sessions never inherit a parent Codex turn's
// thread/session/sandbox identity (Plan 054 § A4).
var codexStrippedEnv = []string{
	"CODEX_THREAD_ID",
	"CODEX_SESSION_ID",
	"CODEX_CI",
	"CODEX_TOOL_CALL_ID",
}

func codexStripped(k string) bool {
	return strings.HasPrefix(k, "CODEX_SANDBOX_") || slices.Contains(codexStrippedEnv, k)
}

// CodexProvider is Codex CLI (`codex app-server --listen stdio://`, Plan 054).
func CodexProvider() Provider {
	return Provider{
		name:            codexName,
		displayName:     codexName,
		modeKinds:       codexModeKinds,
		startModes:      codexStartModes,
		implementPrompt: cursorImplementPrompt,
		defaultBins:     []string{codexBin},
		spawnArgs:       []string{"app-server", "--listen", "stdio://"},
		loginHint:       codexLoginHint,
		resumable:       true,
		capabilities: Capabilities{
			Effort:              true,
			Modes:               true,
			AskCards:            true,
			Todos:               true,
			Interject:           true,
			SubagentRows:        true,
			ParameterizedPicker: true,
		},
	}
}

// isCodex reports whether p is the Codex provider.
func (p Provider) isCodex() bool { return p.name == codexName }
