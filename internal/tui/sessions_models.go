package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// `/provider` and `/model` in the session list's input (plan 030 §3.14, owner
// decision 9): what new sessions from the list run. `/` at the start of the
// input opens the list's commands — `/provider`, `/model`, and `/exit`, which
// the input already takes — in the completion popup (complete.go); choosing
// `/provider` or `/model` lists its values; a value chosen applies to every
// session started from the list (dispatch.go's sessNewSpec, the unstarted
// session) until it is changed or craze quits, and the input's rule names it.
//
//   - /provider lists the providers the startup picker lists (Model.providers:
//     pickerRows over the availability-filtered list the launch passes) — or,
//     once the list's ProviderAvailabilitySource has answered (plan 036
//     §3.3), the providers its latest answer names, a gx installed or gone
//     since the launch among them: one that is not ready is dimmed with its
//     reason and refused when chosen (sessProviderChoices).
//     Choosing one resets the model to that provider's default: native's is
//     read from its model table off the Update (nativeDefaultModel) — the
//     model a native session with no --model starts on — and an ACP
//     provider's is its agent's own, no --model at all, drawn `default`.
//   - /model lists the provider's models: native's from its model table
//     (nativeModelChoices); an ACP provider's from the catalog cache a
//     detached host wrote when a session of it last installed one
//     (SessionStarter.ModelCatalog), titled with how long ago that was — an
//     ACP agent names its models only once a session has started (discovery
//     data.md §5). With no cache, or no candidate matching, what is typed is
//     applied as typed at the next start, exactly as --model would be, and a
//     bad id fails the way --model does (§3.14).
//   - A model chosen belongs to the provider it was chosen for: /model pins
//     the provider it listed, so a list later opened over a session of
//     another provider still starts the model's own.
//   - Typed in full, `/provider <id>` and `/model <id>` do the same with no
//     popup; any other `/…` line is a prompt like any other (X152).
//   - `/effort` (default, low, medium, high, xhigh) and `/fast` (default, on,
//     off) set the effort and fast mode new sessions start at (plan 032
//     §3.11, C17) the same way — a popup of values, or typed in full — and
//     last as the pick does. `default` clears the setting: the model's own.
//     Neither is checked against a provider here: a host's start matches the
//     value against what the model offers, and notes it when nothing does.
//     They are revealed by prefix, never listed by a bare `/`
//     (sessCmdRevealed).

// The list's commands.
const (
	sessCmdProvider = "provider"
	sessCmdModel    = "model"
	sessCmdExit     = "exit"
	sessCmdEffort   = "effort"
	sessCmdFast     = "fast"
)

// sessCmdOlder are the list's first commands (plan 030 C16), the ones a bare
// `/` lists; sessCmdLater the ones added after them (plan 032 C17).
var (
	sessCmdOlder = []string{sessCmdProvider, sessCmdModel, sessCmdExit}
	sessCmdLater = []string{sessCmdEffort, sessCmdFast}
)

// sessCmdRevealed is the reveal rule for a later command (plan 032 §3.11): it
// is listed only once the typed command word is a prefix of it and of no
// older command. A bare `/` (the empty word, a prefix of every command) and
// `/e` (a prefix of /exit too) therefore list exactly what they did before
// C17 — `/`'s popup keeps its frames — while `/ef` reveals /effort and `/f`
// /fast.
func sessCmdRevealed(cmd, word string) bool {
	starts := func(c string) bool { return strings.HasPrefix(c, word) }
	return starts(cmd) && !slices.ContainsFunc(sessCmdOlder, starts)
}

// The values /effort and /fast offer, `default` (sessDefaultModel's word)
// first: the setting cleared.
var (
	sessEffortLevels = []string{sessDefaultModel, "low", "medium", "high", "xhigh"}
	sessFastValues   = []string{sessDefaultModel, sessFastOn, sessFastOff}
)

const (
	sessFastOn  = "on"
	sessFastOff = "off"
)

// sessCmdSourceID names the `/` popup's source: a load's result is stamped
// with it (the catalog's, the model table's), and the list takes only its own
// sources' (applySessMsg).
const sessCmdSourceID = "sessions-commands"

// The popup's and the hint line's words.
const (
	sessCmdTitle       = "commands"
	sessProvidersTitle = "provider for new sessions"
	sessModelsTitle    = " models for new sessions"
	sessCurrentNote    = "current"
	sessDefaultModel   = "default"
	sessDefaultDetail  = "the agent's own choice"
	sessNoCatalogNote  = " catalog seen yet · enter uses the id as typed"
	sessNoModelNote    = "no model in it matches · enter uses the id as typed"
	sessUseNotePrefix  = "new sessions use "
	sessNeedsProvider  = "/provider needs a provider: "
	sessNeedsModel     = "/model needs a model id"
	sessEffortTitle    = "effort for new sessions"
	sessFastTitle      = "fast mode for new sessions"
	sessSettingDetail  = "the model's own"
)

