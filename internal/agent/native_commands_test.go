package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/journal"
)

// A native session that finds its own commands and skills and expands them
// into its turns (plan 022 §3.2, §3.3). The scan itself is pinned by
// native_content_test.go and the expansion by native_expand_test.go; these are
// the two wired to a real session — what the menu gets, what the model gets,
// and what neither of them gets.

// nativeContent is a started native session over content the test wrote: a
// workspace that is its own repository and a home beside it, with craze's own
// diagnostics captured rather than printed.
type nativeContent struct {
	*nativeSession
	f    *nativeFixture
	ws   string // the physical spelling, which is what an entry's Root holds
	home string
	diag *bytes.Buffer
}

// startContent builds that session. opts carries whatever else the case needs;
// its Workspace, ContentHome and Diag are this helper's.
func startContent(t *testing.T, f *nativeFixture, opts Options, ws, home map[string]string) *nativeContent {
	t.Helper()
	base := t.TempDir()
	wsDir := writeTree(t, filepath.Join(base, "ws"), ws)
	gitDir(t, wsDir)
	homeDir := writeTree(t, filepath.Join(base, "home"), home)
	diag := &bytes.Buffer{}
	opts.Workspace, opts.ContentHome, opts.Diag = wsDir, homeDir, diag
	return &nativeContent{
		nativeSession: f.started(opts),
		f:             f,
		ws:            physical(t, wsDir),
		home:          homeDir,
		diag:          diag,
	}
}

// commandDoc is commandFile with extra frontmatter lines, for the two keys
// only native reads.
func commandDoc(desc, body string, extra ...string) string {
	fm := "---\ndescription: " + desc + "\n"
	for _, line := range extra {
		fm += strings.TrimSuffix(line, "\n") + "\n"
	}
	return fm + "---\n" + body + "\n"
}

// userTexts is every user message of one request, in order: the system prompt
// leads a request and the history follows it, so a case that wants "what the
// user's turn carried" has to find it rather than count to it.
func userTexts(call fantasy.Call) []string {
	var out []string
	for _, m := range call.Prompt {
		if m.Role == fantasy.MessageRoleUser {
			out = append(out, userTextOf(m))
		}
	}
	return out
}

// firstUserText is the prompt one turn opened with.
func firstUserText(t *testing.T, call fantasy.Call) string {
	t.Helper()
	texts := userTexts(call)
	if len(texts) == 0 {
		t.Fatalf("the request carried no user message: %+v", call.Prompt)
	}
	return texts[0]
}

// journalBytes is the journal file as it was written, for the assertions that
// are about a string being absent rather than about a record's shape.
func journalBytes(t *testing.T, w *journal.Writer) string {
	t.Helper()
	b, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// displays is the menu's rows as it would draw them, in order.
func displays(rows []PluginCommand) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Display)
	}
	return out
}

func wantDisplays(t *testing.T, rows []PluginCommand, want ...string) {
	t.Helper()
	if got := displays(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("menu rows %v, want %v", got, want)
	}
}

// TestNativeStartNamesItsContent is A4 through a real Start: bare when a name
// is unique, qualified when two sources claim it or a builtin already means
// it, and the pseudo ids spelled the way the menu labels them.
func TestNativeStartNamesItsContent(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{},
		map[string]string{
			".claude/commands/ship.md":       commandFile("ship it", "ship body"),
			".claude/commands/help.md":       commandFile("a name craze owns", "help body"),
			".claude/skills/deploy/SKILL.md": skillDoc("deploy", "deploy it"),
			".agents/skills/probe/SKILL.md":  skillDoc("probe", "probe it"),
		},
		map[string]string{
			".claude/commands/ship.md":   commandFile("the user's ship", "user ship body"),
			".claude/commands/rescue.md": commandFile("rescue", "rescue body"),
		})

	// project:ship and user:ship collide, so both are qualified; help is a
	// craze builtin, so the project's is qualified against it even though
	// nothing else claims the bare name. Everything else is bare. The order
	// is the scan's: the workspace's commands, its skills, then the user's.
	wantDisplays(t, s.Snapshot().Plugins,
		"project:help", "project:ship", "deploy", "probe", "rescue", "user:ship")

	// X2's reading of A4: a bare-displayed row answers to both spellings, and
	// a row the resolver qualified answers to the qualified one only.
	s.mu.Lock()
	lookup := buildPluginLookup(s.plugins, s.snap.Plugins)
	s.mu.Unlock()
	for _, spelling := range []string{"deploy", "project:deploy", "project:ship", "rescue", "user:rescue"} {
		if _, ok := lookup[spelling]; !ok {
			t.Fatalf("/%s resolves to nothing", spelling)
		}
	}
	if _, ok := lookup["ship"]; ok {
		t.Fatal("a bare /ship resolves although two entries claim the name")
	}
	if _, ok := lookup["help"]; ok {
		t.Fatal("a bare /help was taken from craze's own builtin")
	}
}

