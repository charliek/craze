package tui

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

// Plan 047 D4 (§7 A4): /effort in a session — listed where the current model
// has an effort option and nowhere else, after /exit; a bare /effort opens a
// picker of the current model's levels, marked current and (with effort
// facts) default; /effort <level> sets one; both go through /model's effort
// step, bound to the model; and like /model it is refused while a turn runs.

// effortPickerRows is the picker's level rows as drawn, each trimmed, between
// its title and its footer.
func effortPickerRows(t *testing.T, m Model) []string {
	t.Helper()
	if m.dialog != dialogEffort {
		t.Fatalf("the effort picker is not open (dialog %v)", m.dialog)
	}
	r := m.lay.Dialog
	lines := rows(plainView(m))
	var out []string
	for y := r.Y + 2; y < r.Y+r.H-2 && y < len(lines); y++ {
		ln := lines[y]
		start := strings.Index(ln, "│")
		end := strings.LastIndex(ln, "│")
		out = append(out, strings.TrimRight(ln[start+len("│"):end], " "))
	}
	return out
}

// pickerRow is a picker row as a 100-column frame draws it: row, and tag
// right-aligned in the box's 52 inner cells.
func pickerRow(row, tag string) string {
	if tag == "" {
		return row
	}
	return row + strings.Repeat(" ", 52-lipgloss.Width(row)-lipgloss.Width(tag)) + tag
}

// menuRowNamed is the slash menu's row named name over the draft "/", and
// whether there is one.
func menuRowNamed(m Model, name string) (slashItem, bool) {
	for _, it := range draft(m, "/").filteredSlash() {
		if it.Name == name {
			return it, true
		}
	}
	return slashItem{}, false
}

// TestEffortIsListedWhereTheModelHasAnEffortOption (A4): the builtin /effort
// row is in the menu after /exit where the current model has an effort
// option, and hides an agent's own /effort; on a model with none it is
// hidden, the agent's row shows in its place, and Enter sends /effort to the
// agent as the text it is — over the menu built here and over the host's
// catalog alike. Negative control: a projection that does not read the
// effort requirement (slashRowRuns treating it as no requirement) lists the
// builtin on none and fails there.
func TestEffortIsListedWhereTheModelHasAnEffortOption(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(map[bool]string{false: "the menu built here", true: "the host's catalog"}[publish], func(t *testing.T) {
			isolateSkillsHome(t)
			stub := NewStub()
			stub.PublishSlash = publish
			m := startStub(t, stub, t.TempDir(), 100, 30)
			stub.mu.Lock()
			stub.snap.Models = agent.CloneModels(factsModels())
			stub.snap.CurrentModel = "one"
			stub.mu.Unlock()
			stub.SetModelCatalogs(factsCatalogs())
			stub.SetCommands([]agent.CommandInfo{{Name: "effort", Description: "the agent's effort"}})
			m = republish(t, m)
			m = applyPending(t, m)

			var names []string
			for _, it := range draft(m, "/").filteredSlash() {
				if it.Builtin {
					names = append(names, it.Name)
				}
			}
			if i := slices.Index(names, "exit"); i < 0 || i+1 >= len(names) || names[i+1] != "effort" {
				t.Fatalf("the builtins are %v; want /effort right after /exit", names)
			}
			if it, ok := menuRowNamed(m, "effort"); !ok || !it.Builtin || it.Desc != "Change the current model's effort" {
				t.Fatalf("on one the effort row is %+v (%v); want the builtin", it, ok)
			}
			if n := strings.Count(strings.Join(slashNames(draft(m, "/")), " "), "effort"); n != 1 {
				t.Fatalf("the menu has %d effort rows; the builtin hides the agent's", n)
			}

			if _, err := stub.SetModel(context.Background(), "", "none"); err != nil {
				t.Fatal(err)
			}
			m = applyPending(t, m)
			if m.effortOffered() {
				t.Fatal("none has an effort option")
			}
			it, ok := menuRowNamed(m, "effort")
			if !ok || it.Builtin || it.Desc != "the agent's effort" {
				t.Fatalf("on none the effort row is %+v (%v); want the agent's own", it, ok)
			}
			m, _ = typeCommand(t, m, "/effort high")
			if m.dialog != dialogNone || !slices.Contains(stub.Prompts(), "/effort high") {
				t.Fatalf("on none /effort high was not sent to the agent: prompts %q, dialog %v", stub.Prompts(), m.dialog)
			}
		})
	}
}