// sessProviderDetail is each provider's line in /provider's list (the
// mockup's words); a provider it does not name has none.
var sessProviderDetail = map[string]string{
	agent.CursorProvider().Name(): "cursor-agent over ACP",
	agent.GrokProvider().Name():   "grok over ACP",
	agent.GxProvider().Name():     "the gx fork of grok",
	agent.NativeProvider().Name(): "craze's own harness",
}

// sessPick is what /provider and /model — and /effort and /fast (plan 032
// C17) — chose for new sessions from the list (§3.14). It is the TUI's, not an
// opening's (Model.sessPick, carried by withSession): it lasts across every
// opening of the list and every session shown, until changed or craze quits.
type sessPick struct {
	// effort and fast are /effort's and /fast's: "" and nil the model's own.
	// Unlike the model they belong to no provider — a host matches them
	// against what its model offers — so a /provider choice keeps them.
	effort string
	fast   *bool
	// prov is the chosen provider, when provSet: without one a new session
	// runs the provider of the session the list came from (X145).
	prov    agent.Provider
	provSet bool
	// model is the chosen model id — "" the provider's default — and
	// modelName how the rule names it, when modelSet: without one a new
	// session takes the session the list came from's model (X145).
	model     string
	modelName string
	modelSet  bool
	// seq numbers the choices, /provider's and /model's; resolving says
	// native's default is being read for choice seq (sessNativeDefaultMsg),
	// which is taken only while no later choice has replaced it.
	seq       uint64
	resolving bool
}

// sessNativeDefaultMsg is native's default model, read for /provider native
// (nativeDefaultModel) — the pick seq it was read for, and the list's opening
// gen it was chosen in, whose hint line says what new sessions use.
type sessNativeDefaultMsg struct {
	seq, gen uint64
	model    agent.ModelInfo
	err      error
}

// ------------------------------------------------------------ Plan 031's seam

// nativeModelChoices is the ordered list /model offers for native (plan 031
// §3.6, switched from plan 030 §3.14's own): the models a native session's own
// picker offers with no current model — the merged table's models (its files
// over the shipped catalog) whose provider has a usable key, the recently
// used ones first in recent order (Recent 1, 2, 3…, the rows' order and no
// label), then the rest by name — each named by its table name, as a native
// session's own model list is. dir and getenv are the TUI's own seams
// (Model.nativeDir, Model.nativeEnv: Config.NativeDir, Config.Getenv). It
// reads the disk, and is called off the Update (the popup's load); it never
// writes recent.json — only a native session's switch does (plan 031 §3.4).
//
// This and nativeDefaultModel are the one seam through which the list learns
// what native's models are (agreed with Plan 031, 2026-09-30); nothing else
// here reads the table. With no model funded there is nothing to offer, and
// that is an error the popup says, as it is for nativeDefaultModel.
func nativeModelChoices(dir string, getenv func(string) string) ([]agent.ModelInfo, error) {
	table, err := modeltable.Load(dir)
	if err != nil {
		return nil, err
	}
	choices := table.Choices(modeltable.ReadRecent(dir), getenv, "")
	if len(choices) == 0 {
		return nil, errNativeNothingFunded
	}
	out := make([]agent.ModelInfo, 0, len(choices))
	for _, c := range choices {
		out = append(out, agent.ModelInfo{ID: c.Alias, Name: nativeModelName(c.Name, c.Alias), Recent: c.Recent})
	}
	return out, nil
}

// nativeDefaultModel is the model /provider native resets the list's model
// to (plan 031 §3.5): the one a new native session with no --model starts on
// (Table.StartModel) — the newest remembered model that is still funded, else
// the table's default_model, else the first funded alias, sorted — judged
// against getenv, which is this process's environment, which the host a
// dispatch spawns inherits. Nothing funded is an error, as it is at a
// session's start. It reads the disk, and is called off the Update.
func nativeDefaultModel(dir string, getenv func(string) string) (agent.ModelInfo, error) {
	table, err := modeltable.Load(dir)
	if err != nil {
		return agent.ModelInfo{}, err
	}
	alias, _, err := table.StartModel(modeltable.ReadRecent(dir), getenv)
	if errors.Is(err, modeltable.ErrNothingFunded) {
		return agent.ModelInfo{}, errNativeNothingFunded
	}
	if err != nil {
		return agent.ModelInfo{}, err
	}
	return agent.ModelInfo{ID: alias, Name: nativeModelName(table.Models[alias].Name, alias)}, nil
}