// TestNativeStartSkipsAReservedPluginID is A4's other half, in both
// directions: a real plugin calling itself project or user is skipped whole,
// with one line, because two different things answering to "project:" would
// make the menu's spelling a lie about which file a name expands.
func TestNativeStartSkipsAReservedPluginID(t *testing.T) {
	for _, id := range []string{nativeProjectID, nativeUserID} {
		t.Run(id, func(t *testing.T) {
			f := newNativeFixture(t)
			base := t.TempDir()
			wsDir := writeTree(t, filepath.Join(base, "ws"), map[string]string{
				".claude/commands/own.md": commandFile("the project's own", "own body"),
			})
			gitDir(t, wsDir)
			homeDir := writeTree(t, filepath.Join(base, "home"), nil)
			claudeFixture{
				installs: map[string][]claudeInstall{id + "@mkt": {{
					Scope: "user",
					InstallPath: writeTree(t, filepath.Join(homeDir, "install"), map[string]string{
						"commands/taken.md": commandFile("the impostor's", "impostor body"),
					}),
				}}},
				user: map[string]bool{id + "@mkt": true},
			}.write(t, homeDir, wsDir)

			diag := &bytes.Buffer{}
			s := f.started(Options{Workspace: wsDir, ContentHome: homeDir, Diag: diag})
			wantDisplays(t, s.Snapshot().Plugins, "own")
			if !strings.Contains(diag.String(), "plugin "+`"`+id+`"`+" skipped") {
				t.Fatalf("diagnostics %q say nothing about the reserved id", diag.String())
			}
		})
	}
}

// TestNativeStartProjections is A5: naming runs over every entry, the menu
// gets the ones that are not hidden, a hidden entry does not expand when it is
// typed, a disable-model-invocation entry is in the menu all the same, and
// Snapshot hands out a copy.
func TestNativeStartProjections(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{},
		map[string]string{
			".claude/skills/quiet/SKILL.md": skillDoc("quiet", "not for the user", "user-invocable: false"),
			".claude/commands/loud.md":      commandDoc("for the user only", "loud body", "disable-model-invocation: true"),
		},
		map[string]string{
			// A second entry called quiet, so the hidden one's part in naming
			// is visible: the survivor is qualified because two entries
			// claimed the name, hidden or not.
			".claude/commands/quiet.md": commandFile("the user's quiet", "user quiet body"),
		})

	snap := s.Snapshot()
	wantDisplays(t, snap.Plugins, "loud", "user:quiet")
	// The hidden entry is still on the list the catalog will be built from
	// (C6), which is the whole point of two projections over one list.
	s.mu.Lock()
	entries := append([]PluginEntry(nil), s.plugins...)
	s.mu.Unlock()
	if len(entries) != 3 {
		t.Fatalf("%d entries, want the three that were found", len(entries))
	}
	// disable-model-invocation is the opposite projection: the menu keeps it.
	if entries[0].Name != "loud" || !entries[0].NoModel {
		t.Fatalf("the command's entry is %+v, want loud with NoModel", entries[0])
	}
	if entries[1].Name != "quiet" || !entries[1].Hidden {
		t.Fatalf("the skill's entry is %+v, want the hidden quiet", entries[1])
	}

	// Typing the hidden name expands nothing under either spelling; the
	// visible entry that shares the name still does.
	s.mu.Lock()
	refs := s.refsLocked("/project:quiet\n/quiet\n/user:quiet")
	s.mu.Unlock()
	if len(refs) != 1 || refs[0].target.entry.Plugin != nativeUserID {
		t.Fatalf("refs %v, want the user's quiet alone", nativeRefNames(refs))
	}

	// Snapshot is a clone: what a consumer does to its rows cannot reach the
	// lookup a later prompt is resolved against.
	snap.Plugins[0] = PluginCommand{Display: "tampered"}
	if again := s.Snapshot(); again.Plugins[0].Display != "loud" {
		t.Fatalf("a consumer's edit reached the session: %+v", again.Plugins[0])
	}
}

