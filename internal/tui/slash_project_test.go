package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
)

// The menu's two paths (plan 043 §3.4 D7, P13–P15): the host's slash section
// projected, or the same builder run here where there is none — and dispatch,
// which reads neither.

// publishingStub is a Stub that publishes the slash catalog, its agent
// advertising exactly the named commands.
func publishingStub(names ...string) *Stub {
	stub := NewStub()
	stub.PublishSlash = true
	setStubCommands(stub, names...)
	return stub
}

// publishingModel is slashModel over a publishingStub: the menu draws the
// host's section (hostSlash).
func publishingModel(t *testing.T, names ...string) Model {
	t.Helper()
	isolateSkillsHome(t)
	return startStub(t, publishingStub(names...), t.TempDir(), 100, 30)
}

// menuNames is the menu's rows as name, builtin-or-not pairs, in order.
func menuNames(m Model) []string {
	var out []string
	for _, it := range m.slashCatalog() {
		mark := ""
		if it.Builtin {
			mark = "*"
		}
		out = append(out, mark+it.Name)
	}
	return out
}

// TestTheMenuBuildsItsOwnWhereTheHostHasNone is P14's other half: a session
// whose catalog has not reached the mirror — not spawned yet, started but not
// installed, or failed before its install — gets the menu this package always
// drew, built here, though its backend says it publishes one; and the install
// that brings the section changes nothing on screen.
//
// Negative control: a hostSlash that trusts the capability alone (no section
// yet) projects nothing — the pre-install and start-failure menus lose every
// row.
func TestTheMenuBuildsItsOwnWhereTheHostHasNone(t *testing.T) {
	t.Run("unstarted", func(t *testing.T) {
		m, _, _, _ := openedUnstarted(t, 100, 30)
		if m.eng != nil || m.slashHosted() {
			t.Fatalf("fixture: the unstarted session has a backend %v, draws the host's catalog %v", m.eng != nil, m.slashHosted())
		}
		sameMenu(t, "the unstarted session's menu", m.slashCatalog(), todaysSlashCatalog(m))
	})
	t.Run("before the install", func(t *testing.T) {
		isolateSkillsHome(t)
		stub := NewStub()
		stub.PublishSlash, stub.InstallOnStart = true, false
		m := startStub(t, stub, t.TempDir(), 100, 30)
		if !m.info().SlashCatalog || m.slashHosted() {
			t.Fatalf("before the install the backend publishes %v and the menu draws the host's %v; want true, false", m.info().SlashCatalog, m.slashHosted())
		}
		before := m.slashCatalog()
		sameMenu(t, "the menu before the install", before, todaysSlashCatalog(m))
		if len(before) < 8 {
			t.Fatalf("fixture: the menu before the install is %v", menuNames(m))
		}
		m = republish(t, m)
		if !m.slashHosted() || len(stub.Snapshot().Slash) == 0 {
			t.Fatal("the install's section did not reach the menu")
		}
		sameMenu(t, "the menu after the install", m.slashCatalog(), todaysSlashCatalog(m))
	})
	t.Run("a start that failed", func(t *testing.T) {
		isolateSkillsHome(t)
		stub := NewStub()
		stub.PublishSlash = true
		tm, _ := New(Config{Session: &failingStart{Stub: stub, err: errAuthFailed}, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true}).
			Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		m := tm.(Model)
		em, ok := runCmd(m.startCmd()).(errMsg)
		if !ok {
			t.Fatal("fixture: the start did not fail")
		}
		m = deliver(t, m, em)
		if m.status != statusError || !m.info().SlashCatalog || m.slashHosted() {
			t.Fatalf("status %s, the backend publishes %v, the menu draws the host's %v; want error, true, false", m.status, m.info().SlashCatalog, m.slashHosted())
		}
		got := m.slashCatalog()
		sameMenu(t, "the failed session's menu", got, todaysSlashCatalog(m))
		if len(got) < 8 {
			t.Fatalf("fixture: the failed session's menu is %v", menuNames(m))
		}
	})
}

