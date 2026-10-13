package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The slash catalog (plan 043 §3.1 D1, §3.2): every `/` command a session
// offers, in the order the TUI's menu has always listed them, each with what
// running it means — so that a client that is not the TUI can offer the same
// menu without re-deriving the TUI's rules, and the TUI builds its own menu
// with the same builder (P14). It is described once, here, from session facts
// alone (P4, P13); what the host or the client knows on top — whether the
// server serves session.stop or session.aside, whether the client has a hub
// list — is the client's to apply, through Requires and through rows of its
// own.

// SlashItem is one row of the catalog (§3.2's normative table). Every field is
// a value — strings, bools and SlashRun, which holds only the same — so a copy
// of the slice is a deep copy, and cloneSlash needs nothing more than that to
// hand out a catalog that shares no memory with the session's (P6);
// TestSlashItemHoldsNoReferences keeps it so.
type SlashItem struct {
	// Name is the token without its "/", the spelling the row is matched,
	// accepted and run by. It passed SlashNameOK and is never cut (P7).
	Name string
	// Description is exactly what the TUI's menu draws for the row (P17):
	// a builtin's own sentence, or an advertised, plugin or skill
	// description folded onto one line by DisplayLine (P18).
	Description string
	// Source is SlashSourceBuiltin, SlashSourceAgent, SlashSourcePlugin or
	// SlashSourceSkill.
	Source string
	// Plugin is the plugin id a plugin row came from; Qualified its
	// plugin:name spelling, set only when that differs from Name — the second
	// spelling the row answers to (the TUI's qualifiedAlias). Both are "" on
	// every other row.
	Plugin, Qualified string
	// Requires is what the row needs, "" for none: SlashRequiresStop,
	// SlashRequiresAside or SlashRequiresEffort. Stop and aside are session
	// capabilities, facts of the server a client is attached through, which
	// this package cannot see (P4); effort is the client's own reading of the
	// session's snapshot — the session advertises an effort option for its
	// current model (EffortOption is not nil; plan 047 D4, P8), which changes
	// with the model. So the row is always listed, and a client shows it only
	// where what it requires is true.
	//
	// A row with Requires never claims its name: the rows after it are
	// deduplicated as if it were not there. A client that shows it hides any
	// later row of the same name (case-insensitively); one that hides it shows
	// them. That is today's menu exactly: /btw where the session answers side
	// questions, and an agent's or a plugin's /btw where it does not.
	Requires string
	// WhileWorking says the row runs while a turn is running rather than
	// being refused (P16); a prompt row leaves it false, because it queues.
	WhileWorking bool
	// Run is what selecting the row does.
	Run SlashRun
}

// SlashRun is what running a slash row means (§3.2): the kind, and the
// fields that kind reads.
type SlashRun struct {
	// Kind is one of the SlashRun* kinds.
	Kind string
	// Method is the protocol method a SlashRunMethod row calls (the
	// SlashMethod* names), Param the parameter its argument fills — "title"
	// for session.setTitle, "question" for an aside row — and Setting the
	// session.set setting it sends verbatim, its mode value already resolved
	// to an id the session advertised.
	Method, Param string
	Setting       SlashSetting
	// Arg says whether the row takes an argument: SlashArgNone (""),
	// SlashArgOptional or SlashArgRequired. ArgHint is how to show it.
	Arg, ArgHint string
	// Picker names the picker a SlashRunPicker row opens ("model", "effort");
	// Local the client's own action a SlashRunLocal row runs ("help",
	// "clear", "tasks", "theme", "connect"). A client dispatches on these and
	// never on the row's name (P17).
	Picker, Local string
	// Reason is why a SlashRunUnavailable row cannot run, in the words the
	// TUI has always refused it with ("mode ask is not advertised").
	Reason string
}

// SlashSetting is session.set's setting object as a SlashRun carries it: the
// kind ("mode") and the value. The zero value is none.
type SlashSetting struct {
	Kind, Value string
}

