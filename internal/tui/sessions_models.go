package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
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
//     pickerRows over the availability-filtered list the launch passes).
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

// The list's commands.
const (
	sessCmdProvider = "provider"
	sessCmdModel    = "model"
	sessCmdExit     = "exit"
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
)

// sessProviderDetail is each provider's line in /provider's list (the
// mockup's words); a provider it does not name has none.
var sessProviderDetail = map[string]string{
	agent.CursorProvider().Name(): "cursor-agent over ACP",
	agent.GrokProvider().Name():   "grok over ACP",
	agent.GxProvider().Name():     "the gx fork of grok",
	agent.NativeProvider().Name(): "craze's own harness",
}

// sessPick is what /provider and /model chose for new sessions from the list
// (§3.14). It is the TUI's, not an opening's (Model.sessPick, carried by
// withSession): it lasts across every opening of the list and every session
// shown, until changed or craze quits.
type sessPick struct {
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

// nativeModelChoices is the ordered list /model offers for native (plan 030
// §3.14): the models of native's model table — its aliases, sorted, each named
// by its table name, as a native session's own model list is (agent's
// tableModels). It reads the disk, and is called off the Update (the popup's
// load).
//
// This and nativeDefaultModel are the one seam through which the list learns
// what native's models are (agreed with Plan 031, 2026-09-30): Plan 031 —
// recent models first, a compiled catalog under models.toml, unfunded
// providers hidden — swaps both bodies for modeltable.Load(paths.NativeDir()),
// ReadRecent, Table.Choices (no current model) and Table.StartModel, in its
// own commit; nothing else here reads the table.
func nativeModelChoices() ([]agent.ModelInfo, error) {
	table, err := modeltable.Load(paths.NativeDir())
	if err != nil {
		return nil, err
	}
	out := make([]agent.ModelInfo, 0, len(table.Models))
	for _, alias := range table.Aliases() {
		out = append(out, nativeModelInfo(table, alias))
	}
	return out, nil
}

// nativeDefaultModel is the model /provider native resets the list's model
// to (§3.14): the one a native session with no --model starts on — the
// table's default_model, unless its provider has no API key, and then the
// first alias, sorted, whose key resolves (native's fundedModel, plan 018
// §3.8), judged against this process's environment, which the host a
// dispatch spawns inherits. Nothing funded is an error, as it is at a
// session's start. It reads the disk, and is called off the Update. Plan 031
// swaps its body (nativeModelChoices).
func nativeDefaultModel() (agent.ModelInfo, error) {
	table, err := modeltable.Load(paths.NativeDir())
	if err != nil {
		return agent.ModelInfo{}, err
	}
	alias := table.DefaultModel
	if _, err := table.Resolve(alias, os.Getenv); errors.Is(err, modeltable.ErrNoAPIKey) {
		alias = ""
		for _, a := range table.Aliases() {
			if _, err := table.Resolve(a, os.Getenv); err == nil {
				alias = a
				break
			}
		}
		if alias == "" {
			return agent.ModelInfo{}, errors.New("no configured model has an API key")
		}
	}
	return nativeModelInfo(table, alias), nil
}

// nativeModelInfo is alias as a native session's model list names it.
func nativeModelInfo(table *modeltable.Table, alias string) agent.ModelInfo {
	name := sanitizeLine(table.Models[alias].Name)
	if name == "" {
		name = alias
	}
	return agent.ModelInfo{ID: alias, Name: name}
}

// readNativeDefault reads native's default model off the Update, for the
// /provider choice seq made in the list's opening gen.
func readNativeDefault(seq, gen uint64) tea.Cmd {
	return func() tea.Msg {
		md, err := nativeDefaultModel()
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

// ------------------------------------------------------------ the `/` grammar

// sessCmdGrammar is the list's `/` line: the whole input from its leading `/`
// (its first non-space rune), while the cursor is past it — a command word
// being typed that some command starts with (`/`, `/pro`), or `/provider` or
// `/model` and a space, with the value after it. Any other `/…` is a prompt,
// and opens nothing: agents have slash commands of their own (X152).
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
		if word != sessCmdProvider && word != sessCmdModel {
			return completeToken{}, false
		}
	} else if strings.IndexFunc(text, unicode.IsSpace) >= 0 || !sessCmdKnownPrefix(word) {
		return completeToken{}, false
	}
	return completeToken{start: i, end: len(value), text: text}, true
}

// sessCmdKnownPrefix says some command of the list's starts with word.
func sessCmdKnownPrefix(word string) bool {
	for _, c := range []string{sessCmdProvider, sessCmdModel, sessCmdExit} {
		if strings.HasPrefix(c, word) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------ the source

// sessCmdSource is the `/` popup's source as the list stands at one sync:
// the providers /provider offers, what new sessions run now (the notes and
// the `current` marks), where /model reads an ACP provider's catalog, and the
// clock the catalog's age is read against.
type sessCmdSource struct {
	providers  []agent.Provider
	provider   agent.Provider
	provOK     bool
	provLabel  string
	model      string
	modelLabel string
	starter    SessionStarter
	now        time.Time
	frozen     bool
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
		return s.providerValues(arg)
	case sessCmdModel:
		return s.modelValues(q, arg)
	}
	return completeAnswer{}
}

// commands is the list's commands whose names start with prefix: each with
// what it sets and, for /provider and /model, what new sessions use now.
func (s sessCmdSource) commands(prefix string) completeAnswer {
	all := []completeItem{
		{Name: "/" + sessCmdProvider, Detail: "provider for new sessions", Note: s.provLabel, Tone: completeToneProvider,
			Value: "cmd:" + sessCmdProvider, Insert: sessCmdProvider},
		{Name: "/" + sessCmdModel, Detail: "model for new sessions", Note: s.modelLabel,
			Value: "cmd:" + sessCmdModel, Insert: sessCmdModel},
		{Name: "/" + sessCmdExit, Detail: "quit; sessions keep running",
			Value: "cmd:" + sessCmdExit, Insert: sessCmdExit},
	}
	ans := completeAnswer{Title: sessCmdTitle}
	for _, it := range all {
		if strings.HasPrefix(it.Insert, prefix) {
			ans.Items = append(ans.Items, it)
		}
	}
	return ans
}

// providerValues is /provider's list: the startup picker's providers whose id
// or name starts with arg, case folded, the one new sessions run marked
// `current`.
func (s sessCmdSource) providerValues(arg string) completeAnswer {
	fold := strings.ToLower(arg)
	ans := completeAnswer{Title: sessProvidersTitle}
	for _, p := range s.providers {
		id := p.Name()
		if !strings.HasPrefix(strings.ToLower(id), fold) && !strings.HasPrefix(strings.ToLower(p.DisplayName()), fold) {
			continue
		}
		it := completeItem{Name: sanitizeLine(id), Detail: sessProviderDetail[id], Value: "provider:" + id,
			Insert: sessCmdProvider + " " + id}
		if s.provOK && id == s.provider.Name() {
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
	starter := s.starter
	return func(context.Context) completeLoaded {
		var (
			models []agent.ModelInfo
			at     time.Time
		)
		if isNative(p) {
			var err error
			if models, err = nativeModelChoices(); err != nil {
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
	return sessCmdSource{
		providers: m.providers, provider: p, provOK: ok, provLabel: m.sessNewProvider(),
		model: m.sessNewModelID(), modelLabel: m.sessNewModel(), starter: st, now: m.now(), frozen: m.frozen,
	}
}

// ------------------------------------------------------------ choosing

// sessCmdChosen is a key the `/` popup took on a candidate (§3.14): a value —
// a provider, a model — is applied by tab and enter alike (the mockup's
// "tab/enter use it"); /provider and /model are written, with the space that
// opens their values; /exit quits craze on enter, every session left running,
// and is only written by tab, which never submits. handled false: write what
// the popup chose.
func (m Model) sessCmdChosen(key tea.KeyType, it completeItem) (Model, tea.Cmd, bool) {
	kind, id, _ := strings.Cut(it.Value, ":")
	switch {
	case kind == "cmd" && id == sessCmdExit && key == tea.KeyEnter:
		tm, cmd := m.sessQuit()
		return tm.(Model), cmd, true
	case kind == "provider":
		p, err := agent.ProviderByName(id)
		if err != nil {
			return m, nil, false
		}
		next, cmd := m.sessPickProvider(p)
		return next, cmd, true
	case kind == "model":
		next, cmd := m.sessPickModel(id, it.Name)
		return next, cmd, true
	}
	return m, nil, false
}

// sessCmdTyped is enter on a `/provider` or `/model` line the popup did not
// take — typed in full, or with the popup put away: the provider named, by
// id or by name (one the startup picker lists), or the model id as typed,
// which the next start applies as --model would. ok false: not one of them.
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
		for _, p := range m.providers {
			if p.Name() == args || strings.EqualFold(p.Name(), args) || strings.EqualFold(p.DisplayName(), args) {
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
	}
	return m, nil, false
}

// sessProviderIDs is the providers /provider takes, as a note lists them.
func (m Model) sessProviderIDs() string {
	ids := make([]string, 0, len(m.providers))
	for _, p := range m.providers {
		ids = append(ids, p.Name())
	}
	return strings.Join(ids, ", ")
}

// sessPickProvider is /provider's choice (§3.14): new sessions from the list
// run p, on its default model — an ACP provider's agent's own (no --model),
// native's read from its table off the Update (readNativeDefault), which the
// rule shows once it is read. The input is cleared and the hint line says
// what new sessions use.
func (m Model) sessPickProvider(p agent.Provider) (Model, tea.Cmd) {
	seq := m.sessPick.seq + 1
	m.sessPick = sessPick{prov: p, provSet: true, modelSet: true, seq: seq}
	m.sessList.in.set("", 0)
	var read tea.Cmd
	if isNative(p) {
		m.sessPick.resolving = true
		m.sessNote(sessUseNotePrefix+m.sessNewProvider()+" · …", sessNoteOK)
		read = readNativeDefault(seq, m.sessList.gen)
	} else {
		m.sessNote(sessUseNotePrefix+m.sessNewProvider()+" · "+m.sessNewModel(), sessNoteOK)
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
	m.sessNote(sessUseNotePrefix+m.sessNewProvider()+" · "+m.sessNewModel(), sessNoteOK)
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
	m.sessNote(sessUseNotePrefix+m.sessNewProvider()+" · "+m.sessNewModel(), sessNoteOK)
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
	case name == sessCmdProvider || name == sessCmdModel:
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