// socketMenu is a TUI up over the socket in front of a host whose Stub has
// cmds advertised and entries resolved, and — publishing — the disk skills
// found under skillsWS as its host's scan; ws is the session's workspace,
// which the TUI's own walk reads.
func socketMenu(t *testing.T, publish bool, ws, skillsWS string, cmds []string, entries []agent.PluginEntry) Model {
	t.Helper()
	stub := NewStubNoPrimary()
	stub.PublishSlash = publish
	infos := make([]agent.CommandInfo, 0, len(cmds))
	for _, n := range cmds {
		infos = append(infos, agent.CommandInfo{Name: n, Description: "advertised " + n})
	}
	stub.mu.Lock()
	stub.snap.Commands = infos
	stub.snap.Plugins = agent.ResolvePluginNames(entries, cmds, false)
	if publish {
		stub.skills = agent.DiscoverSkills(agent.CursorProvider(), skillsWS, "")
	}
	stub.mu.Unlock()
	h := newAttachHostWith(t, stub, stub, true, control.Options{Workspace: ws})
	cfg := Config{Backend: dialHost(t, h, ws), Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: true}
	return upOverTheSocket(t, cfg, nil).m
}

// TestTheMenuOverTheSocketIsTheSameWithAndWithout is P14 over the wire: a TUI
// attached to a host from before the catalog (no slashCatalog capability: a
// Stub that publishes none) builds its menu here, from the snapshot's
// commands and plugins and its own walk of the session's workspace; one
// attached to a host that publishes it projects the snapshot's
// settings.slash, the skills the host found included; and the two menus are
// the menu this package drew before. A catalog the snapshot cut — more rows
// than SlashRows — is not drawn (recompute): the menu builds its own, every
// advertised command on it.
//
// Negative controls: a mirror that keeps a cut catalog (recompute's
// truncation rule removed) loses the commands past the 512th row in the third
// case; a remote backend that drops slashCatalog (sessionInfo) leaves the
// second case on the local path.
func TestTheMenuOverTheSocketIsTheSameWithAndWithout(t *testing.T) {
	cmds := []string{"research", "btw", "help", "Deploy"}
	entries := []agent.PluginEntry{
		pluginEntry("probe-plugin", "probe-echo", "the plugin one"),
		pluginEntry("alpha", "rescue", "one"),
		pluginEntry("beta", "rescue", "the other"),
	}
	skillsWS := t.TempDir()
	writeSkillMD(t, filepath.Join(skillsWS, ".cursor", "skills", "on-disk", "SKILL.md"), "---\nname: on-disk\ndescription: found by a walk\n---\n")

	isolateSkillsHome(t)
	older := socketMenu(t, false, skillsWS, "", cmds, entries)
	hosted := socketMenu(t, true, t.TempDir(), skillsWS, cmds, entries)
	if older.info().SlashCatalog || older.slashHosted() {
		t.Fatalf("the older host says slashCatalog %v; the menu draws its catalog %v", older.info().SlashCatalog, older.slashHosted())
	}
	if !hosted.info().SlashCatalog || !hosted.slashHosted() {
		t.Fatalf("the publishing host says slashCatalog %v; the menu draws its catalog %v", hosted.info().SlashCatalog, hosted.slashHosted())
	}
	if len(hosted.skills) != 0 {
		t.Fatalf("fixture: the hosted TUI's own walk found %+v", hosted.skills)
	}
	want := todaysSlashCatalog(older)
	sameMenu(t, "the menu over an older host", older.slashCatalog(), want)
	sameMenu(t, "the menu over a publishing host", hosted.slashCatalog(), want)
	if _, ok := catalogByName(hosted, "on-disk"); !ok {
		t.Fatalf("fixture: the host's skill is not on the menu %v", menuNames(hosted))
	}

	t.Run("a cut catalog", func(t *testing.T) {
		many := make([]string, 0, transcript.SlashRows+50)
		for i := range transcript.SlashRows + 50 {
			many = append(many, fmt.Sprintf("cmd%04d", i))
		}
		m := socketMenu(t, true, t.TempDir(), "", many, nil)
		if !m.info().SlashCatalog || m.slashHosted() {
			t.Fatalf("over a cut catalog the host says slashCatalog %v and the menu draws it %v; want true, false", m.info().SlashCatalog, m.slashHosted())
		}
		got := m.slashCatalog()
		sameMenu(t, "the menu over a cut catalog", got, todaysSlashCatalog(m))
		if _, ok := catalogByName(m, many[len(many)-1]); !ok {
			t.Fatalf("the last command is not on the menu (%d rows)", len(got))
		}
	})
}