// SlashCatalog is a session's slash catalog: every row, in order. It is also
// the StateDelta section that carries one (StateDelta.Slash), whole, as the
// other list sections carry theirs.
type SlashCatalog struct {
	Items []SlashItem
}

// cloneSlash is a slash catalog's rows in a slice of their own: a deep copy,
// because SlashItem holds no references.
func cloneSlash(items []SlashItem) []SlashItem { return append([]SlashItem(nil), items...) }

// The vocabulary of a slash row (§3.2). A client hides a row whose Source,
// Run.Kind, Run.Method or Requires it does not know (P17).
const (
	SlashSourceBuiltin = "builtin"
	SlashSourceAgent   = "agent"
	SlashSourcePlugin  = "plugin"
	SlashSourceSkill   = "skill"

	// SlashRunPrompt sends "/name[ args]" as the text of an ordinary prompt.
	SlashRunPrompt = "prompt"
	// SlashRunMethod calls Run.Method.
	SlashRunMethod = "method"
	// SlashRunPicker opens Run.Picker.
	SlashRunPicker = "picker"
	// SlashRunAside asks a side question (session.aside), the argument
	// filling Run.Param.
	SlashRunAside = "aside"
	// SlashRunLocal runs the client's own Run.Local action.
	SlashRunLocal = "local"
	// SlashRunUnavailable cannot run here, for Run.Reason.
	SlashRunUnavailable = "unavailable"

	SlashArgNone     = ""
	SlashArgOptional = "optional"
	SlashArgRequired = "required"

	// The methods a SlashRunMethod row calls: the control protocol's own
	// names (TestSlashMethodsAreTheProtocols).
	SlashMethodSetTitle = "session.setTitle"
	SlashMethodSet      = "session.set"
	SlashMethodStop     = "session.stop"

	// What a row can require (SlashItem.Requires): the session capabilities
	// stop and aside, and effort, the client's reading of the snapshot (an
	// effort option for the current model).
	SlashRequiresStop   = "stop"
	SlashRequiresAside  = "aside"
	SlashRequiresEffort = "effort"
)

// slashGate is when a builtin is listed at all: always, or only where the
// session's facts say it has the thing the row is about.
type slashGate int

const (
	slashAlways slashGate = iota
	// slashModes: the mode rows (plan, ask, agent), listed where the provider
	// shows modes and the session advertised some (the TUI's showModes). Their
	// Run is resolved at build time against the advertised modes (P15).
	slashModes
	// slashTodos: the provider has a tasks panel (the TUI's showTodos).
	slashTodos
	// slashNative: the session is native's — /connect stores a key for craze's
	// own harness, and no ACP agent reads one (plan 031 §3.9).
	slashNative
)

// slashBuiltinRow is one row of craze's own command table: the row as the
// catalog lists it, its gate, and whether its name is reserved.
type slashBuiltinRow struct {
	item SlashItem
	gate slashGate
	// reserved is a name a plugin may never take bare (BuiltinSlashNames).
	reserved bool
}