// TestNativeIgnoresPluginDirs is A14: --plugin-dir is cursor-agent's flag by
// another route, native reads no directory but its own three, and a session
// handed one says so rather than starting with a menu quietly missing what
// was asked for.
func TestNativeIgnoresPluginDirs(t *testing.T) {
	f := newNativeFixture(t)
	dir := writeTree(t, filepath.Join(t.TempDir(), "extra"), map[string]string{
		"commands/extra.md": commandFile("from a --plugin-dir", "extra body"),
	})
	s := startContent(t, f, Options{PluginDirs: []string{dir}},
		map[string]string{".claude/commands/own.md": commandFile("the project's own", "own body")}, nil)

	wantDisplays(t, s.Snapshot().Plugins, "own")
	if !strings.Contains(s.diag.String(), "plugin dirs ignored for native") {
		t.Fatalf("diagnostics %q do not say the dirs were ignored", s.diag.String())
	}
}

// TestNativeProviderHasNoSkillScan pins §3.2's last clause. Everything native
// offers is a PluginEntry and therefore expandable; a non-zero SkillScan would
// have the TUI's rescanSkills add a second row for the same file, labelled
// (skill) and expanding nothing.
func TestNativeProviderHasNoSkillScan(t *testing.T) {
	scan := NativeProvider().SkillScan()
	if len(scan.RelRoots) != 0 || scan.SkipCursorPlugins || scan.NameFromDir {
		t.Fatalf("the native provider advertises a skill scan: %+v", scan)
	}
}