// errNativeNothingFunded is what the list says when not one native model's
// provider has an API key: the popup's note (`native has no models: …`) and
// /provider native's warning (`its model table: …`) both carry it.
var errNativeNothingFunded = errors.New(`no model provider has an API key — run craze auth login (an API key, or "craze auth login chatgpt" for a ChatGPT plan), or set its API key variable`)

// nativeModelName is a native model as the list names it: its table name,
// cleaned for the terminal, else its alias.
func nativeModelName(name, alias string) string {
	if name = sanitizeLine(name); name != "" {
		return name
	}
	return alias
}

// readNativeDefault reads native's default model off the Update, for the
// /provider choice seq made in the list's opening gen, from dir and getenv
// (Model.nativeDir, Model.nativeEnv).
func readNativeDefault(seq, gen uint64, dir string, getenv func(string) string) tea.Cmd {
	return func() tea.Msg {
		md, err := nativeDefaultModel(dir, getenv)
		return sessNativeDefaultMsg{seq: seq, gen: gen, model: md, err: err}
	}
}

// isNative says p is craze's own harness: the one provider whose models are a
// table and not a catalog.
func isNative(p agent.Provider) bool { return p.Name() == agent.NativeProvider().Name() }

// ------------------------------------------------------------ what new sessions run

// sessNewProviderOf is the provider new sessions from the list run: /provider's
// (or the one /model pinned), else the session the list came from's, by its
// id — false when that is not one this craze knows.
func (m Model) sessNewProviderOf() (agent.Provider, bool) {
	if m.sessPick.provSet {
		return m.sessPick.prov, true
	}
	name := m.snap.Provider.Name
	if name == "" {
		name = m.sessProvider
	}
	p, err := agent.ProviderByName(name)
	return p, err == nil
}

// sessNewModelID is the model id new sessions from the list start on: the
// pick's (/provider's reset, or /model's), else the session the list came
// from's current model, else the launch's --model; "" the provider's default.
func (m Model) sessNewModelID() string {
	if m.sessPick.modelSet {
		return m.sessPick.model
	}
	if id := m.snap.CurrentModel; id != "" {
		return id
	}
	if m.model != sessDefaultModel {
		return m.model
	}
	return ""
}

// sessNewModels is the catalog the model new sessions start on is named from:
// the pick's own model, else the session the list came from's catalog (the
// unstarted session's status row, dispatch.go).
func (m Model) sessNewModels() []agent.ModelInfo {
	if m.sessPick.modelSet {
		if m.sessPick.model == "" {
			return nil
		}
		return []agent.ModelInfo{{ID: m.sessPick.model, Name: m.sessPick.modelName}}
	}
	return m.snap.Models
}

// sessNewSettings is the effort and fast mode new sessions start at, as the
// rule and the hint line append them to the provider and model: ` · <effort>`
// when /effort set one, ` · fast` or ` · no fast` when /fast did — nothing
// for either left to the model's own, so a list where neither was chosen
// draws what it always drew.
func (m Model) sessNewSettings() string {
	var b strings.Builder
	if e := m.sessPick.effort; e != "" {
		b.WriteString(" · " + sanitizeLine(e))
	}
	if f := m.sessPick.fast; f != nil {
		if *f {
			b.WriteString(" · fast")
		} else {
			b.WriteString(" · no fast")
		}
	}
	return b.String()
}

// sessUseNote is the hint line's `new sessions use …` after a choice: the
// provider, the model and the settings, as the rule names them.
func (m Model) sessUseNote() string {
	return sessUseNotePrefix + m.sessNewProvider() + " · " + m.sessNewModel() + m.sessNewSettings()
}

// sessEffortLabel and sessFastLabel are /effort's and /fast's current value
// as their rows in the `/` popup note it, and their values' `current` mark.
func sessEffortLabel(pk sessPick) string {
	if pk.effort == "" {
		return sessDefaultModel
	}
	return sanitizeLine(pk.effort)
}

func sessFastLabel(pk sessPick) string {
	switch {
	case pk.fast == nil:
		return sessDefaultModel
	case *pk.fast:
		return sessFastOn
	}
	return sessFastOff
}

// ------------------------------------------------------------ the `/` grammar

// sessCmdGrammar is the list's `/` line: the whole input from its leading `/`
// (its first non-space rune), while the cursor is past it — a command word
// being typed that some command starts with (`/`, `/pro`, `/ef`), or
// `/provider`, `/model`, `/effort` or `/fast` and a space, with the value
// after it. Any other `/…` is a prompt, and opens nothing: agents have slash
// commands of their own (X152).
var sessCmdGrammar = completeGrammar{
	under: sessCmdToken,
	write: func(text string) (string, int, bool) {
		for _, r := range text {
			if unicode.IsControl(r) || r == '�' {
				return "", 0, false
			}
		}
		return "/" + text, 1 + len(text), true
	},
}