// TestEffortComesAfterExitInTheMenu (A4): `/e` still selects /exit — Enter
// completes it and a second Enter runs it — and `/ef` lists /effort alone.
// Negative control: the row placed before /exit in the table makes `/e`'s
// first row /effort and fails the first check.
func TestEffortComesAfterExitInTheMenu(t *testing.T) {
	m, _ := factsStub(t, "one", true)
	m = draft(m, "/e")
	if got := slashNames(m); len(got) < 2 || got[0] != "exit" || got[1] != "effort" {
		t.Fatalf("/e lists %v; want /exit first, then /effort", got)
	}
	m = pressKey(t, m, tea.KeyEnter)
	if v := m.input.Value(); v != "/exit " {
		t.Fatalf("enter on /e left %q; want /exit completed", v)
	}
	m = pressKey(t, m, tea.KeyEnter)
	if !m.quitting {
		t.Fatal("the second enter did not run /exit")
	}

	m, _ = factsStub(t, "one", true)
	if got := slashNames(draft(m, "/ef")); !slices.Equal(got, []string{"effort"}) {
		t.Fatalf("/ef lists %v; want /effort alone", got)
	}
}

// TestTheEffortPickerMarksCurrentAndDefault (A4): a bare /effort opens the
// picker on the current model's levels, the cursor on the level in use,
// marked current, and — on a session with effort facts — the model's default
// marked default; without the facts nothing is marked default. Esc closes it
// and sends nothing. Negative control: an effortDefault that does not ask
// effortFacts marks high default without the facts and fails there.
func TestTheEffortPickerMarksCurrentAndDefault(t *testing.T) {
	for _, facts := range []bool{true, false} {
		m, stub := factsStub(t, "one", facts)
		writes := configWrites(stub)
		m, _ = typeCommand(t, m, "/effort")
		if m.input.Value() != "" {
			t.Fatalf("a bare /effort left the draft %q", m.input.Value())
		}
		high := "  high"
		if facts {
			high = pickerRow("  high", "default")
		}
		want := []string{pickerRow("> low", "current"), high}
		if got := effortPickerRows(t, m); !slices.Equal(got, want) {
			t.Fatalf("facts %v: the picker's rows are\n%q\nwant\n%q\n%s", facts, got, want, plainView(m))
		}
		if !strings.Contains(plainView(m), "effort · One") || !strings.Contains(plainView(m), effortPickerHint) {
			t.Fatalf("facts %v: the picker's title or footer:\n%s", facts, plainView(m))
		}
		m = pressKey(t, m, tea.KeyDown)
		m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
		m = applyPending(t, drainCmd(t, m, cmd))
		if m.dialog != dialogNone || len(writes()) != 0 {
			t.Fatalf("facts %v: esc left dialog %v and sent %v", facts, m.dialog, writes())
		}
	}
}