// slashBuiltinTable is craze's own commands, in the menu's order — the order
// internal/tui's builtinSlash, connectBuiltin and btwBuiltin have always been
// listed in (help, model, /connect right after /model, …, the mode rows,
// /btw, then /exit) — with the TUI's exact descriptions (P17), so no golden
// moves (P20). It moved here from the TUI (plan 043 D1) so a host can describe
// the menu to a client that is not the TUI.
//
// /effort (plan 047 D4) is last, after /exit, so `/e` still selects /exit
// and `/ef` /effort. It is not reserved: where the session has no effort
// option the row is hidden, and an agent's or a plugin's /effort shows.
//
// /sessions is not here (P13): whether a client has a hub list is the
// client's fact, and the TUI adds that row itself.
//
// WhileWorking is real dispatch (P16; internal/tui's handleEnter and
// runsWhileWorking): true for exit, help, theme, tasks, clear, rename and
// btw, false for model, the modes, connect and effort — which the TUI
// refuses busy. TestSlashTableMatchesTheTUIsDispatch holds it to the TUI.
var slashBuiltinTable = []slashBuiltinRow{
	{item: SlashItem{Name: "help", Description: "Keybindings and commands",
		Run: SlashRun{Kind: SlashRunLocal, Local: "help"}, WhileWorking: true}, reserved: true},
	{item: SlashItem{Name: "model", Description: "Switch model",
		Run: SlashRun{Kind: SlashRunPicker, Picker: "model", Arg: SlashArgOptional, ArgHint: "<model> [effort]"}}, reserved: true},
	{item: SlashItem{Name: "connect", Description: "Connect a model provider: store its API key",
		Run: SlashRun{Kind: SlashRunLocal, Local: "connect"}}, gate: slashNative},
	{item: SlashItem{Name: "clear", Description: "Clear transcript",
		Run: SlashRun{Kind: SlashRunLocal, Local: "clear"}, WhileWorking: true}, reserved: true},
	{item: SlashItem{Name: "tasks", Description: "Tasks panel: compact, expanded, hidden",
		Run: SlashRun{Kind: SlashRunLocal, Local: "tasks"}, WhileWorking: true}, gate: slashTodos, reserved: true},
	{item: SlashItem{Name: "theme", Description: "Theme picker, or /theme <name>",
		Run: SlashRun{Kind: SlashRunLocal, Local: "theme", Arg: SlashArgOptional, ArgHint: "<name>"}, WhileWorking: true}, reserved: true},
	{item: SlashItem{Name: "rename", Description: "Rename this session, /rename <title>",
		Run: SlashRun{Kind: SlashRunMethod, Method: SlashMethodSetTitle, Param: "title", Arg: SlashArgRequired, ArgHint: "<title>"}, WhileWorking: true}, reserved: true},
	{item: SlashItem{Name: "plan", Description: "Set plan mode"}, gate: slashModes, reserved: true},
	{item: SlashItem{Name: "ask", Description: "Set ask mode"}, gate: slashModes, reserved: true},
	{item: SlashItem{Name: "agent", Description: "Set agent mode"}, gate: slashModes, reserved: true},
	{item: SlashItem{Name: "btw", Description: "Ask a side question (not saved): /btw <question>", Requires: SlashRequiresAside,
		Run: SlashRun{Kind: SlashRunAside, Param: "question", Arg: SlashArgRequired, ArgHint: "<question>"}, WhileWorking: true}},
	{item: SlashItem{Name: "exit", Description: "End this session", Requires: SlashRequiresStop,
		Run: SlashRun{Kind: SlashRunMethod, Method: SlashMethodStop}, WhileWorking: true}, reserved: true},
	{item: SlashItem{Name: "effort", Description: "Change the current model's effort", Requires: SlashRequiresEffort,
		Run: SlashRun{Kind: SlashRunPicker, Picker: "effort", Arg: SlashArgOptional, ArgHint: "<level>"}}},
}

func init() {
	// Every row of the table is a builtin; saying so once here keeps the
	// literals above to what differs between them.
	for i := range slashBuiltinTable {
		slashBuiltinTable[i].item.Source = SlashSourceBuiltin
	}
}

// SlashBuiltin is one row of craze's own command table, every gate's: Item as
// the catalog lists it — a mode row's Run with its setting naming the
// canonical mode, before any session's modes resolve it — and Reserved, whether
// a plugin may take the name bare (BuiltinSlashNames).
type SlashBuiltin struct {
	Item     SlashItem
	Reserved bool
}

// SlashBuiltins is the whole table in the menu's order, a copy the caller
// owns. The TUI draws its builtin rows and descriptions from it, so the menu
// and the catalog a host sends cannot name or describe a builtin differently.
func SlashBuiltins() []SlashBuiltin {
	out := make([]SlashBuiltin, 0, len(slashBuiltinTable))
	for _, b := range slashBuiltinTable {
		it := b.item
		if b.gate == slashModes {
			it.Run = modeSlashRun(it.Name, []string{it.Name})
		}
		out = append(out, SlashBuiltin{Item: it, Reserved: b.reserved})
	}
	return out
}

