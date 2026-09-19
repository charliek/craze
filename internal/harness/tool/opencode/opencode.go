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

// builders make the profile's tools in the order the model is offered them:
// opencode's registry order (tool/registry.ts:229-246 — bash, read, glob,
// grep, edit, write), less the tools this build does not have yet. The
// order is part of every request's cache prefix, so a tool is added at its
// place in that order, never at the end.
var builders = []func() (tool.Tool, error){
	newBash,
	newRead,
	newEdit,
	newWrite,
}

// Profile returns the opencode profile with a fresh set of its tools.
//
// Its System func is nil: the system prompt the profile will carry arrives
// with the runner that sends it (plan 019 §3.6), and until then
// tool.Registry refuses to register the profile, so no session can be
// opened on half of it.
func Profile() (tool.Profile, error) {
	p := tool.Profile{Name: Name}
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