// TestTheEffortPickerAppliesToTheModelItShows (A4): Enter applies the level
// under the cursor through /model's effort step, bound to the model the
// picker lists (Setting.ForModel) — a model change landing between the
// step's read and its write is refused, the stale note written and nothing
// sent — and the picker follows a model change while it is open: the new
// model's levels, the cursor kept on a level it offers, applied to that
// model. Negative control: a runModelEffort that sends without ForModel lets
// the late move take the write on two, and fails the first case.
func TestTheEffortPickerAppliesToTheModelItShows(t *testing.T) {
	m, stub := factsStub(t, "one", true)
	writes := configWrites(stub)
	m, _ = typeCommand(t, m, "/effort")
	m = pressKey(t, m, tea.KeyDown)
	m, cmd := press(m, enter())
	// Another client's switch to two — which offers high too — lands right
	// after the step has read the session on one, before its write.
	stub.MoveModelOnRead("one", "two")
	m = applyPending(t, flushCmd(t, m, cmd))
	if got := texts(m, entryNote); !slices.Equal(got, []string{"effort not applied: the model changed"}) {
		t.Fatalf("notes %q", got)
	}
	if got := writes(); len(got) != 0 || stub.Snapshot().CurrentModel != "two" {
		t.Fatalf("the late move: sent %v, the session on %s", got, stub.Snapshot().CurrentModel)
	}

	m, stub = factsStub(t, "one", true)
	writes = configWrites(stub)
	m, _ = typeCommand(t, m, "/effort")
	m = pressKey(t, m, tea.KeyDown) // high
	if _, err := stub.SetModel(context.Background(), "", "two"); err != nil {
		t.Fatal(err)
	}
	m = applyPending(t, m)
	if !strings.Contains(plainView(m), "effort · Two") {
		t.Fatalf("the picker does not follow the switch to two:\n%s", plainView(m))
	}
	got := effortPickerRows(t, m)
	if len(got) != 6 || !strings.HasPrefix(got[3], "> high") || !strings.HasPrefix(got[0], "  minimal") {
		t.Fatalf("on two the picker's rows are %q; want two's six, the cursor kept on high", got)
	}
	m = applyDialog(t, m)
	if got := writes(); !slices.Equal(got, []string{"effort=high"}) {
		t.Fatalf("the picker sent %v", got)
	}
	if snap := stub.Snapshot(); snap.CurrentModel != "two" || optionCurrent(snap.Config, agent.NativeEffortID) != "high" {
		t.Fatalf("the session is on %s at %q", snap.CurrentModel, optionCurrent(snap.Config, agent.NativeEffortID))
	}
	if errs := texts(m, entryError); len(errs) != 0 {
		t.Fatalf("errors %q", errs)
	}
}

// drainCmd runs cmd to the end and hands the model everything it answers:
// each command within a step (runWatched), the members of every batch it
// answers run the same way — as the runtime runs them, which handing a
// tea.BatchMsg to Update does not (C2 fix review 4) — and what each message's
// Update returns after it; but for timers and the event reader, which never
// finish on their own (pumpSkips). So a check that nothing was sent cannot
// pass on a command left unrun, batched or not.
func drainCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil || pumpSkips(cmd) {
		return m
	}
	msg := runWatched(t, cmd)
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			m = drainCmd(t, m, c)
		}
		return m
	}
	if msg == nil {
		return m
	}
	tm, next := m.Update(msg)
	return drainCmd(t, tm.(Model), next)
}

// runEffortLine types line, presses Enter and drains the command it returned
// (drainCmd), then delivers the session's deltas — so a check that nothing
// was sent cannot pass on a command left unrun.
func runEffortLine(t *testing.T, m Model, line string) Model {
	t.Helper()
	m, cmd := typeCommand(t, m, line)
	return applyPending(t, drainCmd(t, m, cmd))
}

// TestEffortArguments (A4): /effort <level> applies a level the model offers,
// matched case-folded and sent in the agent's spelling; `default` is the
// model's default on a session with effort facts, and is refused without
// them; a word that names no level is refused — an error row naming the
// levels — and the draft is kept, with nothing sent. Negative control: a
// runEffort that clears the draft before the refusal fails the draft check.
func TestEffortArguments(t *testing.T) {
	m, stub := factsStub(t, "one", true)
	writes := configWrites(stub)
	m = runEffortLine(t, m, "/effort HIGH")
	if got := writes(); !slices.Equal(got, []string{"effort=high"}) || m.input.Value() != "" {
		t.Fatalf("/effort HIGH sent %v, left %q", got, m.input.Value())
	}
	if got := m.modelLabel(); !strings.Contains(got, "high") {
		t.Fatalf("the status row reads %q", got)
	}
	if _, err := stub.SetConfig(context.Background(), "", agent.NativeEffortID, "low", ""); err != nil {
		t.Fatal(err)
	}
	m = applyPending(t, m)
	m = runEffortLine(t, m, "/effort default")
	// The low between is this test's own write, standing in for another
	// client's.
	if got := writes(); !slices.Equal(got, []string{"effort=high", "effort=low", "effort=high"}) {
		t.Fatalf("/effort default sent %v; want one's default, high", got)
	}
	m = runEffortLine(t, m, "/effort nope")
	if got := texts(m, entryError); !slices.Equal(got, []string{"no effort nope: one of default, low, high"}) {
		t.Fatalf("errors %q", got)
	}
	if m.input.Value() != "/effort nope" {
		t.Fatalf("the draft is %q; want it kept", m.input.Value())
	}
	if got := writes(); len(got) != 3 {
		t.Fatalf("a refused level sent %v", got)
	}

	m, stub = factsStub(t, "one", false)
	writes = configWrites(stub)
	m = runEffortLine(t, m, "/effort default")
	if got := texts(m, entryError); !slices.Equal(got, []string{"no effort default: one of low, high"}) {
		t.Fatalf("without facts /effort default: errors %q", got)
	}
	if m.input.Value() != "/effort default" || len(writes()) != 0 {
		t.Fatalf("without facts /effort default left %q and sent %v", m.input.Value(), writes())
	}
}