// BuiltinSlashNames are craze's own slash commands that a plugin may never
// take bare: the reserved rows of the table (P12), in its order — exactly the
// ten the TUI has always answered itself. They live here so the session
// resolving plugin names and the menu drawing them agree on what is already
// spoken for: a plugin shipping a command called "help" has to show as
// plugin:help everywhere, because a bare row would be shadowed by the builtin
// in the menu and expanded by craze in `craze prompt` — the same name meaning
// two things.
//
// /btw and /connect are not reserved, as they never were, nor is /effort
// (plan 047 D4): each is a builtin only where the session has it, and a
// plugin's /btw or /effort stays bare.
func BuiltinSlashNames() []string {
	out := make([]string, 0, len(slashBuiltinTable))
	for _, b := range slashBuiltinTable {
		if b.reserved {
			out = append(out, b.item.Name)
		}
	}
	return out
}

// SlashInputs is everything the catalog is built from, and every one of them
// is a fact of the session (P4, P13): nothing a server or a client knows.
type SlashInputs struct {
	// Provider is the session's provider id (ProviderInfo.Name): /connect is
	// native's alone.
	Provider string
	// Capabilities gate the mode rows (Modes) and /tasks (Todos).
	Capabilities Capabilities
	// Modes is the ids of the modes the session advertised; with none, the
	// mode rows are not listed (the TUI's showModes).
	Modes []string
	// Commands is what the agent advertised, Plugins the on-disk entries
	// resolved against them (Snapshot.Plugins), Skills the provider's disk
	// skills the host scanned (DiscoverSkills; nil for native, whose skills
	// are plugin rows).
	Commands []CommandInfo
	Plugins  []PluginCommand
	Skills   []Skill
}

// BuildSlashCatalog is the catalog from in, pure (D1): the builtins under
// their gates, then the agent's commands, then the plugin rows, then the disk
// skills — the TUI's slashCatalog (internal/tui/slash.go) rule for rule, less
// /sessions (P13) and with /btw always listed (P4).
//
//   - The first source to claim a name keeps it, case-insensitively, so
//     source order is the whole precedence rule: an advertised command beats
//     a plugin row of the same name and a plugin row beats a disk skill. A
//     builtin claims its name before any of them — except a row with
//     Requires, which claims nothing (SlashItem.Requires says why).
//   - A name that is not one token (SlashNameOK) never reaches the catalog,
//     and a plugin row needs its plugin id and its qualified spelling to pass
//     too: the menu offers both spellings.
//   - Every non-builtin description is folded onto one line by DisplayLine,
//     and so are a plugin row's id and qualified spelling.
//   - Each mode row's Run is resolved against the advertised modes
//     (ResolveMode); one the session does not advertise is still listed, as
//     the TUI lists it, and cannot run (P15).
func BuildSlashCatalog(in SlashInputs) SlashCatalog {
	modes := in.Capabilities.Modes && len(in.Modes) > 0
	size := len(slashBuiltinTable) + len(in.Commands) + len(in.Plugins) + len(in.Skills)
	items := make([]SlashItem, 0, size)
	seen := make(map[string]struct{}, size)
	for _, b := range slashBuiltinTable {
		switch b.gate {
		case slashAlways:
		case slashModes:
			if !modes {
				continue
			}
		case slashTodos:
			if !in.Capabilities.Todos {
				continue
			}
		case slashNative:
			if in.Provider != nativeName {
				continue
			}
		}
		it := b.item
		if b.gate == slashModes {
			it.Run = modeSlashRun(it.Name, in.Modes)
		}
		items = append(items, it)
		if it.Requires == "" {
			seen[it.Name] = struct{}{}
		}
	}
	// add is the one gate every non-builtin source passes, as it is the TUI's:
	// an untypable name never reaches the catalog, and the first source to
	// claim a name keeps it.
	add := func(it SlashItem) {
		if !SlashNameOK(it.Name) {
			return
		}
		n := strings.ToLower(it.Name)
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		items = append(items, it)
	}
	prompt := SlashRun{Kind: SlashRunPrompt, Arg: SlashArgOptional}
	for _, c := range in.Commands {
		add(SlashItem{Name: c.Name, Description: DisplayLine(c.Description), Source: SlashSourceAgent, Run: prompt})
	}
	// Plugin rows sit between what the agent advertised and what the disk
	// scan found: the agent's own names win because craze expands nothing it
	// advertised — the resolver has already qualified anything that would
	// have collided — and a plugin row beats a disk skill of the same name
	// because only the plugin row says which plugin it came from.
	for _, p := range in.Plugins {
		if !SlashNameOK(p.Qualified) || !SlashNameOK(p.Plugin) {
			continue
		}
		it := SlashItem{
			Name:        p.Display,
			Description: DisplayLine(p.Description),
			Source:      SlashSourcePlugin,
			Plugin:      DisplayLine(p.Plugin),
			Run:         prompt,
		}
		if q := DisplayLine(p.Qualified); q != "" && !strings.EqualFold(q, p.Display) {
			it.Qualified = q
		}
		add(it)
	}
	for _, sk := range in.Skills {
		add(SlashItem{Name: sk.Name, Description: DisplayLine(sk.Description), Source: SlashSourceSkill, Run: prompt})
	}
	return SlashCatalog{Items: items}
}

