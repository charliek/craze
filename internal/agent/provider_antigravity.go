package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/tool"
)

// The antigravity provider (plan 053): agy, the Antigravity CLI, over its own
// ACP server (`agy acp`), on the user's own signed-in Google account. agy 1.5
// speaks plain ACP v1, so what is antigravity's alone is small and lives here:
// its tables, the auth error a logged-out agy answers with, the command rows'
// shape (run_command's header and exit code), the brain directory its plans
// and todo lists are written to, and the sub-agent rows its invoke_subagent
// calls become. It is hidden (plan 053 M1): `--provider antigravity` resolves
// it, and no listing shows it yet.

const (
	antigravityName = "antigravity"
	// antigravityBin is agy's executable, looked up on PATH like the others;
	// [agents].antigravity, --agent-bin and $CRAZE_AGENT_BIN point at another.
	antigravityBin = "agy"
	// antigravityLoginHint is agy's terminal login (plan 053 § A3): it prints
	// a URL and reads the pasted code. Every method initialize advertises is
	// interactive or needs a key, so craze never calls authenticate.
	antigravityLoginHint = "agy acp --login"
	// antigravityRunCommand is agy's shell tool, whose output carries a header
	// of its own (§ D1).
	antigravityRunCommand = "run_command"
	// antigravitySubagentTool launches one of agy's sub-agents (§ D4).
	antigravitySubagentTool = "invoke_subagent"
)

// antigravityModes are agy's four modes (§ C1), in its order: Default, accept
// edits and auto implement — they differ only in what agy asks before — and
// plan plans. None is read-only, so --ask is refused before the spawn and /ask
// finds no mode to resolve to.
var antigravityModes = []string{"default", "accept-edits", "auto", "plan"}

var antigravityModeKinds = map[string]ModeKind{
	"default":      ModeImplement,
	"accept-edits": ModeImplement,
	"auto":         ModeImplement,
	"plan":         ModePlan,
}

// AntigravityProvider is agy (plan 053 § A). It is hidden until its listing
// milestone (M2), as claude was through its first ones: resolvable by id,
// shown by no listing. Under craze's default --force it runs `agy acp
// --dangerously-skip-permissions`, under --no-force `agy acp`; the user's own
// agy login and HOME are used, and the environment gains nothing but the
// caller marker (spawnEnv). Its capabilities (§ A4): questions over
// elicitation, modes, effort (thought_level), the parameterized picker and the
// sub-agent rows its invoke_subagent calls become — no fast toggle, todos,
// plan cards, interjection, or a sub-agent's transcript, stop or background
// presentation, none of which agy exposes over ACP.
//
// Skills are agy's own (§ A5): it advertises its built-in and workspace
// skills in available_commands_update and expands them itself, so there is
// no skill or plugin scan.
//
// A load of an id agy no longer has comes back as an empty session with no
// error (§ C5, probe § 6): craze loads only ids its own index holds, so this
// is a session deleted outside craze, and it is the documented exception to
// the no-silent-new-session rule (loadSession).
func AntigravityProvider() Provider {
	return Provider{
		name:        antigravityName,
		displayName: antigravityName,
		modeKinds:   antigravityModeKinds,
		startModes:  antigravityModes,
		// A plan-mode turn ends at agy's plan file with no card: the offer
		// leaves plan mode for default and sends cursor's sentence (§ C1).
		implementPrompt: cursorImplementPrompt,
		defaultBins:     []string{antigravityBin},
		spawnArgs:       []string{"acp"},
		forceAfter:      1,
		forceArgs:       []string{"--dangerously-skip-permissions"},
		loginHint:       antigravityLoginHint,
		hidden:          true,
		resumable:       true,
		capabilities: Capabilities{
			Effort:              true,
			Modes:               true,
			AskCards:            true,
			SubagentRows:        true,
			ParameterizedPicker: true,
		},
		dialect:           acp.DialectAntigravity,
		subagentToolNames: []string{antigravitySubagentTool},
		configModels:      true,
	}
}

// isAntigravity reports whether p is the antigravity provider.
func (p Provider) isAntigravity() bool { return p.name == antigravityName }