// TestEffortIsRefusedWhileWorkRuns (A4): like /model, /effort needs the agent,
// so while a turn runs — craze's own, or one the agent runs on its own — it
// is refused: nothing is sent, no picker opens, the draft stays, and one error
// row, plan 046's busy refusal, says why. Negative control: effort added to
// runsWhileWorking sends high mid-turn and draws no row.
func TestEffortIsRefusedWhileWorkRuns(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		m, stub := factsStub(t, "one", true)
		writes := configWrites(stub)
		if foreign {
			stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "wake-1", Running: true})
			m = applyPending(t, m)
		} else {
			m.status = statusWorking
		}
		for _, line := range []string{"/effort high", "/effort"} {
			// The transcript is shared by every copy of the model, so each
			// refusal is counted from the rows before it.
			before := len(texts(m, entryError))
			m2, cmd := typeCommand(t, m, line)
			m2 = drainCmd(t, m2, cmd)
			want := busyRefusalText("effort")
			if got := texts(m2, entryError); len(got) != before+1 || got[len(got)-1] != want || !strings.Contains(plainView(m2), want) {
				t.Fatalf("foreign %v, %s mid-turn: error rows %q, want one more, %q, on screen", foreign, line, got, want)
			}
			if m2.dialog != dialogNone || len(writes()) != 0 || m2.input.Value() != line {
				t.Fatalf("foreign %v, %s mid-turn: dialog %v, sent %v, draft %q", foreign, line, m2.dialog, writes(), m2.input.Value())
			}
		}
	}
}

// TestEffortOverTheFakePerModelAgent (A4): the offline ACP case, over a real
// session and the fake per-model agent — cursor's wire, every model with an
// option catalog of its own. On claude-opus-5 /effort lists its levels, low to
// max, the cursor on its high, marked current and none marked default (an
// ACP option names no default); a level chosen there goes out as
// set_config_option and lands; and /effort default is refused with the levels
// listed. Negative control: a picker that lists the catalog the session
// started on (grok-4.6's) fails the first check.
func TestEffortOverTheFakePerModelAgent(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := t.TempDir()
	sess := agent.New(agent.Options{Binary: bin, ExtraArgs: []string{"-script=permodel"}, Workspace: ws, Force: true, Stderr: io.Discard})
	if err := sess.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "grok-4.6"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = applyPending(t, tm.(Model))
	if m.effortFacts() {
		t.Fatal("an ACP session states effort facts")
	}

	m = pumpEnter(t, m, "/model claude-opus-5")
	m = pumpSettled(t, m)
	if m.snap.CurrentModel != "claude-opus-5" {
		t.Fatalf("the session is on %s", m.snap.CurrentModel)
	}
	m = pumpEnter(t, m, "/effort")
	want := []string{"  low", "  medium", pickerRow("> high", "current"), "  xhigh", "  max"}
	if got := effortPickerRows(t, m); !slices.Equal(got, want) {
		t.Fatalf("on claude-opus-5 the picker's rows are\n%q\nwant\n%q", got, want)
	}
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = pumpSettled(t, m)
	if opt := agent.EffortOption(sess.Snapshot()); opt == nil || opt.Current != "xhigh" {
		t.Fatalf("the session's effort is %+v; want xhigh", opt)
	}
	if got := m.modelLabel(); !strings.Contains(got, "xhigh") {
		t.Fatalf("the status row reads %q", got)
	}

	m = pumpEnter(t, m, "/effort default")
	if got := texts(m, entryError); !slices.Equal(got, []string{"no effort default: one of low, medium, high, xhigh, max"}) {
		t.Fatalf("/effort default on ACP: errors %q", got)
	}
	if m.input.Value() != "/effort default" {
		t.Fatalf("the draft is %q", m.input.Value())
	}
}

