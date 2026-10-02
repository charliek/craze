// Package opencode is the native harness's first tool profile: opencode's
// tool contract (plan 019 §3.2), ported rather than invented — tool ids,
// parameter names, types and required lists, the model-facing descriptions,
// limits, and result phrasing — from opencode at commit 5f9d9187 (MIT). The
// NOTICE file beside this one carries opencode's license, every edit made to
// a description, and every deliberate difference in behaviour.
//
// The descriptions are opencode's own text, the tuned asset, embedded from
// descriptions/ and rendered with tool.Render; nothing in them is reworded
// beyond what NOTICE lists.
package opencode

import (
	"embed"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// Name is the profile's name: what a model's tool_profile in models.toml
// gives to choose it, and what a transcript's header records.
const Name = "opencode"

// CredentialsFile is the name of the harness's key file under Env.Home,
// which the file tools refuse to touch (plan 019 §3.8). It is
// modeltable.ProvidersFile, which this package may not import; a test there
// pins the two together.
const CredentialsFile = "providers.toml"

// AuthDir is the name of the harness's sign-in directory under Env.Home, the
// ChatGPT plan's tokens and registration (plan 033 §3.12), which the file
// tools refuse whole. It is modeltable.ChatGPTAuthDir (and chatgptauth's), a
// test in internal/agent pins the three together.
const AuthDir = "auth"

// Profile returns the opencode profile with a fresh set of its tools. A
// session builds its own, so grep and glob share one ripgrep, and look for
// rg on PATH once per session. Its System func is the system prompt written
// for these tools (System).
func Profile() (tool.Profile, error) {
	system, err := systemFunc()
	if err != nil {
		return tool.Profile{}, fmt.Errorf("opencode: %w", err)
	}
	rg := newRipgrep()
	// The tools in the order the model is offered them: opencode's registry
	// order (tool/registry.ts:229-246). The order is part of every request's
	// cache prefix, so a tool is added at its place in it, never at the end.
	//
	// bash_output and bash_stop, which read and stop a background job (plan
	// 033 §3.7), follow bash, the tool whose jobs they are, as agent_output
	// follows agent: bash's own description changed with them, so the prefix
	// they move is moved anyway. agent, right after write, is craze's
	// sub-agent tool at the place opencode gives its own task tool (plan 026
	// §3.3, §5 C3b), and agent_output, which reads a background agent call's
	// result, follows it (§3.11). The three after them are grok-build's tools
	// (plan 023 §3.4, D-53), each at the place
	// opencode gives its own counterpart — question, todo and plan — with one
	// difference: opencode offers its question tool first of all, and here it
	// follows the six, so that a session's requests still open with the tools
	// every earlier session's did.
	builders := []func() (tool.Tool, error){
		newBash,
		newBashOutput,
		newBashStop,
		newRead,
		func() (tool.Tool, error) { return newGlob(rg) },
		func() (tool.Tool, error) { return newGrep(rg) },
		newEdit,
		newWrite,
		newAgent,
		newAgentOutput,
		newTodoWrite,
		newAskUserQuestion,
		newExitPlanMode,
	}
	p := tool.Profile{Name: Name, System: system}
	for _, build := range builders {
		t, err := build()
		if err != nil {
			return tool.Profile{}, fmt.Errorf("opencode: %w", err)
		}
		p.Tools = append(p.Tools, t)
	}
	return p, nil
}

//go:embed descriptions/*.txt
var descriptions embed.FS

// systemText is the profile's system prompt: craze's own text, written for
// this tool set (plan 029 §3.2 L1; NOTICE's "The system prompt" section
// records which sentences are adapted from a reference and which are craze's
// own). Its three placeholders are the only things that vary, and none of
// them varies within a session, so every request in one starts with the same
// bytes (D-30).
//
//go:embed system.txt
var systemText string

// systemFunc returns the profile's System func. The template is checked
// here, once: Render fails only on the template's own placeholders, never on
// a value (values are inserted as they are), so a template that renders now
// renders for every session.
func systemFunc() (func(tool.SystemEnv) string, error) {
	vars := func(env tool.SystemEnv) map[string]string {
		return map[string]string{"workspace": env.Workspace, "os": env.OS, "shell": env.Shell}
	}
	if _, err := tool.Render(systemText, vars(tool.SystemEnv{})); err != nil {
		return nil, fmt.Errorf("system prompt: %w", err)
	}
	return func(env tool.SystemEnv) string {
		s, _ := tool.Render(systemText, vars(env)) // checked above
		return s
	}, nil
}

// description returns the named tool's description, rendered with vars, as a
// session that runs background jobs is offered it: its jobs blocks kept, their
// marks gone (withJobs). The file's text is otherwise kept exactly, final
// newline included, as opencode sends it.
func description(name string, vars map[string]string) (string, error) {
	return renderDescription(name, vars, true)
}

// descriptionWithoutJobs is description as a session that runs no background
// jobs is offered it — headless, or a sub-agent's (plan 033 X101): its jobs
// blocks cut. The one file is the source of both, so the two cannot drift.
func descriptionWithoutJobs(name string, vars map[string]string) (string, error) {
	return renderDescription(name, vars, false)
}

func renderDescription(name string, vars map[string]string, jobs bool) (string, error) {
	b, err := descriptions.ReadFile("descriptions/" + name + ".txt")
	if err != nil {
		return "", fmt.Errorf("description %q: %w", name, err)
	}
	text, err := withJobs(string(b), jobs)
	if err != nil {
		return "", fmt.Errorf("description %q: %w", name, err)
	}
	s, err := tool.Render(text, vars)
	if err != nil {
		return "", fmt.Errorf("description %q: %w", name, err)
	}
	return s, nil
}

// The marks around a description's text that only a session running
// background jobs is offered (plan 033 X101): bash's background, promotion and
// job sentences, which a headless session's or a sub-agent's bash leaves out
// (C8's description, byte for byte). A block runs from just after one sentence
// to just before the next text both variants share, so cutting it leaves no
// blank line and no doubled space. Neither mark is in any description's text
// otherwise; tool.Render would leave one as it is.
const (
	jobsOpen  = "{{jobs}}"
	jobsClose = "{{/jobs}}"
)

// withJobs returns text with its jobs blocks kept (jobs) or cut, and the
// marks removed either way. A mark out of place — a close before any open, an
// open inside a block, a block never closed — is an error: a bug in the
// file, which the profile's build reports before any session offers it.
func withJobs(text string, jobs bool) (string, error) {
	var b strings.Builder
	for rest := text; ; {
		i := strings.Index(rest, jobsOpen)
		if c := strings.Index(rest, jobsClose); c >= 0 && (i < 0 || c < i) {
			return "", fmt.Errorf("a %s with no %s before it", jobsClose, jobsOpen)
		}
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		b.WriteString(rest[:i])
		rest = rest[i+len(jobsOpen):]
		j := strings.Index(rest, jobsClose)
		switch {
		case j < 0:
			return "", fmt.Errorf("a %s that is never closed", jobsOpen)
		case strings.Contains(rest[:j], jobsOpen):
			return "", fmt.Errorf("a %s inside a jobs block", jobsOpen)
		}
		if jobs {
			b.WriteString(rest[:j])
		}
		rest = rest[j+len(jobsClose):]
	}
}