// restoresAfterLoad reports whether a load of p's session restores the model
// and effort its index row recorded by setting them after the load (plan 053
// § C4): agy ignores configOptions in session/load's params and answers on its
// default (frames-14), where claude takes them in the request itself
// (sessionParams).
func (p Provider) restoresAfterLoad() bool { return p.isAntigravity() }

// ------------------------------------------------------------ auth (§ A3)

// agyAuthRequired is the words of the error a logged-out agy answers
// session/new, session/load, session/list and a prompt with, under -32000.
const agyAuthRequired = "authentication required"

// classifyAuth is a start's or a prompt's failure as p says it is (plan 053
// § A3): agy's -32000 whose message says authentication is required is an
// auth error naming the login to run. -32000 is the generic server error in
// JSON-RPC, so the message must say it too; any other error, and every other
// provider's, is left as it is (claude's prompt errors have their own table,
// ClaudeErrorInfo).
func (p Provider) classifyAuth(err error) error {
	if !p.isAntigravity() || err == nil {
		return err
	}
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) || rpc == nil || rpc.Code != codeAuthRequired ||
		!strings.Contains(strings.ToLower(rpc.Message), agyAuthRequired) {
		return err
	}
	return &authRequiredError{err: rpc, hint: p.loginHint}
}

// authRequiredError is classifyAuth's: the agent's words, its kind auth, and
// the login to run.
type authRequiredError struct {
	err  *acp.RPCError
	hint string
}

func (e *authRequiredError) Error() string {
	return fmt.Sprintf("agent: %s (run `%s`)", sanitizeLine(e.err.Message), e.hint)
}
func (e *authRequiredError) Unwrap() error           { return e.err }
func (e *authRequiredError) ErrorInfo() errkind.Info { return errkind.New(errkind.Auth) }

// ------------------------------------------------------------ rows (§ D1)

// antigravityInput is agy's rawInput as far as a row reads it: the command
// line a run_command names, and the plan-file flag a brain write carries
// (§ D3).
type antigravityInput struct {
	CommandLine      *string `json:"CommandLine"`
	ArtifactMetadata *struct {
		RequestFeedback bool `json:"RequestFeedback"`
	} `json:"ArtifactMetadata"`
}

func parseAntigravityInput(raw json.RawMessage) antigravityInput {
	var in antigravityInput
	_ = json.Unmarshal(raw, &in)
	return in
}

// requestsFeedback is the plan-file flag: agy asks for feedback on its plan
// and on nothing else it writes (frames-09, -10, -15). No flag is no plan.
func (in antigravityInput) requestsFeedback() bool {
	return in.ArtifactMetadata != nil && in.ArtifactMetadata.RequestFeedback
}

// rowText is a row's command text from agy's rawInput raw, read as in: its
// CommandLine when it has one, else the generic rendering (stringifyRawInput),
// which knows only lower-case keys.
func (in antigravityInput) rowText(raw json.RawMessage) string {
	if in.CommandLine != nil {
		return truncateUTF8(sanitizeText(*in.CommandLine), rawInputCap)
	}
	return stringifyRawInput(raw)
}

// agyCommandHeader is the block run_command's output starts with: when it was
// made and finished, how the command exited, and the line before the output
// itself (probe § 3). Only that exact, complete leading block is stripped —
// all five lines, the blank one included: an output that merely starts like
// it, or says "Command output:" later, is the command's own (review M1 F5).
var agyCommandHeader = regexp.MustCompile(`\ACreated At:[^\n]*\nCompleted At:[^\n]*\nCommand exited with code (-?[0-9]+)\.\n\nCommand output:\n`)

// antigravityCommandText is run_command's output as a row shows it: CRLF line
// ends as LF, and the header gone. code is the exit code the header names, and
// ok whether it had one.
func antigravityCommandText(s string) (text string, code int, ok bool) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	m := agyCommandHeader.FindStringSubmatchIndex(s)
	if m == nil {
		return s, 0, false
	}
	n, err := strconv.Atoi(s[m[2]:m[3]])
	return s[m[1]:], n, err == nil
}