// TestThePickerOnAModelWithNoLevelsSendsNothing (A4): the session moving to a
// model with no effort control under the open picker leaves it saying so,
// and Enter closes it with nothing sent. Negative control: a body that draws
// nothing for a model without levels fails the first check.
func TestThePickerOnAModelWithNoLevelsSendsNothing(t *testing.T) {
	m, stub := factsStub(t, "one", true)
	writes := configWrites(stub)
	m, _ = typeCommand(t, m, "/effort")
	if _, err := stub.SetModel(context.Background(), "", "none"); err != nil {
		t.Fatal(err)
	}
	m = applyPending(t, m)
	if view := plainView(m); !strings.Contains(view, "effort · None") || !strings.Contains(view, "this model has no effort levels") {
		t.Fatalf("on none the picker draws:\n%s", view)
	}
	m = pressKey(t, m, tea.KeyDown)
	m, cmd := press(m, enter())
	if m.dialog != dialogNone || cmd != nil || len(writes()) != 0 {
		t.Fatalf("enter on a model with no levels: dialog %v, a command %v, sent %v", m.dialog, cmd != nil, writes())
	}
}

// effortStub is a 100×30 model over a Stub on one model, "m", whose effort
// option is opt; facts makes the Stub state its models' effort facts, m's
// levels and default as given.
func effortStub(t *testing.T, opt agent.ConfigOption, facts bool, def string) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.PublishEffortFacts = facts
	m := startStub(t, stub, t.TempDir(), 100, 30)
	md := agent.ModelInfo{ID: "m", Name: "M"}
	if facts {
		for _, v := range opt.SelectValues {
			md.Efforts = append(md.Efforts, v.Value)
		}
		md.DefaultEffort, md.SwitchEffort = def, def
	}
	stub.mu.Lock()
	stub.snap.Models = []agent.ModelInfo{md}
	stub.snap.CurrentModel = "m"
	stub.mu.Unlock()
	stub.SetModelCatalogs(map[string][]agent.ConfigOption{"m": {opt}})
	m = republish(t, m)
	stubDeltas(t, stub)
	return m, stub
}

// selectOf is an effort select of values (id, name pairs), on current.
func selectOf(current string, values ...string) agent.ConfigOption {
	opt := agent.ConfigOption{ID: "effort", Name: "Effort", Category: "thought_level", Type: "select", Current: current}
	for i := 0; i+1 < len(values); i += 2 {
		opt.SelectValues = append(opt.SelectValues, agent.SelectValue{Value: values[i], Name: values[i+1]})
	}
	return opt
}

// TestEffortDefaultIsReserved (C2 review F2): `default` is decided before any
// level is matched — the model's default with effort facts, even where a
// level is spelled "default", and refused without them, even where the
// agent advertises a level spelled "default" (the picker still reaches it).
// Negative control: matching levels before the reserved word sends
// "default" in both cases.
func TestEffortDefaultIsReserved(t *testing.T) {
	m, stub := effortStub(t, selectOf("high", "default", "Default", "high", "High"), false, "")
	writes := configWrites(stub)
	m = runEffortLine(t, m, "/effort default")
	if got := writes(); len(got) != 0 {
		t.Fatalf("without facts /effort default sent %v", got)
	}
	if got := texts(m, entryError); !slices.Equal(got, []string{"no effort default: one of high"}) || m.input.Value() != "/effort default" {
		t.Fatalf("without facts /effort default: errors %q, draft %q", got, m.input.Value())
	}

	m, stub = effortStub(t, selectOf("default", "default", "default", "high", "high"), true, "high")
	writes = configWrites(stub)
	m = runEffortLine(t, m, "/effort default")
	if got := writes(); !slices.Equal(got, []string{"effort=high"}) {
		t.Fatalf("with facts /effort default sent %v; want the model's default, high", got)
	}
	if errs := texts(m, entryError); len(errs) != 0 {
		t.Fatalf("errors %q", errs)
	}
}