// sessCmdToken is the `/` line under the cursor, if the input is one the
// popup completes (sessCmdGrammar).
func sessCmdToken(value string, cursor int) (completeToken, bool) {
	i := strings.IndexFunc(value, func(r rune) bool { return !unicode.IsSpace(r) })
	if i < 0 || value[i] != '/' || cursor <= i {
		return completeToken{}, false
	}
	text := value[i+1:]
	word, _, spaced := strings.Cut(text, " ")
	word = strings.ToLower(word)
	if spaced {
		switch word {
		case sessCmdProvider, sessCmdModel, sessCmdEffort, sessCmdFast:
		default:
			return completeToken{}, false
		}
	} else if strings.IndexFunc(text, unicode.IsSpace) >= 0 || !sessCmdKnownPrefix(word) {
		return completeToken{}, false
	}
	return completeToken{start: i, end: len(value), text: text}, true
}

// sessCmdKnownPrefix says some command of the list's starts with word.
func sessCmdKnownPrefix(word string) bool {
	starts := func(c string) bool { return strings.HasPrefix(c, word) }
	return slices.ContainsFunc(sessCmdOlder, starts) || slices.ContainsFunc(sessCmdLater, starts)
}

// ------------------------------------------------------------ the source

// sessCmdSource is the `/` popup's source as the list stands at one sync:
// the providers /provider offers (choices) and where their availability is
// read again (availSrc, nil with no ProviderAvailabilitySource), what new
// sessions run now (the notes and the `current` marks), where /model reads an
// ACP provider's catalog, and the clock the catalog's age is read against.
type sessCmdSource struct {
	choices    []provChoice
	availSrc   ProviderAvailabilitySource
	provider   agent.Provider
	provOK     bool
	provLabel  string
	model      string
	modelLabel string
	// effort and fast are /effort's and /fast's current values, as their
	// rows note them and their values mark them (sessEffortLabel,
	// sessFastLabel).
	effort    string
	fast      string
	starter   SessionStarter
	nativeDir string
	nativeEnv func(string) string
	now       time.Time
	frozen    bool
}

func (s sessCmdSource) completeID() string { return sessCmdSourceID }

func (s sessCmdSource) complete(q completeQuery) completeAnswer {
	word, arg, spaced := strings.Cut(q.Text, " ")
	if !spaced {
		return s.commands(strings.ToLower(word))
	}
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(word) {
	case sessCmdProvider:
		return s.providerValues(q, arg)
	case sessCmdModel:
		return s.modelValues(q, arg)
	case sessCmdEffort:
		return sessSettingValues(sessCmdEffort, sessEffortTitle, sessEffortLevels, s.effort, arg)
	case sessCmdFast:
		return sessSettingValues(sessCmdFast, sessFastTitle, sessFastValues, s.fast, arg)
	}
	return completeAnswer{}
}

// commands is the list's commands whose names start with prefix — a later
// one only once the reveal rule shows it (sessCmdRevealed) — each with what
// it sets and, but for /exit, what new sessions use now.
func (s sessCmdSource) commands(prefix string) completeAnswer {
	all := []completeItem{
		{Name: "/" + sessCmdProvider, Detail: "provider for new sessions", Note: s.provLabel, Tone: completeToneProvider,
			Value: "cmd:" + sessCmdProvider, Insert: sessCmdProvider},
		{Name: "/" + sessCmdModel, Detail: "model for new sessions", Note: s.modelLabel,
			Value: "cmd:" + sessCmdModel, Insert: sessCmdModel},
		{Name: "/" + sessCmdExit, Detail: "quit; sessions keep running",
			Value: "cmd:" + sessCmdExit, Insert: sessCmdExit},
		{Name: "/" + sessCmdEffort, Detail: sessEffortTitle, Note: s.effort,
			Value: "cmd:" + sessCmdEffort, Insert: sessCmdEffort},
		{Name: "/" + sessCmdFast, Detail: sessFastTitle, Note: s.fast,
			Value: "cmd:" + sessCmdFast, Insert: sessCmdFast},
	}
	ans := completeAnswer{Title: sessCmdTitle}
	for _, it := range all {
		if !strings.HasPrefix(it.Insert, prefix) {
			continue
		}
		if slices.Contains(sessCmdLater, it.Insert) && !sessCmdRevealed(it.Insert, prefix) {
			continue
		}
		ans.Items = append(ans.Items, it)
	}
	return ans
}