// antigravityOutput is agy's rawOutput, {output, exit_code}, as a row's
// output (§ D1): output normalised (antigravityCommandText) as stdout, and
// exit_code as the exit code, else the header's. A rawOutput of any other
// shape — an edit's {} — is read the generic way (parseToolOutput).
func antigravityOutput(raw json.RawMessage) *ToolOutput {
	trimmed, present := presentJSON(raw)
	if !present || trimmed[0] != '{' {
		return parseToolOutput(raw)
	}
	var w struct {
		Output   *string `json:"output"`
		ExitCode *int    `json:"exit_code"`
	}
	if json.Unmarshal(trimmed, &w) != nil || (w.Output == nil && w.ExitCode == nil) {
		return parseToolOutput(raw)
	}
	var text string
	code, headed := 0, false
	if w.Output != nil {
		text, code, headed = antigravityCommandText(*w.Output)
	}
	if w.ExitCode != nil {
		code, headed = *w.ExitCode, true
	}
	stdout := sanitizeText(text)
	switch {
	case headed:
		return cappedToolOutput(stdout, "", "", &code)
	case stdout != "":
		return cappedToolOutput(stdout, "", "", nil)
	}
	return nil
}

// antigravityContentText is a tool's mirrored text content as a row shows it:
// run_command's normalised as its rawOutput is; any other tool's as it is.
func antigravityContentText(raw json.RawMessage, toolName string) string {
	if toolName != antigravityRunCommand {
		return contentText(raw)
	}
	text, _, _ := antigravityCommandText(contentTextRaw(raw))
	return tailUTF8(sanitizeText(text), contentTextCap)
}

// ------------------------------------------------------------ brain (§ D3)

// antigravityBrainRoot is the directory agy writes its plans, todo lists and
// other artifacts under, for an agent that sees getenv: $HOME/.gemini/
// antigravity-cli/brain, resolved through symlinks as grok's session root is
// (isGrokPlanFile; /var against /private/var on macOS). "" when there is no
// home. A custom --gemini-dir is never passed by craze (plan 053 R4).
func antigravityBrainRoot(getenv func(string) string) string {
	dot := homeDotDir(getenv, ".gemini")
	if dot == "" {
		return ""
	}
	root := filepath.Join(dot, "antigravity-cli", "brain")
	if real, err := tool.RealPath(root); err == nil {
		root = real
	}
	return root
}

// antigravityBrainCheck is the classification of one call's paths (§ D3):
// isArtifact says a resolved path is in session sid's own brain directory,
// and isPlan that it is there and the call asked for feedback on it
// (rawInput.ArtifactMetadata.RequestFeedback) — the plan file. A path in
// another session's brain directory is neither: an outside edit like any
// other.
func antigravityBrainCheck(root, sid string, feedback bool) (isPlan, isArtifact func(string) bool) {
	if root == "" || sid == "" {
		return nil, nil
	}
	isArtifact = strictlyUnder(filepath.Join(root, sid))
	if feedback {
		isPlan = isArtifact
	}
	return isPlan, isArtifact
}

// ------------------------------------------------------------ sub-agents (§ D4)

// toolBackedRows reports whether p's sub-agents are rows backed by the tool
// call that launched them, nothing else of theirs reaching the wire: cursor's
// task (whose receipt fills in the rest) and agy's invoke_subagent.
func (p Provider) toolBackedRows() bool { return p.dialect == acp.DialectCursor || p.isAntigravity() }

// rowsOutliveCall reports whether p's tool-backed rows run on past their
// call, to the end of the turn that launched them (syncToolRowLocked): agy's,
// whose call completes at the launch.
func (p Provider) rowsOutliveCall() bool { return p.isAntigravity() }

// agySubagentInput is invoke_subagent's rawInput (frames-06): the role is the
// row's description, TypeName its type; Model is optional and was never seen.
type agySubagentInput struct {
	Prompt   string `json:"Prompt"`
	Role     string `json:"Role"`
	TypeName string `json:"TypeName"`
	Model    string `json:"Model"`
}

func antigravityTask(raw json.RawMessage) *TaskInfo {
	var in agySubagentInput
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	return &TaskInfo{
		Description:  sanitizeText(in.Role),
		Prompt:       truncateUTF8(sanitizeText(in.Prompt), taskPromptCap),
		SubagentType: sanitizeText(in.TypeName),
		Model:        sanitizeText(in.Model),
	}
}
