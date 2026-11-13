package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/sessions"
)

// The model facts a waiting command reads for itself (plan 050 C2), from a
// native model table this process loads — never the host's, whose notes go
// to its log and whose start errors carry only text and a kind:
//
//   - craze new --wait's pre-check, on provider native, before any host is
//     spawned: a --model that names nothing, or names a retired alias; an
//     explicit --effort the model does not list; --fast, which no native model
//     has. Each is exit 1, kind config, with a reason in the error's detail.
//     It is the one place --effort and --fast are strict: the host is not
//     changed, nothing is recorded for a reload, and craze send, a reload, the
//     TUI, craze prompt and a plain craze new stay lenient. A table this
//     process cannot load skips it, and the host reports as it always did.
//     A --web-search that cannot be honoured (plan 051 S2) is refused ahead
//     of it, before the prompt is read, and wins over a model refusal:
//     kind config too, its reason web_search_not_offered (configRefusal).
//   - The notices: a result or an exit-1 failure about a native session whose
//     model is deprecated says so on stderr and, under --json, in `warnings`.
//     Best-effort: a table that does not load says nothing, and never changes
//     a status or an exit code.

// The reasons a start check's config failure gives in its detail: a closed
// set, and no other failure carries one. The first four are a model's (plan
// 050 C2), and carry the model too (modelRefusal); web_search_not_offered is
// a --web-search that cannot be honoured (plan 051 F.2, S1), which is about
// no model and carries the reason alone (configRefusal). cli.md lists them
// all (TestCLIReferenceNamesEveryReason).
const (
	reasonModelUnknown        = "model_unknown"
	reasonModelRetired        = "model_retired"
	reasonEffortNotOffered    = "effort_not_offered"
	reasonFastNotOffered      = "fast_not_offered"
	reasonWebSearchNotOffered = "web_search_not_offered"
)

// modelsCatalog is the catalog every model fact the CLI reads is merged over:
// the one compiled in. A seam: a test's synthetic catalog (the orchestrator
// fixtures).
var modelsCatalog = modeltable.ShippedCatalog

// errNoCrazeDir is a model table with no directory to load from.
var errNoCrazeDir = errors.New("there is no craze directory to read the model table from (set HOME or CRAZE_HOME)")

// loadNativeTable is this craze directory's native model table over
// modelsCatalog, as a session's host loads it (paths.NativeDir: the user's
// files and the cached account lists).
func loadNativeTable() (*modeltable.Table, error) {
	dir := paths.NativeDir()
	if dir == "" {
		return nil, errNoCrazeDir
	}
	cat, err := modelsCatalog()
	if err != nil {
		return nil, err
	}
	return modeltable.LoadWith(dir, cat)
}

// modelRefusal is a model-shaped config failure (plan 050 C2): the message
// the host would give, and the detail a program reads instead of it.
type modelRefusal struct {
	msg        string
	reason     string
	model      string
	replacedBy string
}

func (e *modelRefusal) Error() string { return e.msg }

// detail is the refusal as the error's detail: model always there, "" when
// nothing was named or the name held nothing of an alias's characters.
func (e *modelRefusal) detail() *errorDetail {
	model := e.model
	return &errorDetail{Reason: e.reason, Model: &model, ReplacedBy: e.replacedBy}
}

// exit is the refusal as command what's exit 1, kind config, its message on
// one line.
func (e *modelRefusal) exit(what string) error { return refusalExit(what, e) }

// refusalExit is a start check's refusal e as command what's exit 1, kind
// config, its message on one line, e its cause (refusalDetail reads it).
func refusalExit(what string, e error) error {
	return &exitError{code: 1, msg: what + ": " + sanitizeLine(e.Error()), kind: errkind.Config, cause: e}
}

// configRefusal is a start check's config failure that is not about a model
// (plan 051 S1): web_search_not_offered. Its detail is the reason alone — no
// model, which modelRefusal always gives.
type configRefusal struct {
	msg    string
	reason string
}

func (e *configRefusal) Error() string { return e.msg }

func (e *configRefusal) detail() *errorDetail { return &errorDetail{Reason: e.reason} }

// webRefusal is a --web-search that cannot be honoured (err, an
// *agent.WebRefusal: agent.CheckWebFlag's) as command what's exit 1, kind
// config, reason web_search_not_offered — which only a waiting command's
// --json prints (refusalDetail). craze serve's refusal reaches its spawner as
// its message alone, so craze new --wait's backstop has no detail.
func webRefusal(what string, err error) error {
	return refusalExit(what, &configRefusal{msg: err.Error(), reason: reasonWebSearchNotOffered})
}

// refusalDetail is the detail err's start-check refusal gives — a model's or
// another config failure's — nil when err is none.
func refusalDetail(err error) *errorDetail {
	var r *modelRefusal
	if errors.As(err, &r) {
		return r.detail()
	}
	var c *configRefusal
	if errors.As(err, &c) {
		return c.detail()
	}
	return nil
}

// nativeModelMiss is raw, a --model MatchNativeModel refused with err, as a
// refusal on the two paths a program reads (a waiting command's pre-check,
// craze models <alias>): model_retired with the row's alias and replacement
// when raw names a retired alias, in the host's own words
// (agent.NativeModelRefusal); and otherwise model_unknown with raw as given,
// held to an alias's characters, its message short — the miss and where the
// models are listed, not the whole table, which would be most of what a
// program's one-line reason shows (the host's own start error, which a
// person reads, keeps the list).
func nativeModelMiss(t *modeltable.Table, raw string, err error) *modelRefusal {
	msg, r, retired := agent.NativeModelRefusal(t, raw, err)
	if retired {
		return &modelRefusal{msg: msg, reason: reasonModelRetired, model: detailModel(r.Alias), replacedBy: detailModel(r.ReplacedBy)}
	}
	return &modelRefusal{msg: "native: " + err.Error() + "; craze models lists the models", reason: reasonModelUnknown, model: detailModel(raw)}
}