// TestNativeExpandsIntoTheTurn is A6 end to end, and §3.3's deliberate split:
// what the model is sent is the draft plus the block, what the session is
// called and what the journal records is the text the user typed.
func TestNativeExpandsIntoTheTurn(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	f.models["test/a"].push(answer("done"))
	s := startContent(t, f, Options{JournalDir: dir},
		map[string]string{".claude/commands/ship.md": commandFile("ship it", "Ship $ARGUMENTS from ${CLAUDE_PLUGIN_ROOT}.")}, nil)
	w := journalOf(t, s.log)

	if _, err := s.Prompt(context.Background(), "/ship v2"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drained(s)
	if err := endings(t, evs, "end_turn"); err != nil {
		t.Fatalf("the turn failed: %v", err)
	}

	// One EventCommand, naming the row and carrying exactly what was sent.
	cmds := ofType(evs, EventCommand)
	if len(cmds) != 1 || cmds[0].Command == nil {
		t.Fatalf("%d command events: %+v", len(cmds), evs)
	}
	cmd := cmds[0].Command
	if cmd.Qualified != "project:ship" || cmd.Kind != PluginKindCommand {
		t.Fatalf("the command event names %+v", cmd)
	}
	if cmd.Path != filepath.Join(s.ws, ".claude", "commands", "ship.md") {
		t.Fatalf("the command event's path is %q", cmd.Path)
	}

	// The model saw the draft, then the block, with the arguments and the
	// root substituted.
	calls := f.models["test/a"].requests()
	if len(calls) != 1 {
		t.Fatalf("%d requests, want one", len(calls))
	}
	sent := firstUserText(t, calls[0])
	if !strings.HasPrefix(sent, "/ship v2\n\n") {
		t.Fatalf("the draft does not lead the prompt: %q", sent)
	}
	if !strings.Contains(sent, "Ship v2 from "+s.ws+".") {
		t.Fatalf("the prompt does not hold the expanded body: %q", sent)
	}
	if !strings.Contains(sent, `from this project`) {
		t.Fatalf("the block does not name its source: %q", sent)
	}
	if sent != "/ship v2\n\n"+cmd.Text {
		t.Fatalf("what was sent is not the draft and the event's block: %q", sent)
	}

	// The title and the journal's prompt note keep the typed text; only the
	// store records the expansion.
	if title := s.Snapshot().Title; title != "/ship v2" {
		t.Fatalf("title %q, want the typed text", title)
	}
	closeJournaled(t, s, w)
	attempts := journalAttempts(t, assertOneJournal(t, dir, w, s.Incarnation()))
	if len(attempts) != 1 {
		t.Fatalf("%d journal attempts, want one", len(attempts))
	}
	// The event record carries the block — that is what the stream held — but
	// the prompt note is what the user asked for, and it is the typed text.
	if got := jsonString(attempts[0].prompt, "text"); got != "/ship v2" {
		t.Fatalf("the journal's prompt note is %q, want the typed text", got)
	}
	if !containsLine(transcriptOf(t, f), "Ship v2 from") {
		t.Fatalf("the transcript does not hold what was sent:\n%s", strings.Join(transcriptOf(t, f), "\n"))
	}
}

// TestNativeExpandsOneEventPerBlock: two references compose on two lines, in
// the order they were written, and each produces exactly one announcement.
func TestNativeExpandsOneEventPerBlock(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("done"))
	s := startContent(t, f, Options{}, map[string]string{
		".claude/commands/ship.md":        commandFile("ship it", "Ship $ARGUMENTS."),
		".claude/skills/release/SKILL.md": skillDoc("release", "cut a release"),
	}, nil)

	if _, err := s.Prompt(context.Background(), "/ship v2\n/release"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	cmds := ofType(drained(s), EventCommand)
	if len(cmds) != 2 {
		t.Fatalf("%d command events, want one per block", len(cmds))
	}
	if cmds[0].Command.Qualified != "project:ship" || cmds[1].Command.Qualified != "project:release" {
		t.Fatalf("events out of reference order: %q %q", cmds[0].Command.Qualified, cmds[1].Command.Qualified)
	}
	sent := firstUserText(t, f.models["test/a"].requests()[0])
	if strings.Index(sent, cmds[0].Command.Text) > strings.Index(sent, cmds[1].Command.Text) {
		t.Fatalf("the blocks are out of order on the wire: %q", sent)
	}
}

// TestNativeFailedStartLeavesNoRows: the scan runs only once the harness has
// opened, so a session that could not start has no menu either — and the next
// attempt builds one from scratch rather than inheriting a half-started one.
func TestNativeFailedStartLeavesNoRows(t *testing.T) {
	f := newNativeFixture(t)
	base := t.TempDir()
	wsDir := writeTree(t, filepath.Join(base, "ws"), map[string]string{
		".claude/commands/ship.md": commandFile("ship it", "ship body"),
	})
	gitDir(t, wsDir)
	// A model the table does not have: Open is never reached.
	s := f.session(Options{Workspace: wsDir, ContentHome: writeTree(t, filepath.Join(base, "home"), nil), Model: "nope/x"})
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded on a model the table does not have")
	}
	if rows := s.Snapshot().Plugins; len(rows) != 0 {
		t.Fatalf("a session that failed to start has %v in its menu", displays(rows))
	}
}

// TestNativeUnresolvedNameIsSentAsTyped is A4's last clause. Native
// advertises no catalog, so there is nothing a prompt could wait for: an
// unknown /nope goes to the model as it was typed, at once.
func TestNativeUnresolvedNameIsSentAsTyped(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("ok"))
	s := startContent(t, f, Options{},
		map[string]string{".claude/commands/ship.md": commandFile("ship it", "ship body")}, nil)

	out := startPrompt(s, "/nope please")
	if got := await(t, out, "a prompt naming nothing craze knows"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	evs := drained(s)
	if len(ofType(evs, EventCommand)) != 0 {
		t.Fatalf("an unknown name expanded: %+v", evs)
	}
	calls := f.models["test/a"].requests()
	if sent := firstUserText(t, calls[0]); sent != "/nope please" {
		t.Fatalf("the prompt is %q, want the text as typed", sent)
	}
}