// sessSettingValues is /effort's or /fast's list (cmd's): its values whose
// names start with arg, case folded — `default`, the model's own, first — the
// one new sessions use marked `current`. They are offered whatever the
// provider: a host matches the value against what its model offers.
func sessSettingValues(cmd, title string, values []string, current, arg string) completeAnswer {
	fold := strings.ToLower(arg)
	ans := completeAnswer{Title: title}
	for _, v := range values {
		if !strings.HasPrefix(v, fold) {
			continue
		}
		it := completeItem{Name: v, Value: cmd + ":" + v, Insert: cmd + " " + v}
		if v == sessDefaultModel {
			it.Detail = sessSettingDetail
		}
		if v == current {
			it.Note, it.Tone = sessCurrentNote, completeToneAccent
		}
		ans.Items = append(ans.Items, it)
	}
	if len(ans.Items) == 0 {
		ans.Note, ans.NoteErr = "no "+cmd+" "+sanitizeLine(arg)+": one of "+strings.Join(values, ", "), true
	}
	return ans
}

// sessProvidersKey keys /provider's read of the providers' availability (plan
// 036 §3.3): once per opening of its values, as the popup's loads are.
const sessProvidersKey = "providers"

// providerValues is /provider's list: the providers it offers
// (sessProviderChoices) whose id or name starts with arg, case folded, the
// one new sessions run marked `current` — and one that is not ready (plan 036
// §3.3) with its reason in place of its detail, its state for its note, dim,
// and its refusal, which enter and tab draw as the popup's note instead of
// taking it (sessCmdChosen) — but for native needing setup, which carries
// none: enter and tab open the pre-session connect dialog on it instead
// (§3.6). With a ProviderAvailabilitySource the
// providers' availability is read again as the values open — the popup's
// load, off the Update — and the list's latest answer serves until it is
// back.
func (s sessCmdSource) providerValues(q completeQuery, arg string) completeAnswer {
	fold := strings.ToLower(arg)
	ans := completeAnswer{Title: sessProvidersTitle}
	if s.availSrc != nil {
		if _, back := q.Loaded(sessProvidersKey); !back {
			src := s.availSrc
			ans.Load = &completeLoad{Key: sessProvidersKey, Run: func(context.Context) completeLoaded {
				return completeLoaded{Data: askSessAvail(src)}
			}}
		}
	}
	for _, c := range s.choices {
		id := c.p.Name()
		if !strings.HasPrefix(strings.ToLower(id), fold) && !strings.HasPrefix(strings.ToLower(c.p.DisplayName()), fold) {
			continue
		}
		it := completeItem{Name: sanitizeLine(id), Detail: sessProviderDetail[id], Value: "provider:" + id,
			Insert: sessCmdProvider + " " + id}
		switch {
		case !c.a.ready():
			it.Detail, it.Note, it.Tone = sanitizeLine(c.a.Reason), c.a.State.word(), completeToneDim
			it.Refusal = c.refusal()
		case s.provOK && id == s.provider.Name():
			it.Note, it.Tone = sessCurrentNote, completeToneAccent
		}
		ans.Items = append(ans.Items, it)
	}
	if len(ans.Items) == 0 {
		ans.Note, ans.NoteErr = "no provider "+sanitizeLine(arg), true
	}
	return ans
}

// modelValues is /model's list for the provider new sessions run: native's
// table, or an ACP provider's cached catalog — each read once per opening of
// the popup, off the Update (a load keyed by the provider) — filtered by arg,
// a substring of a model's name or id, case folded. The model new sessions
// start on is marked `current`; an ACP provider's list begins with its
// agent's own default, and its title says how old the catalog is.
//
// The agent's own default is a row of the list's own, never one of the
// catalog's: its id is "" — no --model at all — which no cached model has
// (modelcache keeps none without an id), and it is offered whatever the
// catalog holds. A catalog model whose id is `default` is another row, and
// chosen it starts the session with --model=default (plan 030 C15r, sol
// r31-c16 1: it used to stand in for the agent's own row, which it hid).
func (s sessCmdSource) modelValues(q completeQuery, arg string) completeAnswer {
	if !s.provOK {
		return completeAnswer{Note: "no provider to list models for", NoteErr: true}
	}
	p := s.provider
	name := sanitizeLine(p.Name())
	ans := completeAnswer{Title: name + sessModelsTitle}
	key := "models:" + p.Name()
	l, back := q.Loaded(key)
	if !back {
		ans.Load = &completeLoad{Key: key, Run: s.loadModels(p)}
		return ans
	}
	if l.Err != nil {
		return completeAnswer{Title: ans.Title, Note: name + " has no models: " + sessErrText(l.Err), NoteErr: true}
	}
	if !l.At.IsZero() {
		d := s.now.Sub(l.At)
		if d < 0 || s.frozen {
			d = 0
		}
		ans.Title += " · last seen " + sessAgeText(d) + " ago"
	}
	items := l.Items
	if !isNative(p) {
		if len(items) == 0 {
			return completeAnswer{Title: ans.Title, Note: "no " + name + sessNoCatalogNote}
		}
		items = append([]completeItem{{Name: sessDefaultModel, Detail: sessDefaultDetail}}, items...)
	}
	fold := strings.ToLower(arg)
	for _, it := range items {
		if fold != "" && !strings.Contains(strings.ToLower(it.Name), fold) && !strings.Contains(strings.ToLower(it.Value), fold) {
			continue
		}
		id := it.Value
		row := completeItem{Name: it.Name, Detail: it.Detail, Value: "model:" + id, Insert: sessCmdModel + " " + id}
		if id == "" {
			row.Insert = sessCmdModel + " " + sessDefaultModel
		}
		if id == s.model {
			row.Note, row.Tone = sessCurrentNote, completeToneAccent
		}
		ans.Items = append(ans.Items, row)
	}
	if len(ans.Items) == 0 {
		ans.Note = sessNoModelNote
	}
	return ans
}

