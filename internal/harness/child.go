package harness

import (
	"slices"
	"strings"
	"sync/atomic"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// Sub-agents' sessions (plan 026 §3.2, §3.5). A child is an ordinary Session,
// opened with Options.Child set by the runner that starts it for the parent's
// agent tool: it has its own model, its own transcript, its own tools and its
// own turn, and it is a child for its whole life. What makes it one is decided
// here and in Open, once:
//
//   - its session id is the runner's (ChildOptions.ID), so the parent knows
//     it before the child writes anything, and its transcript's header links
//     it to the parent's session and call;
//   - its tools are the profile's less the four a child never gets, then only
//     the ones its type lists (keeps);
//   - its system prompt is the parent's frozen string with a role section
//     after it (withChildRole), so the two are byte-equal up to the role;
//   - its mode is the parent's when it opened and never changes, except that
//     the parent's later switches tighten its gate (tool.NewChildModeGate); in
//     plan mode it has no plan file and a reminder of its own (reminders.go);
//   - it has nobody to ask and no todo list, and it shares its parent's
//     path-lock table, so their edits of one file serialize.
//
// Everything else — home, workspace, table, model client, environment, clock,
// version — arrives through the ordinary Options, the parent's own values,
// and the redaction keys are computed from the table as for any session.

// ChildOptions opens a session as a sub-agent of another. Set only by the
// runner (subagents.go); a session with Child set is a child for its whole
// life.
type ChildOptions struct {
	// ID is the child's session id, minted by the runner: its transcript's
	// name and header id, SubagentInfo.ID and Event.Agent. It must be usable
	// in a file name; "" refuses Open.
	ID string
	// ParentSession is the parent's harness session id, and ParentCall the
	// parent's harness id for the agent call that started the child
	// (ToolStarted.ID). The header records both (parent_session,
	// parent_tool_call).
	ParentSession string
	ParentCall    string
	// Type is the resolved agent type's name (subagent_type), and PersonaPath
	// the file it came from (persona_path), "" for a built-in: provenance in
	// the header, nothing more.
	Type        string
	PersonaPath string
	// Role is the persona's body or the built-in's role text, frozen into the
	// role section (renderChildRole).
	Role string
	// AllTools gives the child every tool a child may have; it is true only
	// when the persona has no tools: key (§3.4). Otherwise Tools, native tool
	// ids the adapter mapped, narrows them, and an empty Tools is a text-only
	// child: it fails closed.
	AllTools bool
	Tools    []string
	// BaseSystem is the parent's frozen system prompt and BaseProfile the tool
	// profile it was written for. A child whose model resolves that profile
	// sends BaseSystem verbatim with its role section after it; one whose
	// model resolves another builds its own from Options.Prompt, which the
	// runner fills with a clone of the parent's extras (§3.2).
	BaseSystem  string
	BaseProfile string
	// Mode is the parent's mode when the child opens; Options.Mode is not
	// read for a child. SetMode on a child is ErrChildMode (§3.5).
	Mode string
	// Strictness is the child's monotonic strictness, which the parent's
	// SetMode raises through the runner's registry (tool.Raise); the child's
	// gate judges each call under the stricter of it and Mode. nil tightens
	// nothing.
	Strictness *atomic.Int32
	// Locks is the parent's path-lock table, shared; nil gives the child one
	// of its own.
	Locks *tool.PathLocks
}

// childWithheld are the tools a child never gets (owner decision 2): agent,
// because depth is 1 by construction (§3.2, crush's structural shape); the
// todo list and the two tools that block on a person, because a child has
// neither and nobody to ask. agent is filtered by name before any profile
// registers it, so the rule holds the day one does.
var childWithheld = []string{tool.AgentTool, "todo_write", "ask_user_question", tool.ExitPlanModeTool}

// keeps reports whether a session opened with c offers the tool with id:
// every tool for an ordinary session (c nil), and for a child every tool but
// the withheld ones, narrowed to c.Tools unless c.AllTools. The filter runs
// before both the specs the model is offered and the dispatcher's tools are
// built, so a tool it drops is neither advertised nor dispatchable (astra
// panel r1 #23).
func (c *ChildOptions) keeps(id string) bool {
	switch {
	case c == nil:
		return true
	case slices.Contains(childWithheld, id):
		return false
	}
	return c.AllTools || slices.Contains(c.Tools, id)
}

// The role section: a sub-agent's system prompt ends with it, after
// everything the parent's has (§3.2). It is craze's heading and preamble and
// then the role, which is the one part a file wrote.
const (
	childRoleHeading  = "# Your role as a sub-agent"
	childRolePreamble = "You were started by another agent to do one task. Your final message is\n" +
		"returned to it as the result: make it complete and self-contained. You cannot\n" +
		"ask the user anything.\n"

	// maxChildRole bounds the role's text, the marker of a cut included. It is
	// the section's own budget, not the instructions' shared 96 KiB: a role is
	// not an instruction document, and charging it there would put it before
	// the catalog and let a workspace's instructions starve it (panel astra
	// 11). The heading and the preamble are craze's framing and cost it
	// nothing, as the documents' headings cost theirs nothing.
	maxChildRole = 32 << 10
)

// childFramingTexts are the headings a role is escaped against: every one of
// craze's that comes before it in the prompt, and its own, which a role that
// wrote it could use to start a second role section in craze's voice.
var childFramingTexts = append(slices.Clone(framingTexts), "your role as a sub-agent")

// renderChildRole is the role section. The role is hostile input in the same
// sense an instruction document is — a persona file a plugin or a repository
// wrote — so it is rendered the way renderInstructions renders one: line
// endings normalised, a line that would forge craze's framing escaped, the
// result redacted and only then measured and cut, since every one of those
// can change its length (X14's rule). A role with nothing in it leaves the
// heading and the preamble, which are the whole contract with the parent.
func renderChildRole(role string, red *redact.Replacer) string {
	section := childRoleHeading + "\n\n" + childRolePreamble
	text := red.String(escapeFramingOf(normalizeLines(role), childFramingTexts))
	if strings.TrimSpace(text) == "" {
		return section
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return section + "\n" + cutToBudget(text, maxChildRole)
}

// withChildRole is a sub-agent's whole system prompt: base — the parent's
// frozen string, or the child's own for another profile — and the role
// section after it, separated by the blank line withPromptExtras puts between
// its sections, so base is a byte-identical prefix (A2).
//
// The role was redacted on its own, and this is the whole-string check
// withPromptExtras makes for a parent (panel astra 11): a key can be
// reconstructed across the join — base's last bytes and the heading, the
// preamble and the role's first bytes, a cut and the truncation marker — and
// base itself can hold a key the parent did not know when it froze it, since
// this child resolved its keys at its own Open. None of those can be redacted
// here: base must stay the parent's bytes, and the framing is craze's. So a
// prompt the redactor would change refuses the child (errChildPromptKey), and
// the parent's model reads a failed sub-agent.
func withChildRole(base, role string, red *redact.Replacer) (string, error) {
	out := base + "\n" + renderChildRole(role, red)
	if red.String(out) != out {
		return "", errChildPromptKey
	}
	return out, nil
}