// TestTypedModeOnASessionWithoutItIsRefused is P14's dispatch bar: /plan typed
// where the session shows no modes, and /ask where it shows modes but not
// that one, is refused with the line it always was — never sent to the agent
// as text — whether the menu draws the host's catalog (which lists no mode row
// in the first case and an unavailable one in the second) or builds its own.
//
// Negative control: a dispatch that sent a builtin the catalog does not list
// as text (handleEnter consulting slashCatalog) fails the first case.
func TestTypedModeOnASessionWithoutItIsRefused(t *testing.T) {
	for _, publish := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			modes []agent.ModeInfo
			typed string
		}{
			{"no modes", nil, "/plan"},
			{"ask not advertised", []agent.ModeInfo{{ID: "agent"}, {ID: "plan"}}, "/ask"},
		} {
			t.Run(fmt.Sprintf("%s, catalog published %v", tc.name, publish), func(t *testing.T) {
				isolateSkillsHome(t)
				stub := NewStub()
				stub.PublishSlash = publish
				stub.snap.Modes = tc.modes
				m := startStub(t, stub, t.TempDir(), 100, 30)
				if m.slashHosted() != publish {
					t.Fatalf("fixture: the menu draws the host's catalog %v", m.slashHosted())
				}
				name := strings.TrimPrefix(tc.typed, "/")
				it, listed := catalogByName(m, name)
				if listed != (tc.modes != nil) || (listed && !it.Builtin) {
					t.Fatalf("/%s on the menu %v (%+v)", name, listed, it)
				}
				m.input.SetValue(tc.typed)
				tm, _ := m.Update(enter())
				m = tm.(Model)
				if errs := texts(m, entryError); !slices.Equal(errs, []string{"mode " + name + " is not advertised"}) {
					t.Fatalf("error rows %q", errs)
				}
				if m.status == statusWorking || len(stub.Prompts()) != 0 || len(texts(m, entryUser)) != 0 {
					t.Fatalf("%s went to the agent: status %s, prompts %q", tc.typed, m.status, stub.Prompts())
				}
			})
		}
	}
}

// TestTheSessionsRowIsTheClients (P13): the host never lists /sessions, so an
// agent's own /sessions shows where this client has no session list, and
// where it has one the client's row — before /exit, as it always was — wins
// over it and hides it.
//
// Negative control: a projection that does not seed the client row's name
// lists the agent's /sessions beside it in the second case.
func TestTheSessionsRowIsTheClients(t *testing.T) {
	m := publishingModel(t, "sessions", "research")
	it, ok := catalogByName(m, "sessions")
	if !m.slashHosted() || !ok || it.Builtin || it.Desc != "advertised sessions" {
		t.Fatalf("without a session list /sessions is %+v (on the menu %v); want the agent's", it, ok)
	}

	isolateSkillsHome(t)
	tm, _ := New(Config{Session: publishingStub("sessions", "research"), Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true, Sessions: &fakeSessions{}}).
		Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = startedLikeInit(t, tm.(Model))
	if !m.slashHosted() {
		t.Fatal("fixture: the menu builds its own catalog")
	}
	names := menuNames(m)
	i := slices.Index(names, "*sessions")
	if i < 0 || i+1 >= len(names) || names[i+1] != "*exit" || slices.Contains(names, "sessions") {
		t.Fatalf("with a session list the menu is %v; want the client's /sessions before /exit and no agent row", names)
	}
}

// TestARequiresAsideBtwYieldsToTheAgents (plan 043 A1): where the session does
// not answer side questions the builtin /btw (requires aside) is hidden and
// the agent's own /btw shows in its place; where it does, the builtin shows
// and the agent's is hidden.
//
// Negative control: a projection that ignores requires keeps the builtin in
// the first case.
func TestARequiresAsideBtwYieldsToTheAgents(t *testing.T) {
	m := publishingModel(t, "btw")
	it, ok := catalogByName(m, "btw")
	if !m.slashHosted() || m.btwOffered() || !ok || it.Builtin || it.Desc != "advertised btw" {
		t.Fatalf("on cursor /btw is %+v (on the menu %v); want the agent's", it, ok)
	}

	isolateSkillsHome(t)
	m = startSession(t, AsidingStub{Stub: publishingStub("btw")}, t.TempDir(), 100, 30)
	it, ok = catalogByName(m, "btw")
	if !m.slashHosted() || !m.btwOffered() || !ok || !it.Builtin || it.Desc != btwBuiltin.Desc {
		t.Fatalf("where side questions are answered /btw is %+v (on the menu %v); want the builtin", it, ok)
	}
	if n := strings.Count(strings.Join(menuNames(m), ","), "btw"); n != 1 {
		t.Fatalf("/btw is on the menu %d times: %v", n, menuNames(m))
	}
}

