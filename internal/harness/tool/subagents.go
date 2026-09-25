package tool

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Sub-agents (plan 026 §3.1). The agent tool (opencode's profile) prepares a
// call and hands it to Env.Subagents; the harness's runner, which this package
// may not import, opens a child session for it, runs it to its end and answers
// with the call's Result. Everything here is pure data on both sides, like the
// asker's and the todo list's seams: the call as the model made it, the result
// and the child's usage coming back, and the agent types a persona file
// describes, which the adapter reads and the harness resolves.

// DefaultAgentType is the agent type a call that names none starts: the
// harness's general-purpose built-in (plan 026 §3.3, §3.4).
const DefaultAgentType = "general-purpose"

// SubagentCall is one agent call as the tool prepared it: the model's
// arguments, parsed and defaulted, and nothing resolved yet — the runner picks
// the type, the model and the effort, and it is the runner's refusal the model
// reads when one of them names nothing (plan 026 §3.4, §3.6).
type SubagentCall struct {
	// ID is the harness's id for the agent call (Call.ID, "t<turn>.<step>.<n>"):
	// the parent call a child's transcript header links to, the CallID its
	// lifecycle events carry, and the name of the spill file a truncated
	// result is saved to.
	ID string
	// Description is the model's short label for the task, which the child's
	// row shows; Prompt is the task itself, the child's first user message.
	Description, Prompt string
	// Type is the agent type the model asked for, DefaultAgentType when it
	// named none.
	Type string
	// Model and Effort are the call's own choices, "" for none: the first
	// candidates of the runner's resolution, which falls back to the type's,
	// the configured default's and the parent's own (plan 026 §3.6).
	Model, Effort string
	// Background is the call's run_in_background: the model asks for the
	// child to run in the background, the call returning at once and the
	// result delivered when it finishes (plan 026 §3.11). It is a request: a
	// session that runs no background children runs the call in the
	// foreground and answers with the result, as if it were false.
	Background bool
}

// OutputCall is one agent_output call as the tool prepared it (plan 026
// §3.11): the background child whose result the model wants, and how long to
// wait for one still running.
type OutputCall struct {
	// CallID is the harness's id for the agent_output call (Call.ID,
	// "t<turn>.<step>.<n>"), which names the step it runs in: a result it
	// takes is delivered by that step's append, or given back when the step
	// writes nothing.
	CallID string
	// ID is the sub-agent's id as the agent call's acknowledgement named it,
	// trimmed; the model's text, which may name nothing.
	ID string
	// Wait bounds the wait for a child still running: from 0 (look, and do not
	// wait) to AgentOutputMaxWait.
	Wait time.Duration
}

// agent_output's wait: the default and the most a call may ask for (plan 026
// §3.11, codex's wait_agent shape).
const (
	AgentOutputDefaultWait = 30 * time.Second
	AgentOutputMaxWait     = 600 * time.Second
)

// Subagents runs a sub-agent for the agent tool (plan 026 §3.1, §3.8).
//
// Run blocks until the child has ended and returns what the parent's model
// reads (§3.7): the child's final message, a note appended when it was cut
// off, refused, stopped or looping, or an error result for a call the runner
// refused (an unknown type, model or effort) or a child that failed. A
// cancelled call returns ClassAborted with AbortedText, as every tool must
// once its ctx is done. Result.Child carries the child's usage whenever a
// child ran. Run never returns a Go error, like every Prepared.Run.
//
// Run cuts the result itself, a success and an error alike — plan 026 §3.7's
// "one length cap, on both paths" — through TruncateRedacted, with the keys
// its session and the child know (review r6). The agent tool's spec is
// Truncate None, so the dispatcher only redacts the result, as it redacts
// every tool's.
//
// A call with Background set, in a session that runs background children
// (plan 026 §3.11), returns once the child has started, with an
// acknowledgement naming it and no usage; the child's result is delivered to
// the parent later, and Output is how the model asks for it sooner.
//
// Output answers an agent_output call: the result of a background child —
// delivered once, whichever way — or why there is none to give now. It waits
// only for a child still running, up to call.Wait; like Run it never returns
// a Go error, and it returns aborted once ctx is done. Its text is capped
// already (the result was cut when the child finished), so the agent_output
// tool's spec is Truncate None as well.
type Subagents interface {
	Run(ctx context.Context, call SubagentCall) Result
	Output(ctx context.Context, call OutputCall) Result
}

// Usage is a sub-agent's token counts: the tool package's own shape, since
// this package may not import the store whose Usage the harness persists
// (plan 026 §3.7). The fields are the store's, one for one.
type Usage struct {
	Input, Output, Reasoning, CacheRead, CacheCreation int64
}

// ChildUsage is what a sub-agent spent and the model it spent it on — the
// model table's provider id and alias and the provider's wire id, as the
// transcript names a model — so the parent's step records it per model
// (subagent_usage) and a child that ran on another model than its parent is
// priced at its own model's rate (plan 026 §3.7).
type ChildUsage struct {
	Provider, Model, WireModel string
	Usage                      Usage
}

// The scopes a Persona comes from. The first three are the adapter's sources
// (plan 026 §3.4): the workspace's .claude/agents chain, the user's
// ~/.claude/agents and an installed plugin's agents/. PersonaBuiltin is the
// harness's own three types; the harness ignores a persona handed to it with
// any scope but the adapter's three.
const (
	PersonaProject = "project"
	PersonaUser    = "user"
	PersonaPlugin  = "plugin"
	PersonaBuiltin = "builtin"
)

// Persona is one agent type (plan 026 §3.4): a persona file the adapter read,
// handed to the harness as data (harness.Options.Personas), or one of the
// harness's built-ins. The harness never reads the file itself.
type Persona struct {
	// Name is what the model passes as subagent_type: the file's frontmatter
	// name, else its base name, qualified plugin:name for a plugin's.
	Name string
	// Description is the frontmatter description, which the agent tool's
	// per-session description lists.
	Description string
	// Model and Effort are the frontmatter's model and effort, "" when the
	// file has none: the second candidates of the runner's resolution, after
	// the call's own (plan 026 §3.6).
	Model, Effort string
	// Role is the file's body, frozen into the child's system prompt as its
	// role section; the harness caps and redacts it there.
	Role string
	// Path is the file the persona came from, recorded in the child's
	// transcript header as provenance; "" for a built-in.
	Path string
	// Scope is where it came from: PersonaProject, PersonaUser, PersonaPlugin
	// or PersonaBuiltin. It decides precedence (project > built-in > user >
	// plugin).
	Scope string
	// AllTools gives the child every tool a child may have. It is true only
	// when the file has no tools key at all: a key that is present but maps to
	// nothing gives a text-only child rather than every tool, so a list craze
	// cannot read fails closed (plan 026 §3.4, panel CodeRabbit 3).
	AllTools bool
	// Tools are the native tool ids the child is given when AllTools is false
	// (MapClaudeTools), and DisallowedTools the native ids taken away from
	// whichever set it has (MapClaudeDisallowed); an empty result is a
	// text-only child.
	Tools, DisallowedTools []string
}

// claudeToolIDs maps a Claude Code tool name, lowercased, to the native id it
// stands for (plan 026 §3.4). The native ids of the tools a child may have are
// their own lowercase spellings, so a list written in native ids maps through
// the same table, verbatim.
var claudeToolIDs = map[string]string{
	"read":      "read",
	"ls":        "read",
	"write":     "write",
	"edit":      "edit",
	"multiedit": "edit",
	"bash":      "bash",
	"glob":      "glob",
	"grep":      "grep",
}

// droppedTools are the names, lowercased, a persona's list may hold that are
// dropped without a word: Claude Code's tools craze has no counterpart for, or
// withholds from every child by design — starting another agent or reading a
// background one's result, the todo list, asking the person, leaving plan mode
// — and the native ids of those five, since a list may be written in either
// vocabulary. Every name starting "task" and every MCP name ("mcp__…") is
// dropped as well (droppedTool).
var droppedTools = []string{
	AgentTool, "todowrite", "askuserquestion", "exitplanmode",
	"webfetch", "websearch", "notebookedit", "notebookread",
	"killshell", "bashoutput", "workflow", "skill",
	"todo_write", "ask_user_question", ExitPlanModeTool, AgentOutputTool,
}

// droppedTool reports whether a persona's tool name, lowercased and without
// its parenthesised part, is one MapClaudeTools drops silently.
func droppedTool(base string) bool {
	return slices.Contains(droppedTools, base) || strings.HasPrefix(base, "task") || strings.HasPrefix(base, "mcp__")
}

// MapClaudeTools turns a persona's tools (or disallowedTools) list into native
// tool ids (plan 026 §3.4). It is the one vocabulary the harness and the
// adapter share: the adapter calls it when it reads a persona file, where it
// can report what it could not map, and hands the harness only native ids.
//
// Names match case-insensitively. Read and LS are read, Write is write, Edit
// and MultiEdit are edit, and Bash, Glob and Grep are themselves; a native id
// is taken verbatim. Dropped silently: Agent, Agent(…), Task and every name
// that starts with it, TodoWrite, AskUserQuestion, ExitPlanMode, WebFetch,
// WebSearch, NotebookEdit, NotebookRead, KillShell, BashOutput, Workflow,
// Skill, every MCP name, and the native ids of the four tools a child never
// gets. Every other name is returned in unknown, for the caller's one
// diagnostic, and not mapped — a name with a parenthesised restriction
// included, Bash(git status:*) say: craze cannot honour the restriction, and
// the unrestricted tool would give the child more than its author wrote, so it
// fails closed, as a tools key craze cannot read does. Surrounding spaces are
// trimmed and an empty name is skipped. Both results are deduplicated, first
// occurrence kept, in the list's order; both are nil when empty, and an empty
// ids for a tools key is a text-only child.
//
// It is the allow list's reading. A deny list reads a restriction the other
// way round (MapClaudeDisallowed).
func MapClaudeTools(names []string) (ids, unknown []string) {
	return mapClaudeNames(names, false)
}

// MapClaudeDisallowed is MapClaudeTools for a persona's disallowedTools list:
// the same vocabulary, the same silent drops and the same unknown names, with
// one difference that fails closed the other way. A name with a parenthesised
// restriction — Bash(rm:*) say — takes away the whole of the tool it
// restricts, bash, rather than being reported unknown and dropped (review r2
// of C3a, finding 2).
//
// On an allow list, not granting Bash(git status:*) at all is the closed
// reading: craze cannot honour the restriction (H3), and granting bash whole
// would give the child more than its author wrote. On a deny list the same
// drop is the open reading: a persona with no tools key and
// disallowedTools: Bash(rm:*) would keep an unrestricted bash its author meant
// to fence. Taking the whole tool away gives the child less than the author
// wrote, which is the side craze errs on. A restricted name that is dropped
// silently (Agent(…), WebFetch(domain:…)) stays dropped: those are tools no
// child has anyway, so there is nothing to take away.
func MapClaudeDisallowed(names []string) (ids, unknown []string) {
	return mapClaudeNames(names, true)
}

// mapClaudeNames is both readings. deny is the deny list's: a known name with
// a restriction maps to its tool's id instead of being reported unknown.
func mapClaudeNames(names []string, deny bool) (ids, unknown []string) {
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		base, restricted := name, false
		if i := strings.IndexByte(name, '('); i >= 0 {
			base, restricted = strings.TrimSpace(name[:i]), true
		}
		low := strings.ToLower(base)
		id, known := claudeToolIDs[low]
		switch {
		case droppedTool(low):
		case known && (!restricted || deny):
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		default:
			if !slices.Contains(unknown, name) {
				unknown = append(unknown, name)
			}
		}
	}
	return ids, unknown
}