// detailModel is s as a detail's model: its ASCII letters, digits and
// . _ - / : @ + alone, at most modeltable.MaxAliasLen bytes — never text from
// outside craze.
func detailModel(s string) string {
	var b strings.Builder
	for _, c := range []byte(strings.TrimSpace(s)) {
		if b.Len() == modeltable.MaxAliasLen {
			break
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("._-/:@+", c) >= 0:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// nativeStartCheck is craze new --wait's pre-check on provider native (the
// file's comment): model, effort and fast as the command was given them, model
// and effort trimmed. It answers the alias --model resolves to ("" with no
// --model, or one that resolves to nothing), which the notices are about until
// the session names its model — on an effort's or --fast's refusal too — and
// the refusal as command what's exit. A table that does not load checks
// nothing.
func nativeStartCheck(what, model, effort string, fast bool) (string, error) {
	t, err := loadNativeTable()
	if err != nil {
		return "", nil
	}
	alias := ""
	if model != "" {
		// Matched exactly as the host's start matches it.
		if alias, err = agent.MatchNativeModel(t, model); err != nil {
			return "", nativeModelMiss(t, model, err).exit(what)
		}
		if effort != "" && !offersEffort(t.Models[alias].Efforts, effort) {
			return alias, effortRefusal(alias, effort, t.Models[alias].Efforts).exit(what)
		}
	}
	if fast {
		msg := "--fast: no native model has a fast variant"
		if alias != "" {
			msg = "--fast: " + alias + " has no fast variant"
		}
		return alias, (&modelRefusal{msg: msg, reason: reasonFastNotOffered, model: detailModel(alias)}).exit(what)
	}
	return alias, nil
}

// offersEffort says levels hold effort as a native start matches it (the
// session's effort option: its value, in any case).
func offersEffort(levels []string, effort string) bool {
	for _, l := range levels {
		if strings.EqualFold(l, effort) {
			return true
		}
	}
	return false
}

// effortRefusal is an --effort the model alias does not list.
func effortRefusal(alias, effort string, levels []string) *modelRefusal {
	msg := fmt.Sprintf("--effort %s: %s offers %s", effort, alias, strings.Join(levels, ", "))
	if len(levels) == 0 {
		msg = fmt.Sprintf("--effort %s: %s has no effort control", effort, alias)
	}
	return &modelRefusal{msg: msg, reason: reasonEffortNotOffered, model: detailModel(alias)}
}

// ------------------------------------------------------------ the notices

// warningModelDeprecated is the one kind a warning has (plan 050 C2).
const warningModelDeprecated = "model_deprecated"

// maxWarnings bounds `warnings`.
const maxWarnings = 8

// resultWarning is one entry of a waiting command's `warnings`.
type resultWarning struct {
	Kind       string `json:"kind"`
	Model      string `json:"model"`
	ReplacedBy string `json:"replacedBy"`
	Message    string `json:"message"`
}

// storedAsked is the native model a reload asks for again, the --model an
// unattended session's row recorded (row.Unattended.Model), matched as the
// host's start matches it, for the notices to judge while the reloaded
// session has named no model of its own — its reload failing included (plan
// 050 review): "" when the row is not a native session's, recorded no model,
// or names one the table (noticeTable) does not match.
func storedAsked(row sessions.Row) string {
	if row.Provider != agent.NativeProvider().Name() || row.Unattended == nil || row.Unattended.Model == "" {
		return ""
	}
	t, err := noticeTable()
	if err != nil {
		return ""
	}
	alias, err := agent.MatchNativeModel(t, row.Unattended.Model)
	if err != nil {
		return ""
	}
	return alias
}

// noticeTable is the table the notices read: this process's own. A seam.
var noticeTable = loadNativeTable

// modelWarnings are the warnings about a session of provider on model: one
// model_deprecated when provider is native and model a deprecated model of
// the table (noticeTable), and none otherwise — none too when the table does
// not load.
func modelWarnings(provider, model string) []resultWarning {
	if provider != agent.NativeProvider().Name() || model == "" {
		return nil
	}
	t, err := noticeTable()
	if err != nil {
		return nil
	}
	replacedBy, sentence, ok := t.DeprecatedNotice(model)
	if !ok {
		return nil
	}
	return []resultWarning{{Kind: warningModelDeprecated, Model: model, ReplacedBy: replacedBy, Message: sanitizeLine(sentence)}}
}

// notice is o's notices about a session of provider on model — the model
// craze new --wait's pre-check matched when the session has named none — each
// said on o.notes as one line under the command's name, and answered for
// `warnings`. A command with no notes says none.
func (o resultOutput) notice(provider, model string) []resultWarning {
	if o.notes == nil {
		return nil
	}
	if model == "" && o.asked != "" {
		provider, model = agent.NativeProvider().Name(), o.asked
	}
	w := capWarnings(modelWarnings(provider, model))
	for _, x := range w {
		fmt.Fprintf(o.notes, "%s: %s\n", o.what, x.Message)
	}
	return w
}

// capWarnings is w held to maxWarnings.
func capWarnings(w []resultWarning) []resultWarning {
	if len(w) > maxWarnings {
		return w[:maxWarnings]
	}
	return w
}