// TestTheMenuHidesRowsItCannotRun (P17, plan 043 A9): of a section a later
// host might send, each row this client could not run is hidden — and only
// that row: an unknown source, run kind, method, requires, local action,
// picker or argument, a builtin its dispatch does not answer to, a plugin row
// with no plugin id or an untypable second spelling. A hidden row claims no
// name, so a later row of its name shows. An unavailable row, and /exit
// (requires stop: this client's own quit), show; a description is folded
// onto one line.
//
// Negative controls (each by hand): every one of slashRowRuns' and
// slashRunKnown's checks removed in turn lists its row.
func TestTheMenuHidesRowsItCannotRun(t *testing.T) {
	m := publishingModel(t)
	prompt := agent.SlashRun{Kind: agent.SlashRunPrompt, Arg: agent.SlashArgOptional}
	agentRow := func(name string, r agent.SlashRun) agent.SlashItem {
		return agent.SlashItem{Name: name, Source: agent.SlashSourceAgent, Run: r}
	}
	rows := []agent.SlashItem{
		{Name: "help", Description: "Keybindings and commands", Source: agent.SlashSourceBuiltin, WhileWorking: true,
			Run: agent.SlashRun{Kind: agent.SlashRunLocal, Local: "help"}},
		{Name: "ask", Description: "Set ask mode", Source: agent.SlashSourceBuiltin,
			Run: agent.SlashRun{Kind: agent.SlashRunUnavailable, Reason: "mode ask is not advertised"}},
		{Name: "src", Source: "macro", Run: prompt},
		agentRow("kind", agent.SlashRun{Kind: "teleport"}),
		agentRow("nokind", agent.SlashRun{}),
		agentRow("meth", agent.SlashRun{Kind: agent.SlashRunMethod, Method: "session.fly"}),
		{Name: "req", Source: agent.SlashSourceAgent, Requires: "warp", Run: prompt},
		agentRow("loc", agent.SlashRun{Kind: agent.SlashRunLocal, Local: "compact"}),
		agentRow("pick", agent.SlashRun{Kind: agent.SlashRunPicker, Picker: "colour"}),
		agentRow("argy", agent.SlashRun{Kind: agent.SlashRunPrompt, Arg: "many"}),
		{Name: "compact", Source: agent.SlashSourceBuiltin, Run: agent.SlashRun{Kind: agent.SlashRunLocal, Local: "help"}},
		{Name: "connect", Source: agent.SlashSourceBuiltin, Run: agent.SlashRun{Kind: agent.SlashRunLocal, Local: "connect"}},
		{Name: "plug", Source: agent.SlashSourcePlugin, Plugin: "p", Qualified: "p :plug", Run: prompt},
		{Name: "plug2", Source: agent.SlashSourcePlugin, Run: prompt},
		{Name: "dup", Source: "macro", Run: prompt},
		{Name: "dup", Description: "the second dup", Source: agent.SlashSourceAgent, Run: prompt},
		{Name: "keep", Description: "two\nlines \x1b[1mbold\x1b[0m", Source: agent.SlashSourceAgent, Run: prompt},
		{Name: "review", Description: "review it", Source: agent.SlashSourcePlugin, Plugin: "pr-tools", Qualified: "pr-tools:review", Run: prompt},
		{Name: "exit", Description: "End this session", Source: agent.SlashSourceBuiltin, Requires: agent.SlashRequiresStop, WhileWorking: true,
			Run: agent.SlashRun{Kind: agent.SlashRunMethod, Method: agent.SlashMethodStop}},
	}
	m = feed(t, m, agent.Event{Type: agent.EventMeta, Seq: m.foldedSeq() + 1, State: &agent.StateDelta{Slash: &agent.SlashCatalog{Items: rows}}})
	if !m.slashHosted() {
		t.Fatal("fixture: the section did not reach the menu")
	}
	if got, want := menuNames(m), []string{"*help", "*ask", "dup", "keep", "review", "*exit"}; !slices.Equal(got, want) {
		t.Fatalf("the menu is %v, want %v", got, want)
	}
	if it, _ := catalogByName(m, "dup"); it.Desc != "the second dup" {
		t.Fatalf("/dup is %+v, want the second row", it)
	}
	if it, _ := catalogByName(m, "keep"); it.Desc != "two lines bold" {
		t.Fatalf("/keep's description %q was not folded onto one line", it.Desc)
	}
	if it, _ := catalogByName(m, "review"); it.labeledDesc() != "review it (pr-tools)" || it.qualifiedAlias() != "pr-tools:review" {
		t.Fatalf("the plugin row is %+v", it)
	}
}

