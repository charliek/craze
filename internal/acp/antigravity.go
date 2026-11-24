package acp

import (
	"encoding/json"
	"regexp"
)

// The antigravity dialect (plan 053 § B): agy, the Antigravity CLI's `agy
// acp`, speaks plain ACP v1. What is its own is small: the tool name rides at
// the top level of every tool_call and tool_call_update (`name`), questions
// arrive as elicitation/create forms keyed q<i>/q<i>_other — the form
// capability it is told of at initialize (Initialize) — and the requests it
// sends are numbered with strings, which the client answers by echoing the id
// as it came. Everything else — permissions, the session updates, cancel,
// load — is the generic path (probe 1.5.14).

// antigravityKeys is agy's question scheme (frames-05): q<i>, 1-based, the
// field's title the question itself, and q<i>_other the optional free-form
// answer beside it. The option consts are 1-based strings too ("1", "2", …),
// which the strict parser reads like any other string const.
var antigravityKeys = elicitationKeys{
	pattern:       regexp.MustCompile(`^q([1-9][0-9]{0,3})(_other)?$`),
	textSuffix:    "_other",
	titleIsPrompt: true,
}

// antigravityToolName is the tool's name as agy spells it, at the update's
// top level (read_file, edit_file, write_file, run_command, ask_question,
// invoke_subagent, schedule, manage_task): every update of a call repeats it,
// so it is read off each one, as grokToolName reads grok's _meta. A name that
// is not a string is no name.
func antigravityToolName(update json.RawMessage) string {
	var w struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(update, &w); err != nil {
		return ""
	}
	return w.Name
}
