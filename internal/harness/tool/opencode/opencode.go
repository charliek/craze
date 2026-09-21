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
	// The three after write are grok-build's tools (plan 023 §3.4, D-53), each
	// at the place opencode gives its own counterpart — question, todo and
	// plan — with one difference: opencode offers its question tool first of
	// all, and here it follows the six, so that a session's requests still
	// open with the tools every earlier session's did.
	builders := []func() (tool.Tool, error){
		newBash,
		newRead,
		func() (tool.Tool, error) { return newGlob(rg) },
		func() (tool.Tool, error) { return newGrep(rg) },
		newEdit,
		newWrite,
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

// systemText is the profile's system prompt: opencode's
// session/prompt/default.txt reduced to what is true of craze, with the
// environment block H1's prompt had (NOTICE lists every edit). Its two
// placeholders are the only things that vary, and neither varies within a
// session, so every request in one starts with the same bytes (D-30).
//
//go:embed system.txt
var systemText string

// systemFunc returns the profile's System func. The template is checked
// here, once: Render fails only on the template's own placeholders, never on
// a value (values are inserted as they are), so a template that renders now
// renders for every session.
func systemFunc() (func(tool.SystemEnv) string, error) {
	vars := func(env tool.SystemEnv) map[string]string {
		return map[string]string{"workspace": env.Workspace, "os": env.OS}
	}
	if _, err := tool.Render(systemText, vars(tool.SystemEnv{})); err != nil {
		return nil, fmt.Errorf("system prompt: %w", err)
	}
	return func(env tool.SystemEnv) string {
		s, _ := tool.Render(systemText, vars(env)) // checked above
		return s
	}, nil
}

// description returns the named tool's description, rendered with vars. The
// file's text is kept exactly, final newline included, as opencode sends it.
func description(name string, vars map[string]string) (string, error) {
	b, err := descriptions.ReadFile("descriptions/" + name + ".txt")
	if err != nil {
		return "", fmt.Errorf("description %q: %w", name, err)
	}
	s, err := tool.Render(string(b), vars)
	if err != nil {
		return "", fmt.Errorf("description %q: %w", name, err)
	}
	return s, nil
}