// loadModels is /model's load for p: native's model table
// (nativeModelChoices), or p's cached catalog (SessionStarter.ModelCatalog),
// as rows — the model's name, its id beside it when they differ — with the
// time the catalog was seen.
func (s sessCmdSource) loadModels(p agent.Provider) func(context.Context) completeLoaded {
	starter, nativeDir, nativeEnv := s.starter, s.nativeDir, s.nativeEnv
	return func(context.Context) completeLoaded {
		var (
			models []agent.ModelInfo
			at     time.Time
		)
		if isNative(p) {
			var err error
			if models, err = nativeModelChoices(nativeDir, nativeEnv); err != nil {
				return completeLoaded{Err: err}
			}
		} else if starter != nil {
			if c, ok := starter.ModelCatalog(p.Name()); ok {
				models, at = agent.OrderModels(agent.Snapshot{Models: c.Models}), c.ObservedAt
			}
		}
		items := make([]completeItem, 0, len(models))
		for _, md := range models {
			if md.ID == "" {
				// No id: nothing --model could name, and "" is the agent's
				// own default's row (modelValues).
				continue
			}
			name := sanitizeLine(md.Name)
			if name == "" {
				name = sanitizeLine(md.ID)
			}
			it := completeItem{Name: name, Value: md.ID}
			if name != sanitizeLine(md.ID) {
				it.Detail = sanitizeLine(md.ID)
			}
			items = append(items, it)
		}
		return completeLoaded{Items: items, At: at}
	}
}

// sessCmdSourceNow is the `/` popup's source as the list stands now.
func (m Model) sessCmdSourceNow() sessCmdSource {
	p, ok := m.sessNewProviderOf()
	st, _ := m.sessions.(SessionStarter)
	src, _ := m.sessions.(ProviderAvailabilitySource)
	return sessCmdSource{
		choices: m.sessProviderChoices(), availSrc: src, provider: p, provOK: ok, provLabel: m.sessNewProvider(),
		model: m.sessNewModelID(), modelLabel: m.sessNewModel(), effort: sessEffortLabel(m.sessPick), fast: sessFastLabel(m.sessPick),
		starter: st, nativeDir: m.nativeDir, nativeEnv: m.nativeEnv, now: m.now(), frozen: m.frozen,
	}
}

// ------------------------------------------------------------ choosing

// sessCmdChosen is a key the `/` popup took on a candidate (§3.14): a value —
// a provider, a model, an effort, a fast mode — is applied by tab and enter
// alike (the mockup's "tab/enter use it"); /provider, /model, /effort and
// /fast are written, with the space that opens their values; /exit quits
// craze on enter, every session left running, and is only written by tab,
// which never submits. handled false: write what the popup chose.
func (m Model) sessCmdChosen(key tea.KeyType, it completeItem) (Model, tea.Cmd, bool) {
	kind, id, _ := strings.Cut(it.Value, ":")
	switch {
	case kind == "cmd" && id == sessCmdExit && key == tea.KeyEnter:
		tm, cmd := m.sessQuit()
		return tm.(Model), cmd, true
	case kind == "provider" && it.Refusal != "":
		// A provider that cannot start (plan 036 §3.3): nothing is taken —
		// the input and what new sessions run stay as they were — and the
		// popup, still up, says why.
		m.sessList.in.cmd.refuse(it.Refusal)
		return m, nil, true
	case kind == "provider":
		p, err := agent.ProviderByName(id)
		if err != nil {
			return m, nil, false
		}
		if c, ok := m.sessProviderChoice(p); ok && c.verdict() == availConnect {
			// Native needing setup (plan 036 §3.6): nothing is taken — the
			// pre-session connect dialog opens over the list, which comes
			// back as it was, and native is chosen again once it is ready.
			next, cmd := m.connectOverSessions()
			return next, cmd, true
		}
		next, cmd := m.sessPickProvider(p)
		return next, cmd, true
	case kind == "model":
		next, cmd := m.sessPickModel(id, it.Name)
		return next, cmd, true
	case kind == sessCmdEffort || kind == sessCmdFast:
		next, cmd := m.sessPickSetting(kind, id)
		return next, cmd, true
	}
	return m, nil, false
}