// TestNativeInterjectExpands is §3.3's interjection rule: the same text means
// the same thing whether it was merged into a running turn or refused,
// queued, and drained through Begin.
//
// And the other half of that rule, which the expansion nearly took away: what
// goes to the model is the expansion, what goes in the transcript is the four
// words the user typed. The harness echoes an accepted steer back as the text
// it was handed, and the adapter turns that echo into the EventUser row, so
// without the pairing a /ship would put a whole command file where the user's
// own line belongs — the same "wire content is never display content" rule
// §3.6 pins for the composer's shell mode. The control is the second
// interjection, which expands to nothing and must be untouched.
func TestNativeInterjectExpands(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{}, map[string]string{
		".claude/commands/ship.md": commandFile("ship it", "Ship $ARGUMENTS now."),
		"a.txt":                    "alpha\n",
	}, nil)
	h := newHeld(t)
	m := f.models["test/a"]
	m.push(
		h.step(nativeCallParts("c1", "read", nativeArgs(t, map[string]any{"filePath": "a.txt"})),
			finishParts(fantasy.FinishReasonToolCalls)),
		answer("done"),
	)

	out := startPrompt(s, "look around")
	await(t, h.reached, "the held tool step")
	for _, text := range []string{"/ship v3", "and hurry"} {
		if err := s.Interject(context.Background(), text); err != nil {
			t.Fatalf("Interject(%q): %v", text, err)
		}
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}

	calls := m.requests()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want two", len(calls))
	}
	var steered string
	for _, msg := range calls[1].Prompt {
		if text := userTextOf(msg); strings.HasPrefix(text, "/ship v3") {
			steered = text
		}
	}
	if steered == "" {
		t.Fatalf("the interjection never reached the model: %+v", calls[1].Prompt)
	}
	if !strings.Contains(steered, "Ship v3 now.") {
		t.Fatalf("the interjection was not expanded: %q", steered)
	}
	// The rows: the typed line for the expanded one, and the plain one back
	// unchanged. No EventCommand is published for either — the announcement
	// would have no ordering it could hold to against a turn already emitting.
	evs := drained(s)
	texts, _ := interjected(evs)
	if len(texts) != 2 || texts[0] != "/ship v3" || texts[1] != "and hurry" {
		t.Fatalf("interjection rows %q, want exactly what was typed", texts)
	}
	if n := len(ofType(evs, EventCommand)); n != 0 {
		t.Fatalf("%d command events for an interjection, want none", n)
	}
	// And the body really did go out, so the row above is a translation and
	// not an expansion that never happened.
	if strings.Contains(strings.Join(texts, "\n"), "Ship v3 now.") {
		t.Fatalf("a row carries the expansion: %q", texts)
	}
}

// TestNativeUnansweredInterjectionQueuesTheTypedText is the other end of the
// same pairing. An interjection accepted during a turn's final step has no
// later step to take it up, so it comes back in Result.Unanswered — in the
// spelling craze sent, expansion and all — and from there it is an ordinary
// queued row. Three things go wrong if it is queued that way: the row is shown
// as the whole command file, the size cap judges it by that length, and the
// drain hands it to Begin, where the /ship still at the start of its first
// line is expanded a second time. The last is the one the model would notice.
//
// The control is the drained turn's request, which must carry the body once.
func TestNativeUnansweredInterjectionQueuesTheTypedText(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{}, map[string]string{
		".claude/commands/ship.md": commandFile("ship it", "Ship $ARGUMENTS now."),
	}, nil)
	h := newHeld(t)
	m := f.models["test/a"]
	// One step, and it is the turn's last: an interjection taken up here is
	// accepted and unanswerable.
	m.push(h.step(textParts("all done"), finishParts(fantasy.FinishReasonStop)))

	out := startPrompt(s, "go")
	await(t, h.reached, "the held final step")
	if err := s.Interject(context.Background(), "/ship v3"); err != nil {
		t.Fatalf("Interject: %v", err)
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	drained(s)

	q := queueTexts(s)
	if len(q) != 1 || q[0] != "/ship v3" {
		t.Fatalf("the queue holds %q, want exactly the typed text", q)
	}

	// Drained as the TUI drains it: taken off the queue and sent through the
	// ordinary prompt path, which expands it.
	p, ok := s.PopQueue()
	if !ok {
		t.Fatal("the queued row could not be taken")
	}
	m.push(answer("shipped"))
	if got := await(t, startPrompt(s, p.Text), "the drained row"); got.err != nil {
		t.Fatalf("the drained prompt: %v", got.err)
	}

	calls := m.requests()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want the turn and the drained row", len(calls))
	}
	// The last user message of that request, not the first: the drained row
	// opens its turn behind the history the first one left.
	texts := userTexts(calls[1])
	sent := texts[len(texts)-1]
	if n := strings.Count(sent, "Ship v3 now."); n != 1 {
		t.Fatalf("the drained row carried the body %d times, want once:\n%s", n, sent)
	}
	if n := len(ofType(drained(s), EventCommand)); n != 1 {
		t.Fatalf("%d command events for the drained row, want one", n)
	}
}