// TestEffortMatchesAnIDOrAName (C2 review F3, D4): /effort's word is matched
// as --effort's is — the id, then the name — and sent as the id: an agent
// whose levels are budget_low "Low" and budget_high "High" takes /effort
// high as budget_high. Negative control: the id-only MatchEffortValue
// refuses it.
func TestEffortMatchesAnIDOrAName(t *testing.T) {
	m, stub := effortStub(t, selectOf("budget_low", "budget_low", "Low", "budget_high", "High"), false, "")
	writes := configWrites(stub)
	m = runEffortLine(t, m, "/effort high")
	if got := writes(); !slices.Equal(got, []string{"effort=budget_high"}) {
		t.Fatalf("/effort high sent %v; errors %q", got, texts(m, entryError))
	}
}

// TestAPendingSwitchKeepsTheModelAndItsLevelsTogether (C2 review F1): while a
// /model switch of this client's is still waiting for its answer, the status
// row names the model asked for and the effort option is still the old
// model's. /effort is refused then — with an error row, nothing sent, the
// draft kept — rather than offer one model's levels under another's name;
// the model dialog, opening on the model asked for, draws that model's own
// levels from the facts, and none for a model that has none. Once the switch
// lands, /effort is the new model's. Enter on the dialog untouched sends the
// effort it showed for two, bound to two, behind the held switch (C2 fix
// review 1): two lands on max, not its catalog's high. Negative controls: a
// runEffort without the pending refusal opens the picker and fails the first
// check; a dialog that takes the status row's model for the catalog's draws
// one's levels on two and fails the dialog's; an apply that judges "a switch"
// against the status row's model sends nothing and fails the Enter's.
func TestAPendingSwitchKeepsTheModelAndItsLevelsTogether(t *testing.T) {
	m, stub := factsStub(t, "one", true)
	writes := configWrites(stub)
	m, held := typeCommand(t, m, "/model two")
	if m.snap.CurrentModel != "two" || optionCurrent(m.snap.Config, agent.NativeEffortID) != "low" {
		t.Fatalf("fixture: the screen is on %s with one's effort option %+v", m.snap.CurrentModel, m.snap.Config)
	}
	for _, line := range []string{"/effort", "/effort high"} {
		m = runEffortLine(t, m, line)
		if m.dialog != dialogNone || len(writes()) != 0 || m.input.Value() != line {
			t.Fatalf("%s while the switch is pending: dialog %v, sent %v, draft %q", line, m.dialog, writes(), m.input.Value())
		}
		m.input.SetValue("")
	}
	want := effortPendingText("two")
	if got := texts(m, entryError); !slices.Equal(got, []string{want, want}) {
		t.Fatalf("errors %q", got)
	}
	m = openDialog(t, m)
	if got := effortRow(t, m); got != "  effort  minimal  low  medium  high  xhigh  [max]" {
		t.Fatalf("the dialog on two, pending: the effort row is %q", got)
	}
	m, applied := press(m, enter())
	// The dialog's chain runs behind the held switch, so running it runs the
	// switch first (chainLock); the switch's own command then answers.
	m = drainCmd(t, m, applied)
	m = applyPending(t, drainCmd(t, m, held))
	if m.modelPending() || m.snap.CurrentModel != "two" {
		t.Fatalf("after the switch landed: pending %v, on %s", m.modelPending(), m.snap.CurrentModel)
	}
	if got := writes(); !slices.Equal(got, []string{"effort=max"}) {
		t.Fatalf("Enter on two, pending, sent %v; want the max it showed", got)
	}
	if snap := stub.Snapshot(); optionCurrent(snap.Config, agent.NativeEffortID) != "max" {
		t.Fatalf("two is at %q; want the max the dialog showed", optionCurrent(snap.Config, agent.NativeEffortID))
	}
	if got := texts(m, entryNote); !slices.Equal(got, []string{"effort → max"}) {
		t.Fatalf("notes %q", got)
	}
	m, _ = typeCommand(t, m, "/effort")
	if !strings.Contains(plainView(m), "effort · Two") || len(effortPickerRows(t, m)) != 6 {
		t.Fatalf("after the switch /effort is not two's:\n%s", plainView(m))
	}

	m, _ = factsStub(t, "one", true)
	m, _ = typeCommand(t, m, "/model none")
	m = openDialog(t, m)
	if got := effortRow(t, m); got != "" {
		t.Fatalf("the dialog on none, pending: the effort row is %q; none has no levels", got)
	}
}