// TestASectionFromASessionThatPublishesNoneIsNotDrawn: the menu draws a
// section only where the backend says the session publishes one (D5) — a
// section folded from a session that says it does not leaves the menu built
// here.
//
// Negative control: a hostSlash that reads the section alone lists
// /only-in-section.
func TestASectionFromASessionThatPublishesNoneIsNotDrawn(t *testing.T) {
	m, _ := slashModel(t, 100, 30, "research")
	row := agent.SlashItem{Name: "only-in-section", Source: agent.SlashSourceAgent, Run: agent.SlashRun{Kind: agent.SlashRunPrompt}}
	m = feed(t, m, agent.Event{Type: agent.EventMeta, Seq: m.foldedSeq() + 1, State: &agent.StateDelta{Slash: &agent.SlashCatalog{Items: []agent.SlashItem{row}}}})
	if m.snap.Slash == nil || m.info().SlashCatalog {
		t.Fatalf("fixture: the mirror holds %+v, the backend publishes %v", m.snap.Slash, m.info().SlashCatalog)
	}
	if m.slashHosted() {
		t.Fatal("the menu draws a section the session does not say it publishes")
	}
	if _, ok := catalogByName(m, "only-in-section"); ok {
		t.Fatalf("the menu lists the section's row: %v", menuNames(m))
	}
	sameMenu(t, "the menu", m.slashCatalog(), todaysSlashCatalog(m))
}

// TestTheBareSlashRescanIsTheLocalMenus (plan 043 D7, X2): a bare "/" walks the
// disk for skills only where the menu builds its own catalog, as it always
// did; where it draws the host's, the host finds skills at its own triggers
// and nothing is walked here — a skill saved since the session came up is not
// on the menu until the host's next scan publishes it.
//
// Negative control: the rescan's slashHosted gate removed walks the disk on the
// hosted model, which then holds the new skill.
func TestTheBareSlashRescanIsTheLocalMenus(t *testing.T) {
	skill := "---\nname: later\ndescription: saved after the start\n---\n"
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprintf("catalog published %v", publish), func(t *testing.T) {
			isolateSkillsHome(t)
			stub := NewStub()
			stub.PublishSlash = publish
			ws := t.TempDir()
			m := startStub(t, stub, ws, 100, 30)
			if m.slashHosted() != publish {
				t.Fatalf("fixture: the menu draws the host's catalog %v", m.slashHosted())
			}
			writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "later", "SKILL.md"), skill)
			tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
			m = tm.(Model)
			walked := slices.ContainsFunc(m.skills, func(s agent.Skill) bool { return s.Name == "later" })
			_, listed := catalogByName(m, "later")
			if walked != !publish || listed != !publish {
				t.Fatalf("after a bare /, the walk found the new skill %v and the menu lists it %v; want %v", walked, listed, !publish)
			}
			if publish {
				// The host's scan is what brings it (the live session's
				// scanner; the Stub's SetSkills stands in for it).
				stub.SetSkills(agent.DiscoverSkills(agent.CursorProvider(), ws, homeDir()))
				m = applyPending(t, m)
				if it, ok := catalogByName(m, "later"); !ok || !it.Skill {
					t.Fatalf("the host's scan did not bring the skill to the menu: %v", menuNames(m))
				}
			}
		})
	}
}

// slashSection feeds m a slash section of rows, as the host's next delta.
func slashSection(t *testing.T, m Model, rows ...agent.SlashItem) Model {
	t.Helper()
	return feed(t, m, agent.Event{Type: agent.EventMeta, Seq: m.foldedSeq() + 1, State: &agent.StateDelta{Slash: &agent.SlashCatalog{Items: rows}}})
}

// helpLists reports whether /help's command lines name /name.
func helpLists(m Model, name string) bool {
	return slices.ContainsFunc(m.helpCommandLines(), func(l helpLine) bool { return l.key == "/"+name })
}

