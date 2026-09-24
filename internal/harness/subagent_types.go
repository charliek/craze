package harness

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/charliek/craze/internal/harness/tool"
)

// Agent types (plan 026 §3.4). A session's agent tool starts a sub-agent of a
// type: one of the harness's three built-ins, or a persona the adapter found
// in a persona file and handed over as data (Options.Personas). The session
// merges the two once, at Open (agentTypes), into the one list the agent
// tool's description shows and a call's subagent_type resolves against, so
// the type a call gets is the type the model was shown. The adapter owns
// Claude's vocabulary — reading the files, mapping their tool names
// (tool.MapClaudeTools), reporting what it could not map — and the harness
// owns precedence, the built-ins and what a type gives a child.

// Persona is tool.Persona, named here so that Options can carry the adapter's
// (Options.Personas): harness.go shapes Fantasy's types, and toolbridge.go is
// the one file that may import both Fantasy and the tool framework (Seam 1,
// plan 019 §3.1), as Asker says.
type Persona = tool.Persona

// builtinTypes are the harness's own agent types, in the order they are
// listed. Their descriptions and roles are craze's own words (§3.4's table):
// general-purpose has every tool a child may have, and explore and plan read
// and search only, so neither can change anything whatever the mode.
var builtinTypes = []tool.Persona{
	{
		Name:        tool.DefaultAgentType,
		Description: "A general-purpose agent for multi-step research and code tasks: it searches, reads, runs commands and edits files.",
		Role: "You are a general-purpose agent. Carry out the task you were given from start\n" +
			"to finish: search for and read what you need, run commands, and make the\n" +
			"changes it asks for. When you are done, report what you did and what you\n" +
			"found, with the file paths that matter.\n",
		Scope:    tool.PersonaBuiltin,
		AllTools: true,
	},
	{
		Name:        "explore",
		Description: "Fast, read-only codebase search: finds files, definitions and usages, and reports what it found.",
		Role: "You explore a codebase to answer a question, and you change nothing: you can\n" +
			"read files and search them, and nothing else. Search broadly first, then\n" +
			"narrow down, and read the parts that answer the question. Report what you\n" +
			"found — file paths with line numbers, and the lines that matter — so the\n" +
			"agent that started you does not have to search again.\n",
		Scope: tool.PersonaBuiltin,
		Tools: []string{"read", "grep", "glob"},
	},
	{
		Name:        "plan",
		Description: "A read-only software architect: studies the code and returns an implementation plan.",
		Role: "You are a software architect. Study the code the task concerns — you can read\n" +
			"files and search them, and nothing else — and return an implementation plan:\n" +
			"the steps in order, the files and functions each step touches, what could\n" +
			"break, and the questions still open. Do not write the code; a short snippet\n" +
			"is fine where it makes a step clear.\n",
		Scope: tool.PersonaBuiltin,
		Tools: []string{"read", "grep", "glob"},
	},
}

// BuiltinAgentTypes returns the names of the harness's built-in agent types,
// in the order they are listed: general-purpose, explore, plan. The adapter
// reads it when it discovers personas, to skip — with its one diagnostic — a
// user persona that would shadow one (plan 026 §3.4); a project persona
// shadows one by design. A new slice each call.
func BuiltinAgentTypes() []string {
	names := make([]string, len(builtinTypes))
	for i, b := range builtinTypes {
		names[i] = b.Name
	}
	return names
}

// scopeRank is a persona scope's place in the precedence order — project,
// built-in, user, plugin (grok-build's, owner decision 6) — or -1 for a scope
// the harness does not take from its caller: the built-ins are its own, and a
// persona claiming that scope, or none, is not one the adapter read.
func scopeRank(scope string) int {
	switch scope {
	case tool.PersonaProject:
		return 0
	case tool.PersonaUser:
		return 2
	case tool.PersonaPlugin:
		return 3
	}
	return -1
}

// AgentTypeKey is how agent type names compare: two names are one type when
// their keys are equal. A name is folded onto one line, as the description
// shows it, and case-insensitively, so Claude Code's Explore and Plan find the
// built-ins (plan 026 §3.4). It is the one definition: the adapter
// deduplicates the personas it reads by it, so it and the harness's
// resolution can never disagree about which names are one.
//
// Case-insensitively in strings.EqualFold's sense, not strings.ToLower's: each
// rune becomes the least rune of its simple case-folding orbit, so two names
// EqualFold calls equal have one key. Lowering alone leaves Σ and ς apart —
// both survive agentTypes, and the lower-precedence one answers to its own
// spelling (review r2 of C3a).
func AgentTypeKey(name string) string { return strings.Map(foldRune, foldLine(name)) }