// sessCmdTyped is enter on a `/provider`, `/model`, `/effort` or `/fast`
// line the popup did not take — typed in full, or with the popup put away:
// the provider named, by id or by name (one /provider offers,
// sessProviderChoices — refused on the hint line, the input left as typed,
// when the list's latest availability answer says it cannot start, plan 036
// §3.3, and the pre-session connect dialog opened on native needing setup,
// §3.6); the model id as typed, which the next start applies as --model
// would; or one of /effort's or /fast's values, case folded, as /provider
// takes only a provider it lists. ok false: not one of them.
func (m Model) sessCmdTyped(line string) (Model, tea.Cmd, bool) {
	name, args, ok := parseSlashLine(line)
	if !ok {
		return m, nil, false
	}
	switch name {
	case sessCmdProvider:
		if args == "" {
			m.sessNote(sessNeedsProvider+m.sessProviderIDs(), sessNoteErr)
			return m, nil, true
		}
		for _, c := range m.sessProviderChoices() {
			if p := c.p; p.Name() == args || strings.EqualFold(p.Name(), args) || strings.EqualFold(p.DisplayName(), args) {
				if refusal := c.refusal(); refusal != "" {
					m.sessNote(refusal, sessNoteErr)
					return m, nil, true
				}
				if c.verdict() == availConnect {
					next, cmd := m.connectOverSessions()
					return next, cmd, true
				}
				next, cmd := m.sessPickProvider(p)
				return next, cmd, true
			}
		}
		m.sessNote("no provider "+sanitizeLine(args)+": one of "+m.sessProviderIDs(), sessNoteErr)
		return m, nil, true
	case sessCmdModel:
		if args == "" {
			m.sessNote(sessNeedsModel, sessNoteErr)
			return m, nil, true
		}
		if args == sessDefaultModel {
			next, cmd := m.sessPickModel("", sessDefaultModel)
			return next, cmd, true
		}
		next, cmd := m.sessPickModel(args, args)
		return next, cmd, true
	case sessCmdEffort, sessCmdFast:
		values := sessEffortLevels
		if name == sessCmdFast {
			values = sessFastValues
		}
		one := strings.Join(values, ", ")
		if args == "" {
			m.sessNote("/"+name+" needs a value: "+one, sessNoteErr)
			return m, nil, true
		}
		for _, v := range values {
			if strings.EqualFold(v, args) {
				next, cmd := m.sessPickSetting(name, v)
				return next, cmd, true
			}
		}
		m.sessNote("no "+name+" "+sanitizeLine(args)+": one of "+one, sessNoteErr)
		return m, nil, true
	}
	return m, nil, false
}

// sessProviderIDs is the providers /provider takes, as a note lists them.
func (m Model) sessProviderIDs() string {
	choices := m.sessProviderChoices()
	ids := make([]string, 0, len(choices))
	for _, c := range choices {
		ids = append(ids, c.p.Name())
	}
	return strings.Join(ids, ", ")
}

// sessProviderChoices is the providers /provider offers and takes (plan 036
// §3.3): the list's latest availability answer's, in its order, each with its
// state — membership recomputed with the states, so a gx installed since the
// launch is offered and one gone is not, and a missing gx that is the
// configured default is offered unavailable — each a provider this craze
// knows; or, with no answer yet (or no ProviderAvailabilitySource), the
// startup picker's rows, every one ready, as /provider always offered them.
func (m Model) sessProviderChoices() []provChoice {
	if !m.sessList.availHave {
		out := make([]provChoice, 0, len(m.providers))
		for _, p := range m.providers {
			out = append(out, provChoice{p: p})
		}
		return out
	}
	out := make([]provChoice, 0, len(m.sessList.avail))
	for _, a := range m.sessList.avail {
		if p, err := agent.ProviderByName(a.ID); err == nil && p.Name() == a.ID {
			out = append(out, provChoice{p: p, a: a})
		}
	}
	return out
}

// sessProviderChoice is p as /provider offers it now (sessProviderChoices),
// and whether it offers it at all.
func (m Model) sessProviderChoice(p agent.Provider) (provChoice, bool) {
	for _, c := range m.sessProviderChoices() {
		if c.p.Name() == p.Name() {
			return c, true
		}
	}
	return provChoice{}, false
}