// TestLeavingTheHostsCatalogWalksTheDiskAgain (astra C3 T4): a skill saved
// after the session came up reaches the menu through the host's scan, and a
// bare "/" while the host's catalog is drawn walks nothing here — so when the
// menu goes back to building its own (here a section that cleared the
// catalog; a reconnect's snapshot that cut it or left it out is the same
// switch in recompute), the skills this client holds are walked again, and
// the skill stays on the menu and in /help.
//
// Negative control: recompute without the walk on the switch loses the
// skill from both.
func TestLeavingTheHostsCatalogWalksTheDiskAgain(t *testing.T) {
	isolateSkillsHome(t)
	stub := publishingStub()
	ws := t.TempDir()
	m := startStub(t, stub, ws, 100, 30)
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "later", "SKILL.md"), "---\nname: later\ndescription: saved after the start\n---\n")
	stub.SetSkills(agent.DiscoverSkills(agent.CursorProvider(), ws, homeDir()))
	m = applyPending(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.slashHosted() || len(m.skills) != 0 {
		t.Fatalf("fixture: the menu draws the host's catalog %v; this client's walk holds %+v", m.slashHosted(), m.skills)
	}
	if it, ok := catalogByName(m, "later"); !ok || !it.Skill || !helpLists(m, "later") {
		t.Fatalf("fixture: the host's scan did not bring the skill to the menu: %v", menuNames(m))
	}

	m = slashSection(t, m)
	if m.slashHosted() {
		t.Fatal("fixture: a cleared section left the menu on the host's catalog")
	}
	if it, ok := catalogByName(m, "later"); !ok || !it.Skill {
		t.Fatalf("the skill left the menu with the host's catalog: %v", menuNames(m))
	}
	if !helpLists(m, "later") {
		t.Fatal("the skill left /help with the host's catalog")
	}
}

// helpless is a section from a later host that gives /help an action this
// client does not know — so the row is hidden (P17) — and lists an agent's
// /helpful.
func helpless() []agent.SlashItem {
	return []agent.SlashItem{
		{Name: "help", Description: "Keybindings and commands", Source: agent.SlashSourceBuiltin, WhileWorking: true,
			Run: agent.SlashRun{Kind: agent.SlashRunLocal, Local: "assist"}},
		{Name: "exit", Description: "End this session", Source: agent.SlashSourceBuiltin, Requires: agent.SlashRequiresStop, WhileWorking: true,
			Run: agent.SlashRun{Kind: agent.SlashRunMethod, Method: agent.SlashMethodStop}},
		{Name: "helpful", Description: "the agent's", Source: agent.SlashSourceAgent,
			Run: agent.SlashRun{Kind: agent.SlashRunPrompt, Arg: agent.SlashArgOptional}},
	}
}

// TestAFullyTypedBuiltinTheHostHidesStillRuns (astra C3 T5): where the host's
// catalog hides /help and offers /helpful, a fully typed /help still opens
// help on Enter and completes to itself on Tab, as when the menu was always
// built here and help's row came first — never /helpful. A partly typed
// /hel completes to the row the menu shows, and a row the user moved to is
// the one Enter takes.
//
// Negative control: Enter's and Tab's slashTypedBuiltin checks removed — Enter
// writes /helpful into the composer, and Tab too.
func TestAFullyTypedBuiltinTheHostHidesStillRuns(t *testing.T) {
	fresh := func(t *testing.T) Model {
		t.Helper()
		m := slashSection(t, publishingModel(t), helpless()...)
		if _, ok := catalogByName(m, "help"); ok || !m.slashHosted() {
			t.Fatalf("fixture: the menu is %v (the host's %v)", menuNames(m), m.slashHosted())
		}
		return m
	}

	m, _ := press(draft(fresh(t), "/help"), enter())
	if m.dialog != dialogHelp || m.input.Value() != "" {
		t.Fatalf("Enter on /help: dialog %v, composer %q; want help open", m.dialog, m.input.Value())
	}

	m, _ = press(draft(fresh(t), "/help"), tea.KeyMsg{Type: tea.KeyTab})
	if m.input.Value() != "/help " {
		t.Fatalf("Tab on /help wrote %q, want %q", m.input.Value(), "/help ")
	}

	m, _ = press(draft(fresh(t), "/hel"), enter())
	if m.input.Value() != "/helpful " || m.dialog == dialogHelp {
		t.Fatalf("Enter on /hel wrote %q (dialog %v), want the menu's row %q", m.input.Value(), m.dialog, "/helpful ")
	}
}