// modeSlashRun is a mode row's Run: session.set to the id ResolveMode finds
// for name among the advertised modes, or unavailable in the TUI's own words
// for a mode the session does not have (internal/tui's runBuiltin).
func modeSlashRun(name string, advertised []string) SlashRun {
	id, ok := ResolveMode(name, advertised)
	if !ok {
		return SlashRun{Kind: SlashRunUnavailable, Reason: "mode " + name + " is not advertised"}
	}
	return SlashRun{Kind: SlashRunMethod, Method: SlashMethodSet, Setting: SlashSetting{Kind: "mode", Value: id}}
}

// SlashCatalogFor is BuildSlashCatalog over a session's snapshot and the disk
// skills its host scanned (P19): the one helper the live session, the native
// session and the stubs build their catalog through, so none of them derives
// the inputs its own way. The capabilities are the snapshot's provider's.
func SlashCatalogFor(snap Snapshot, skills []Skill) SlashCatalog {
	return SlashCatalogWith(snap, snap.Provider.Capabilities(), skills)
}

// SlashCatalogWith is SlashCatalogFor under caps rather than the snapshot
// provider's capabilities: the catalog a client with no catalog of its host's
// builds itself (the TUI's localSlash, P14), from the capabilities its backend
// advertised — over a socket, this binary's provider table need not be the
// host's.
func SlashCatalogWith(snap Snapshot, caps Capabilities, skills []Skill) SlashCatalog {
	return BuildSlashCatalog(SlashInputs{
		Provider:     snap.Provider.Name,
		Capabilities: caps,
		Modes:        modeIDs(snap.Modes),
		Commands:     snap.Commands,
		Plugins:      snap.Plugins,
		Skills:       skills,
	})
}

// SlashNameOK keeps the catalog to names a token could hold. A directory name
// or an advertised command with whitespace in it could never be typed as one
// token, so it would sit in the menu unreachable; a control character would
// break the row it draws on. Rejecting them here means every later stage — the
// filter, the accept, the row — can assume one clean token.
//
// utf8.RuneError goes with them, and for a sharper reason than looks: bubbles'
// rune sanitizer *drops* it (runeutil.go:67), so a name carrying one — or the
// invalid UTF-8 that range yields it for — would come out of the TUI's
// SetValue shorter than the string its accept measured the new cursor offset
// against, and the cursor would land that many bytes into the rest of the
// draft.
//
// It moved here from internal/tui with the catalog (plan 043 C1); the TUI
// calls it under its old name.
func SlashNameOK(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}