// sessPickProvider is /provider's choice (§3.14): new sessions from the list
// run p, on its default model — an ACP provider's agent's own (no --model),
// native's read from its table off the Update (readNativeDefault), which the
// rule shows once it is read. The input is cleared and the hint line says
// what new sessions use.
func (m Model) sessPickProvider(p agent.Provider) (Model, tea.Cmd) {
	seq := m.sessPick.seq + 1
	m.sessPick = sessPick{prov: p, provSet: true, modelSet: true, seq: seq, effort: m.sessPick.effort, fast: m.sessPick.fast}
	m.sessList.in.set("", 0)
	var read tea.Cmd
	if isNative(p) {
		m.sessPick.resolving = true
		m.sessNote(sessUseNotePrefix+m.sessNewProvider()+" · …"+m.sessNewSettings(), sessNoteOK)
		read = readNativeDefault(seq, m.sessList.gen, m.nativeDir, m.nativeEnv)
	} else {
		m.sessNote(m.sessUseNote(), sessNoteOK)
	}
	return m, tea.Batch(read, m.syncSessInput())
}

// sessPickModel is /model's choice (§3.14): new sessions start on id, named
// name ("" the provider's default) — and on the provider it was chosen for,
// which the pick now holds, so the model never follows the list onto another
// session's provider. A native default still being read is no longer wanted.
func (m Model) sessPickModel(id, name string) (Model, tea.Cmd) {
	if !m.sessPick.provSet {
		if p, ok := m.sessNewProviderOf(); ok {
			m.sessPick.prov, m.sessPick.provSet = p, true
		}
	}
	if id == "" {
		// The provider's default: drawn `default`, whatever the row said.
		name = ""
	}
	m.sessPick.model, m.sessPick.modelName, m.sessPick.modelSet = id, name, true
	m.sessPick.resolving = false
	m.sessPick.seq++
	m.sessList.in.set("", 0)
	m.sessNote(m.sessUseNote(), sessNoteOK)
	return m, m.syncSessInput()
}

// sessPickSetting is /effort's or /fast's choice (kind's; plan 032 C17): new
// sessions start at value — `default` clearing it, the model's own; `on` and
// `off` fast mode on and off. Nothing else about the pick moves, and nothing
// checks the value against a provider: a host matches it against what its
// model offers. The input is cleared and the hint line says what new
// sessions use.
func (m Model) sessPickSetting(kind, value string) (Model, tea.Cmd) {
	switch {
	case kind == sessCmdEffort && value == sessDefaultModel:
		m.sessPick.effort = ""
	case kind == sessCmdEffort:
		m.sessPick.effort = value
	case value == sessFastOn, value == sessFastOff:
		on := value == sessFastOn
		m.sessPick.fast = &on
	default:
		m.sessPick.fast = nil
	}
	m.sessList.in.set("", 0)
	m.sessNote(m.sessUseNote(), sessNoteOK)
	return m, m.syncSessInput()
}

// sessNativeDefault is native's default read for a /provider choice: taken
// only while that choice stands (a later /provider or /model moved seq on).
// The model is the pick's from here, and the hint line of the opening the
// choice was made in says so; one that could not be read leaves the model
// native's own (no --model), and says why.
func (m Model) sessNativeDefault(msg sessNativeDefaultMsg) Model {
	pk := &m.sessPick
	if !pk.resolving || pk.seq != msg.seq {
		return m
	}
	pk.resolving = false
	if msg.err == nil {
		pk.model, pk.modelName = msg.model.ID, msg.model.Name
	}
	if !m.sessList.open || m.sessList.gen != msg.gen {
		return m
	}
	if msg.err != nil {
		m.sessNote(sessUseNotePrefix+m.sessNewProvider()+" · "+sessDefaultModel+
			" (its model table: "+strings.TrimPrefix(sanitizeLine(msg.err.Error()), "modeltable: ")+")", sessNoteWarn)
		return m
	}
	m.sessNote(m.sessUseNote(), sessNoteOK)
	return m
}

// sessCmdLineHint is the hint line's words for a `/` line the popup is not
// up over, or nil: what enter does with it.
func sessCmdLineHint(v string, key, txt func(string) seg) []seg {
	name, args, ok := parseSlashLine(v)
	if !ok {
		return nil
	}
	switch {
	case name == sessCmdExit && args == "":
		return []seg{key("enter"), txt(" quits craze; every session keeps running · "), key("esc"), txt(" clear")}
	case name == sessCmdProvider || name == sessCmdModel || name == sessCmdEffort || name == sessCmdFast:
		return []seg{key("enter"), txt(" sets it for new sessions · "), key("esc"), txt(" clear")}
	}
	return nil
}

// sessModelLabel is how the rule names a pick's model: its name, else its
// id, else `default`.
func sessModelLabel(pk sessPick) string {
	switch {
	case pk.model == "":
		return sessDefaultModel
	case pk.modelName != "":
		return sanitizeLine(pk.modelName)
	}
	return sanitizeLine(pk.model)
}

// errNoProvider is sessNewSpec's refusal of a provider this craze does not
// know.
func errNoProvider(name string) error {
	return fmt.Errorf("a new session cannot run provider %q", sanitizeLine(name))
}