// foldRune is r's representative under simple case folding: the least rune of
// the orbit unicode.SimpleFold walks from r.
func foldRune(r rune) rune {
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

// agentTypes is the list a session offers: personas and the built-ins merged
// in precedence order — project, built-in, user, plugin; within a scope, the
// caller's order, which for the workspace is innermost first — with each name
// once, the first to claim it winning, compared by AgentTypeKey. So a project
// persona shadows a built-in, a built-in shadows a user persona (which the
// adapter has already dropped with a diagnostic; this is the defensive half),
// and a plugin's, named plugin:name, cannot collide with a bare name.
//
// Every name is folded onto one line, which is how the description lists it
// and how a call's name is compared: a name the model reads must be the name
// it can send back. One that folds to nothing is dropped, as is a persona of a
// scope the harness does not take (scopeRank). The slices are copied, so the
// caller's personas are never aliased by the session.
func agentTypes(personas []tool.Persona) []tool.Persona {
	var ranked [4][]tool.Persona
	ranked[1] = builtinTypes
	for _, p := range personas {
		if r := scopeRank(p.Scope); r >= 0 {
			ranked[r] = append(ranked[r], p)
		}
	}
	var out []tool.Persona
	seen := map[string]bool{}
	for _, scope := range ranked {
		for _, p := range scope {
			p.Name = foldLine(p.Name)
			key := AgentTypeKey(p.Name)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			p.Tools, p.DisallowedTools = slices.Clone(p.Tools), slices.Clone(p.DisallowedTools)
			out = append(out, p)
		}
	}
	return out
}

// callError is an agent call the runner refuses before any child opens, with
// the class its result carries: the model reads the text and tries again
// (plan 026 §3.4, §3.7).
type callError struct {
	class tool.ErrorClass
	text  string
}

func (e *callError) Error() string { return e.text }

// result is the refusal as the agent call's result.
func (e *callError) result() tool.Result {
	return tool.Result{Text: e.text, IsError: true, Class: e.class}
}

// The bounds of the unknown-type refusal: the whole text, and the name the
// model sent, as it is shown back to it.
const (
	maxUnknownTypeText = 1 << 10
	maxShownTypeName   = 128
)

// resolveAgentType is the agent type named name in types (agentTypes), matched
// by AgentTypeKey; "" is the default, general-purpose, as the tool's Prepare
// already made it. A name no type has is a *callError of class invalid_input
// listing the types there are, in the order the description lists them,
// within 1 KiB (plan 026 §3.4): "Unknown agent type `x`. Available types: a,
// b, c (…and N more)." The persona returned shares no slice with types.
func resolveAgentType(types []tool.Persona, name string) (tool.Persona, error) {
	if strings.TrimSpace(name) == "" {
		name = tool.DefaultAgentType
	}
	key := AgentTypeKey(name)
	for _, p := range types {
		if AgentTypeKey(p.Name) == key {
			p.Tools, p.DisallowedTools = slices.Clone(p.Tools), slices.Clone(p.DisallowedTools)
			return p, nil
		}
	}
	names := make([]string, len(types))
	for i, p := range types {
		names[i] = p.Name
	}
	return tool.Persona{}, &callError{class: tool.ClassInvalidInput, text: unknownTypeText(name, names)}
}

// unknownTypeText is the refusal of name: it shown folded and cut to
// maxShownTypeName bytes, since the model may have sent anything, and as many
// of names, from the front, as fit maxUnknownTypeText with the count of the
// rest. The count is inside the budget, so the loop asks what fits with the
// words it would then write.
func unknownTypeText(name string, names []string) string {
	head := "Unknown agent type `" + oneLine(foldLine(name), maxShownTypeName) + "`. Available types: "
	for keep := len(names); ; keep-- {
		list := strings.Join(names[:keep], ", ")
		if keep < len(names) {
			if list != "" {
				list += " "
			}
			list += fmt.Sprintf("(…and %d more)", len(names)-keep)
		}
		if text := head + list + "."; len(text) <= maxUnknownTypeText || keep == 0 {
			return text
		}
	}
}

// childToolSet is what a child of type p is given, out of offered: the tools
// a child may have in the parent's profile, in the profile's order (plan 026
// §3.4). A type with every tool and nothing disallowed is all true and no
// list, which ChildOptions.AllTools says; any other is the list — the type's
// tools, or every offered one, less the ones it disallows, in offered's order
// — and an empty list is a text-only child. disallowedTools is subtracted
// after the adapter mapped both lists, so a Claude name and its native id take
// away the same tool.
func childToolSet(p tool.Persona, offered []string) (all bool, ids []string) {
	if p.AllTools && len(p.DisallowedTools) == 0 {
		return true, nil
	}
	for _, id := range offered {
		if (p.AllTools || slices.Contains(p.Tools, id)) && !slices.Contains(p.DisallowedTools, id) {
			ids = append(ids, id)
		}
	}
	return false, ids
}