// TestAFullyTypedBuiltinTheMenuDoesNotListStillCompletes pins how narrow
// slashTypedBuiltin is: Enter and Tab are exactly what they were before the
// catalog, for every host's catalog today. /plan on a session with no modes is
// on no menu — neither the host's nor the one built here — so Enter on it
// completes to the agent's /planner, as it always has, whichever menu is
// drawn.
//
// Negative control: a slashTypedBuiltin that runs every name the dispatch
// answers to (without the menu built here listing it) refuses /plan instead.
func TestAFullyTypedBuiltinTheMenuDoesNotListStillCompletes(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprintf("catalog published %v", publish), func(t *testing.T) {
			isolateSkillsHome(t)
			stub := NewStub()
			stub.PublishSlash = publish
			stub.snap.Modes = nil
			setStubCommands(stub, "planner")
			m := startStub(t, stub, t.TempDir(), 100, 30)
			if m.slashHosted() != publish {
				t.Fatalf("fixture: the menu draws the host's catalog %v", m.slashHosted())
			}
			m, _ = press(draft(m, "/plan"), enter())
			if m.input.Value() != "/planner " || len(texts(m, entryError)) != 0 {
				t.Fatalf("Enter on /plan wrote %q (errors %q), want the completion %q", m.input.Value(), texts(m, entryError), "/planner ")
			}
		})
	}
}

// stopFacts is the in-process backend with Info().Stop as a test sets it: a
// session whose host does or does not serve session.stop.
type stopFacts struct {
	backend.Backend
	eng  *engine.Engine
	stop bool
}

func (b stopFacts) engine() *engine.Engine { return b.eng }

func (b stopFacts) Info() backend.SessionInfo {
	info := b.Backend.Info()
	info.Stop = b.stop
	return info
}

// TestARequiresStopRowNeedsStop (astra C3, "requires stop"): only the builtin
// /exit is shown whatever the host serves — it is this client's own quit. Any
// other row that requires stop is shown only where Stop ends the session;
// where it does not, it is hidden and claims no name, so a later row of its
// name shows.
//
// Negative controls: requires stop shown unconditionally lists the gated
// /halt where the host cannot stop; gating /exit with it loses /exit there.
func TestARequiresStopRowNeedsStop(t *testing.T) {
	prompt := agent.SlashRun{Kind: agent.SlashRunPrompt, Arg: agent.SlashArgOptional}
	rows := []agent.SlashItem{
		{Name: "exit", Description: "End this session", Source: agent.SlashSourceBuiltin, Requires: agent.SlashRequiresStop, WhileWorking: true,
			Run: agent.SlashRun{Kind: agent.SlashRunMethod, Method: agent.SlashMethodStop}},
		{Name: "halt", Description: "needs stop", Source: agent.SlashSourceAgent, Requires: agent.SlashRequiresStop, Run: prompt},
		{Name: "halt", Description: "the ungated halt", Source: agent.SlashSourceAgent, Run: prompt},
	}
	for _, tc := range []struct {
		stop bool
		halt string
	}{{false, "the ungated halt"}, {true, "needs stop"}} {
		t.Run(fmt.Sprintf("stop %v", tc.stop), func(t *testing.T) {
			isolateSkillsHome(t)
			stub := publishingStub()
			eng, err := engine.New(stub, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = eng.Close() })
			ws := t.TempDir()
			b := stopFacts{Backend: newEngineBackend(eng, ws), eng: eng, stop: tc.stop}
			tm, _ := New(Config{Backend: b, Theme: "tokyo-night", Workspace: ws, Yolo: true}).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			m := slashSection(t, startedLikeInit(t, tm.(Model)), rows...)
			if !m.slashHosted() || m.stopOffered() != tc.stop {
				t.Fatalf("fixture: the menu draws the host's catalog %v, stop %v", m.slashHosted(), m.stopOffered())
			}
			if got, want := menuNames(m), []string{"*exit", "halt"}; !slices.Equal(got, want) {
				t.Fatalf("the menu is %v, want %v", got, want)
			}
			if it, _ := catalogByName(m, "halt"); it.Desc != tc.halt {
				t.Fatalf("/halt is %q, want %q", it.Desc, tc.halt)
			}
		})
	}
}