// TestNativeCancelDuringACommandEventReturns is A7, and the whole reason the
// announcements go out under the turn's own context rather than on emit's
// background one. With a consumer that has stopped draining, the EventCommand
// blocks on the primary; Esc then cancels a prompt whose bytes have not left,
// and the prompt has to come back — with one cancelled ending and no second
// one — rather than wait for a reader that is not coming.
func TestNativeCancelDuringACommandEventReturns(t *testing.T) {
	f := newNativeFixture(t)
	base := t.TempDir()
	wsDir := writeTree(t, filepath.Join(base, "ws"), map[string]string{
		".claude/commands/ship.md": commandFile("ship it", "ship body"),
	})
	gitDir(t, wsDir)
	s := f.session(Options{Workspace: wsDir, ContentHome: writeTree(t, filepath.Join(base, "home"), nil)})
	// Set before Start, so nothing is publishing while the field is written.
	// The fill takes seqs 1..primaryCap and Start publishes nothing, so the
	// expansion's event is the next number.
	hook, inside := insideAt(primaryCap + 1)
	abandoned := make(chan struct{})
	var once sync.Once
	s.log.hooks = &logHooks{
		beforePrimarySend: hook,
		// The fill's publishes all go through, so the first event given up is
		// this test's: it is what says the cancel reached the blocked
		// publisher, and the drain below must not start before it.
		abandoning: func(bool) { once.Do(func() { close(abandoned) }) },
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fillPrimary(t, s.log)
	// No step queued: the model fails the moment it is asked, so the turn's
	// outcome is decided by the cancelled context and not by a scripted
	// answer that happened to stream anyway.

	out := startPrompt(s, "/ship")
	await(t, inside, "the command event reaching the primary send")
	cancelled := make(chan error, 1)
	go func() { cancelled <- s.Cancel(context.Background()) }()
	await(t, abandoned, "the command event being given up")

	// From here the test is the consumer again: the turn's ending still has
	// to reach a primary the fill left full.
	var evs []Event
	var got outcome
	for pending := true; pending; {
		select {
		case ev := <-s.Events():
			evs = append(evs, ev)
		case got = <-out:
			pending = false
		case <-time.After(nativeWait):
			t.Fatalf("the prompt did not return within %v", nativeWait)
		}
	}
	evs = append(evs, drained(s)...)

	if got.err != nil || got.res.StopReason != harness.StopCancelled {
		t.Fatalf("Prompt = %+v, %v; want a cancelled turn", got.res, got.err)
	}
	if err := endings(t, evs, harness.StopCancelled); err != nil {
		t.Fatalf("the cancelled turn also failed: %v", err)
	}
	// The announcement was abandoned, not delivered late: it never took a
	// sequence number, so nothing downstream has a hole to reason about.
	if n := len(ofType(evs, EventCommand)); n != 0 {
		t.Fatalf("%d command events survived the cancel", n)
	}
	if err := await(t, cancelled, "Cancel"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

// TestNativeCommandBodyKeyIsRedacted is A8's Go half. Run persists and sends
// the user's message unchanged, so a provider key inside a command file would
// reach the wire, the transcript, the journal and --json at once — the
// adapter redacts the block before any of them sees it.
func TestNativeCommandBodyKeyIsRedacted(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	f.models["test/a"].push(answer("ok"))
	s := startContent(t, f, Options{JournalDir: dir},
		map[string]string{
			".claude/commands/leaky.md": commandFile("holds a key", "export NATIVE_TEST_KEY="+nativeCanary),
		}, nil)
	w := journalOf(t, s.log)

	if _, err := s.Prompt(context.Background(), "/leaky"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drained(s)
	if len(ofType(evs, EventCommand)) != 1 {
		t.Fatalf("%d command events, want one", len(ofType(evs, EventCommand)))
	}
	// Sanity: the block really was sent, so its cleanliness below is the
	// redactor's doing and not an expansion that never happened.
	calls := f.models["test/a"].requests()
	sent := firstUserText(t, calls[0])
	if !strings.Contains(sent, "export NATIVE_TEST_KEY=") {
		t.Fatalf("the command body never reached the wire: %q", sent)
	}
	// The wire, and the events a headless caller prints as --json.
	for what, v := range map[string]any{"the wire": calls, "the events": evs} {
		if leaks := nativeLeaks(v, nativeCanary); len(leaks) > 0 {
			t.Fatalf("the key leaked into %s at %v", what, leaks)
		}
	}
	for _, line := range transcriptOf(t, f) {
		if strings.Contains(line, nativeCanary) {
			t.Fatalf("the key leaked into the transcript: %q", line)
		}
	}
	closeJournaled(t, s, w)
	assertOneJournal(t, dir, w, s.Incarnation())
	if written := journalBytes(t, w); strings.Contains(written, nativeCanary) {
		t.Fatalf("the key leaked into the journal:\n%s", written)
	}
}

// TestNativeSkillDescriptionKeyIsRedacted is A8's catalog half (round-2
// review). A SKILL.md with no frontmatter description is given one from its
// body — the first heading, else the first line (skillHeadingDescription) — so
// a key written there travels by a road the block's own redaction never
// touches: PluginCommand.Description, which is Snapshot.Plugins, the rows the
// slash menu draws from it (internal/tui/slash.go), and the EventCommand
// record the lossless codec writes to the journal and a headless caller prints
// as --json. Start redacts the entries before any of them is built.
func TestNativeSkillDescriptionKeyIsRedacted(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	f.models["test/a"].push(answer("ok"))
	// No description in the frontmatter, and a heading that is a key.
	skill := "---\nname: leaky\n---\n# export NATIVE_TEST_KEY=" + nativeCanary + "\n\nrun it\n"
	s := startContent(t, f, Options{JournalDir: dir},
		map[string]string{".claude/skills/leaky/SKILL.md": skill}, nil)
	w := journalOf(t, s.log)

	rows := s.Snapshot().Plugins
	wantDisplays(t, rows, "leaky")
	// Sanity: the heading really did become the description, so its
	// cleanliness below is the redactor's doing and not a description that
	// never arrived.
	if !strings.Contains(rows[0].Description, "export NATIVE_TEST_KEY=") {
		t.Fatalf("the body's heading never became the description: %q", rows[0].Description)
	}
	if strings.Contains(rows[0].Description, nativeCanary) {
		t.Fatalf("the key is in the menu's own row: %q", rows[0].Description)
	}

	if _, err := s.Prompt(context.Background(), "/leaky"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drained(s)
	cmds := ofType(evs, EventCommand)
	if len(cmds) != 1 {
		t.Fatalf("%d command events, want one", len(cmds))
	}
	if strings.Contains(cmds[0].Command.Description, nativeCanary) {
		t.Fatalf("the key is in the expansion's record: %+v", cmds[0].Command.PluginCommand)
	}
	for what, v := range map[string]any{
		"the wire":     f.models["test/a"].requests(),
		"the events":   evs,
		"the snapshot": s.Snapshot(),
	} {
		if leaks := nativeLeaks(v, nativeCanary); len(leaks) > 0 {
			t.Fatalf("the key leaked into %s at %v", what, leaks)
		}
	}
	closeJournaled(t, s, w)
	assertOneJournal(t, dir, w, s.Incarnation())
	if written := journalBytes(t, w); strings.Contains(written, nativeCanary) {
		t.Fatalf("the key leaked into the journal:\n%s", written)
	}
}

// TestNativeCommandSpliceIsNotExecuted is A6's last clause through a real
// turn: a body holding !`cmd` reaches the wire byte for byte, and craze runs
// nothing — the file the command names is never written.
func TestNativeCommandSpliceIsNotExecuted(t *testing.T) {
	f := newNativeFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	body := "Branch: !`touch " + marker + "`\nThen act."
	f.models["test/a"].push(answer("ok"))
	s := startContent(t, f, Options{},
		map[string]string{".claude/commands/branch.md": commandFile("splices a command", body)}, nil)

	if _, err := s.Prompt(context.Background(), "/branch"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	sent := firstUserText(t, f.models["test/a"].requests()[0])
	if !strings.Contains(sent, body) {
		t.Fatalf("the splice did not reach the wire byte for byte: %q", sent)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("craze executed the splice")
	}
}
