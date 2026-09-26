package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/journal"
)

// Personas (plan 026 §3.4): the sub-agent types a native session's agent tool
// can start, beyond the harness's own three built-ins. They are read from
// three places — the workspace chain's .claude/agents, the user root's
// agents, and every installed-and-enabled plugin's agents — which are data on
// contentSources like every other location (D-45), by the same scan that
// reads the commands and skills, under the same reading rules: a symlink where
// a directory or a file should be is not followed, a file past the ceiling is
// not read, a plugin whose id is reserved or unusable is not read at all.
//
// And nothing else of that scan's. A persona is not a menu row or a catalog
// row, so it is kept out of both by construction: a list of its own
// (pluginDiscovery.agents, never out), a name set of its own ("agent:<name>"),
// so a command, a skill and a persona of one name all stay, and a budget of
// its own, so the persona files installed on a machine cannot crowd out a later
// plugin's skills (panel astra 9, CodeRabbit 2). What the menu, the name
// resolver and the frozen prompt see is therefore exactly what they saw before
// personas existed.
//
// The adapter owns Claude's vocabulary under that seam (panel CodeRabbit 11):
// it reads the files, maps their tool names to native ids
// (tool.MapClaudeTools, tool.MapClaudeDisallowed), drops a user persona that
// would shadow a built-in, and says what it could not use on its diagnostics
// lane. The harness gets data (harness.Options.Personas) and owns the rest:
// precedence against its built-ins, the agent tool's list, and what a type
// gives a child.

// The persona budget (plan 026 §3.4): files and bytes read across all three
// sources of one scan, counted apart from the skills' and commands'
// (maxPluginFiles). The machine this was planned on has 44 persona files, all
// from plugins, so 128 leaves room for a project's and a user's own.
const (
	maxAgentFiles = 128
	maxAgentBytes = 1 << 20
	// maxPersonaRole bounds a persona's body, which is its role: the text the
	// harness freezes into a child's system prompt under its own 32 KiB budget
	// (renderChildRole). Holding more than that for every persona of a
	// session would be memory spent on text no child is ever sent.
	maxPersonaRole = 32 << 10
)

// diagSubagentWarning is the journal diag a harness Options.Warn line is
// written as: a persona's or the configured default's model or effort that did
// not resolve, so a sub-agent call moved on to the next candidate rather than
// failing (plan 026 §3.6). Its one field, "text", is the line, redacted. The
// kind is the adapter's own word; the journal does not interpret kinds.
const diagSubagentWarning = "subagent_warning"

// diagResumeWarning is the journal diag a harness Options.Warn line from Open
// is written as: a resumed session that could not continue on its
// transcript's model and moved on to another, or whose last mode this craze
// does not know (plan 028 §3.3). Its one field, "text", is the line, redacted,
// and the same line goes to the session's diagnostics (note), where the user
// reads native's other startup warnings.
const diagResumeWarning = "resume_warning"

// agentEntry is one persona file as the scan read it, before any mapping: its
// tool lists are as the file wrote them. Kind is PluginKindAgent, always; it is
// a type of its own rather than a PluginEntry so that it cannot land in the
// entry list by accident — the separate namespace is a type error to break.
type agentEntry struct {
	// Plugin is nativeProjectID, nativeUserID or the plugin's id, and Root the
	// chain directory, the user root or the plugin's install, as for an entry.
	Plugin, Root string
	// Name is the frontmatter name, else the file's base name without .md:
	// bare for a project's or a user's, qualified plugin:name for a plugin's
	// only when handed on (personaName).
	Name        string
	Description string
	// Role is the body, trimmed and within maxPersonaRole.
	Role string
	Path string
	Kind string
	// Model and Effort are the frontmatter's, "" when absent.
	Model, Effort string
	// HasTools says the frontmatter has a tools key at all, whatever it holds:
	// only its absence gives a child every tool (plan 026 §3.4's fail-closed
	// rule). Tools and Disallowed are the two lists' names as written.
	HasTools          bool
	Tools, Disallowed []string
}

// personaName is the name a persona is offered under: bare for the project's
// and the user's, plugin:name for a plugin's. A bare name can never look
// qualified, since the name passed pluginNameOK and a colon is not in its
// class, so a project file cannot pass itself off as a plugin's persona.
func (e agentEntry) personaName() string {
	if nativePseudoID(e.Plugin) {
		return e.Name
	}
	return e.Plugin + ":" + e.Name
}

// personaScope is the harness's word for where a persona came from, which
// decides its precedence there.
func personaScope(plugin string) string {
	switch strings.ToLower(plugin) {
	case nativeProjectID:
		return tool.PersonaProject
	case nativeUserID:
		return tool.PersonaUser
	}
	return tool.PersonaPlugin
}

// personaKey is what the persona set deduplicates by: "agent:" and the name it
// is offered under, keyed as the harness keys agent types
// (harness.AgentTypeKey), so two names the model could not tell apart are one
// persona here too, the first read winning. For the ASCII class names are
// held to this is lowercasing by another route; it is the harness's own key so
// the two cannot drift if that class ever widens.
func personaKey(name string) string { return "agent:" + harness.AgentTypeKey(name) }

// readAgentDir reads one persona directory — agents/*.md, flat, in lexical
// order — under the id and root its entries are filed by. dir is "" when there
// is nothing to read: the directory is absent, is a symlink (pluginSubdir,
// nativeSubdir refuse one, as for commands), or the layout names none. A
// symlinked or non-regular file inside it is skipped by pluginMarkdownFiles
// and readAgentFile, the rule every walk feeding this scan holds to.
func (d *pluginDiscovery) readAgentDir(dir, id, root string) {
	if dir == "" {
		return
	}
	for _, name := range pluginMarkdownFiles(dir) {
		path := filepath.Join(dir, name)
		data, ok := d.readAgentFile(path)
		if !ok {
			continue
		}
		if e, parsed := parseAgentFile(path, data, d.parse); parsed {
			e.Plugin, e.Root = id, root
			d.addAgent(e)
		}
	}
}

// readAgentFile is readFile against the persona budget instead of the
// entries': maxAgentFiles files and maxAgentBytes bytes, and an os.SameFile
// list of its own, so one persona file reached twice — a home that is also the
// workspace — is read once, by the first source to reach it. A file past the
// per-file ceiling, or one that would take the scan past maxAgentBytes, is not
// read and costs nothing; a later, smaller one still can be.
func (d *pluginDiscovery) readAgentFile(path string) ([]byte, bool) {
	if d.nAgentFiles >= maxAgentFiles {
		return nil, false
	}
	// Lstat, as readFile: a symlinked persona file is not read at all.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPluginBytes || d.agentBytes+info.Size() > maxAgentBytes {
		return nil, false
	}
	for _, seen := range d.agentFiles {
		if os.SameFile(seen, info) {
			return nil, false
		}
	}
	d.nAgentFiles++
	data, ok := readCappedFile(path)
	// Checked again against what was read: the file can grow in between.
	if !ok || d.agentBytes+int64(len(data)) > maxAgentBytes {
		return nil, false
	}
	d.agentBytes += int64(len(data))
	d.agentFiles = append(d.agentFiles, info)
	return data, true
}

// addAgent adds e to the scan's persona list, in the order its three sources
// are read: the chain innermost first, then the user root, then the plugins.
//
// Its dedupe is scoped to one source (review r5, finding 5): the key is
// e.Plugin (nativeProjectID, nativeUserID, or the plugin's own id) plus
// personaKey, so two files that parse to the same name within the same
// source still collapse to the first, in that source's own read order — a
// project chain's subdirectory beating its root, say (TestNativePersonaSources,
// "the chain is innermost first"). A name repeated *across* sources now
// reaches nativePersonas as more than one entry, because deciding between
// them here was the bug: first wins on personaKey alone claimed a name for
// whichever source the scan reached first, before nativePersonas's key gate
// had any say, so a project persona whose path held a configured key still
// claimed the name at scan time and only got dropped afterwards — by which
// point a valid user persona of that same name had already lost the claim
// and was gone from the list entirely. Cross-source precedence is decided
// once, in nativePersonas, over every persona that survives the key gate.
func (d *pluginDiscovery) addAgent(e agentEntry) {
	key := e.Plugin + ":" + personaKey(e.personaName())
	if d.agentSeen == nil {
		d.agentSeen = make(map[string]struct{})
	}
	if _, ok := d.agentSeen[key]; ok {
		return
	}
	d.agentSeen[key] = struct{}{}
	d.agents = append(d.agents, e)
}

// parseAgentFile reads a persona file: the name and description as a command's
// are read (parseFrontmatterLines, unchanged, so a persona and a command
// agree on what those two keys say), the agent-only keys by their own reader
// (parseAgentFrontmatter), and the body as the role. A name craze could not
// offer as one token is refused with the scan's one line, as a command's is.
// A body past maxPersonaRole is cut at a line boundary, with a line: the role
// is the persona, and a child working from part of it should not be a
// surprise.
func parseAgentFile(path string, data []byte, opts pluginParseOpts) (agentEntry, bool) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	fm, body, _, err := splitFrontmatter(text)
	if err != nil {
		return agentEntry{}, false
	}
	meta := parseFrontmatterLines(fm)
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		base := filepath.Base(path)
		name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if !pluginNameOK(name) {
		opts.badName(path, name)
		return agentEntry{}, false
	}
	role := strings.TrimSpace(body)
	if len(role) > maxPersonaRole {
		role = cutRole(role)
		opts.warnf("persona %q in %q: its body is over %d bytes; only the first %d are its role", name, path, maxPersonaRole, len(role))
	}
	am := parseAgentFrontmatter(fm)
	return agentEntry{
		Name:        name,
		Description: sanitizeText(strings.TrimSpace(meta.Description)),
		Role:        role,
		Path:        path,
		Kind:        PluginKindAgent,
		Model:       am.Model,
		Effort:      am.Effort,
		HasTools:    am.HasTools,
		Tools:       am.Tools,
		Disallowed:  am.Disallowed,
	}, true
}

// cutRole is a role over maxPersonaRole cut to whole lines within it, or at a
// rune boundary when its first line alone is longer: the rule the harness's
// own budget cuts by (cutToBudget), without a marker, since the harness adds
// none to a role that fits.
func cutRole(role string) string {
	cut := strings.LastIndexByte(role[:maxPersonaRole], '\n')
	if cut <= 0 {
		for cut = maxPersonaRole; cut > 0 && !utf8.RuneStart(role[cut]); cut-- {
		}
	}
	return strings.TrimRight(role[:cut], " \t\r\n")
}

// agentFrontmatter is what a persona file says beyond a command's two keys
// (plan 026 §3.4, panel CodeRabbit 3). color, skills, initialPrompt,
// permissionMode, maxTurns, isolation, background, hooks and mcpServers are
// not fields: each is ignored without a word, as every key craze does not read
// is.
type agentFrontmatter struct {
	// HasTools is a tools key being there at all. A present key whose value
	// reads as nothing is a list of nothing — a text-only child — never "every
	// tool", which only its absence means.
	HasTools bool
	// Tools and Disallowed are the two lists' names as written, in order.
	// disallowedTools and disallowed-tools are both read, and both are
	// subtracted when a file has both: a deny list read twice denies more.
	Tools, Disallowed []string
	Model, Effort     string
}

// parseAgentFrontmatter reads the agent-only keys off a frontmatter block. It
// is the scalar reader's loop (parseFrontmatterLines: top-level keys only, a
// comment line skipped, keys case-insensitive) run a second time over the same
// block with its own keys, because the two lists take a shape the shared reader
// deliberately skips — a block list of indented "- Read" lines — and extending
// the shared reader would change what every skill and command is read as.
// Only these four keys are read here; the shared reader still reads the rest.
//
// A tools key given twice takes the last, as the shared reader does for its own
// keys.
func parseAgentFrontmatter(fm string) agentFrontmatter {
	var out agentFrontmatter
	lines := strings.Split(fm, "\n")
	for i := 0; i < len(lines); i++ {
		raw := strings.TrimSuffix(lines[i], "\r")
		if !topLevel(raw) {
			continue
		}
		line := trimLine(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(unquoteScalar(strings.TrimSpace(key))) {
		case "tools":
			names, used := agentToolList(val, lines[i+1:])
			out.HasTools, out.Tools = true, names
			i += used
		case "disallowedtools", "disallowed-tools":
			names, used := agentToolList(val, lines[i+1:])
			out.Disallowed = append(out.Disallowed, names...)
			i += used
		case "model":
			out.Model = strings.TrimSpace(unquoteScalar(cutComment(val)))
		case "effort":
			out.Effort = strings.TrimSpace(unquoteScalar(cutComment(val)))
		}
	}
	return out
}

// agentToolList reads a tools or disallowedTools value in any of the three
// syntaxes found in installed persona files (plan 026 §3.4):
//
//   - a comma string, quoted or not: `tools: Read, Glob`;
//   - a flow sequence, which may run on over the lines below it until its
//     bracket closes: `tools: ["Read", "Grep"]`;
//   - a block list, nothing after the key and one "- Read" line per name
//     below it, indented or not.
//
// val is what followed the key's colon and rest the frontmatter's lines after
// the key; used is how many of those the value took. The split respects
// parentheses and quotes, since Agent(a, b, c) is one name with commas inside.
// A value that does not read — a flow sequence that never closes, an indented
// line that is no list item — contributes nothing, which the caller reads as
// a list of nothing: it fails closed.
func agentToolList(val string, rest []string) (names []string, used int) {
	v := cutComment(val)
	switch {
	case strings.HasPrefix(v, "["):
		return flowList(v, rest)
	case v == "":
		return blockList(rest)
	default:
		return splitAgentList(unquoteScalar(v)), 0
	}
}

// flowList is a flow sequence whose first line is first. It takes the lines
// below until the bracket closes; a line back at the top level ends it unless
// it is the closing bracket itself, so an unclosed sequence cannot swallow the
// keys after it. Unclosed, it is nothing.
func flowList(first string, rest []string) (names []string, used int) {
	text := first
	for flowEnd(text) < 0 && used < len(rest) {
		raw := strings.TrimSuffix(rest[used], "\r")
		line := trimLine(raw)
		if topLevel(raw) && line != "" && !strings.HasPrefix(line, "]") {
			break
		}
		used++
		text += " " + cutComment(line)
	}
	end := flowEnd(text)
	if end < 0 {
		return nil, used
	}
	return splitAgentList(text[1:end]), used
}

// flowEnd is the index of the bracket that closes text's opening one, outside
// quotes, or -1.
func flowEnd(text string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// blockList is the "- name" lines below a key with no value. Blank lines and
// comments are passed over, an indented line that is no item is taken and
// contributes nothing, and the first top-level line that is not an item ends
// the list — the next key.
func blockList(rest []string) (names []string, used int) {
	for ; used < len(rest); used++ {
		raw := strings.TrimSuffix(rest[used], "\r")
		line := trimLine(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == "-" || strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "-\t"):
			if item := strings.TrimSpace(unquoteScalar(cutComment(line[1:]))); item != "" {
				names = append(names, item)
			}
		case topLevel(raw):
			return names, used
		}
	}
	return names, used
}

// splitAgentList splits a list's text at the commas outside parentheses and
// quotes, each name trimmed and unquoted; an empty one is dropped.
func splitAgentList(s string) []string {
	var out []string
	start, depth := 0, 0
	var quote byte
	add := func(end int) {
		if item := strings.TrimSpace(unquoteScalar(strings.TrimSpace(s[start:end]))); item != "" {
			out = append(out, item)
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			if depth > 0 {
				depth--
			}
		case c == ',' && depth == 0:
			add(i)
			start = i + 1
		}
	}
	add(len(s))
	return out
}

// cutComment is s trimmed, without a YAML comment: a "#" at its start or after
// a space or a tab, outside quotes. A plain scalar ends there, so
// `tools: Read, Grep  # read-only` is two names, not a third called
// "Grep  # read-only".
func cutComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// nativePersonas is the scan's persona list as the harness takes it
// (harness.Options.Personas): each one named, scoped, and with its tool lists
// mapped to native ids (plan 026 §3.4). This is where the adapter's half of the
// rules runs, at discovery time, because this is where the diagnostics lane
// is — harness.Open has none (panel CodeRabbit 11):
//
//   - An identity holding a provider key drops the persona whole, as it drops
//     an entry (dropKeyBearing): its name is what the model sends back, and a
//     path holding a key is one a child's header would carry.
//   - A user persona named like a built-in is skipped with a line: the
//     built-in outranks it (project > built-in > user > plugin), so it could
//     never be started, and saying so beats a persona that silently does not
//     exist. A project persona of that name shadows the built-in by design;
//     a plugin's is qualified and cannot collide.
//   - tools maps through tool.MapClaudeTools and disallowedTools through
//     tool.MapClaudeDisallowed, with one line per persona naming what neither
//     could map. A tools key that maps to nothing — empty, unreadable, or
//     every name dropped — is a text-only child, with a line: it fails closed.
//     Only a file with no tools key gets every tool (AllTools).
//   - Same-name precedence is decided here too, on personaKey, first (surviving)
//     wins in agents's scan order (review r5, finding 5): a candidate claims
//     its name only once it has cleared every gate above, so a project
//     persona dropped for holding a configured key can never suppress a
//     valid user persona of that same name the way it could when addAgent
//     claimed the name at scan time, before either gate had run.
//
// warn is the scan's lane (contentWarn), which redacts. The result is in the
// scan's order, and nil when empty.
func nativePersonas(agents []agentEntry, keys []string, warn func(string)) []harness.Persona {
	if warn == nil {
		warn = func(string) {}
	}
	builtins := harness.BuiltinAgentTypes()
	seen := make(map[string]struct{})
	var out []harness.Persona
	for _, a := range agents {
		name := a.personaName()
		if keyed(keys, a.Plugin, a.Name, a.Path, a.Root, name) {
			warn(fmt.Sprintf("persona %q not offered: its name, its plugin id, its path or its root contains a configured provider key", name))
			continue
		}
		scope := personaScope(a.Plugin)
		if scope == tool.PersonaUser {
			if b, shadows := builtinNamed(builtins, name); shadows {
				warn(fmt.Sprintf("persona %q in %q skipped: the built-in agent type %q has that name and comes first", name, a.Path, b))
				continue
			}
		}
		key := personaKey(name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		p := harness.Persona{
			Name:        name,
			Description: a.Description,
			Model:       a.Model,
			Effort:      a.Effort,
			Role:        a.Role,
			Path:        a.Path,
			Scope:       scope,
			AllTools:    !a.HasTools,
		}
		var unknownTools, unknownDenied []string
		if a.HasTools {
			p.Tools, unknownTools = tool.MapClaudeTools(a.Tools)
		}
		p.DisallowedTools, unknownDenied = tool.MapClaudeDisallowed(a.Disallowed)
		if line := unmappedLine(unknownTools, unknownDenied); line != "" {
			warn(fmt.Sprintf("persona %q in %q: %s", name, a.Path, line))
		}
		if a.HasTools && len(p.Tools) == 0 {
			warn(fmt.Sprintf("persona %q in %q: its tools key names no tool craze can give it, so it runs with no tools", name, a.Path))
		}
		out = append(out, p)
	}
	return out
}

// keyed reports whether any of fields holds one of keys.
func keyed(keys []string, fields ...string) bool {
	for _, f := range fields {
		if holdsNativeKey(f, keys) {
			return true
		}
	}
	return false
}

// builtinNamed is the built-in agent type name is spelled like, compared by
// case folding in strings.EqualFold's sense — the harness's own comparison, so
// Claude Code's Explore is the built-in explore here as it is there.
func builtinNamed(builtins []string, name string) (string, bool) {
	for _, b := range builtins {
		if strings.EqualFold(b, name) {
			return b, true
		}
	}
	return "", false
}

// unmappedLine is the one sentence about the names a persona's two lists held
// that craze could not map, "" when there were none. Each name is quoted: it
// is the file's text.
func unmappedLine(tools, denied []string) string {
	var parts []string
	for _, l := range []struct {
		key   string
		names []string
	}{{"tools", tools}, {"disallowedTools", denied}} {
		if len(l.names) == 0 {
			continue
		}
		quoted := make([]string, len(l.names))
		for i, n := range l.names {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		parts = append(parts, l.key+" "+strings.Join(quoted, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "tool names craze cannot map were left out: " + strings.Join(parts, "; ")
}

// warnf writes one line through Warn, when there is one; native's alone, like
// badName.
func (o pluginParseOpts) warnf(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(fmt.Sprintf(format, args...))
	}
}

// nativeModelMatcher is harness.Options.MatchModel for table: a sub-agent's
// model named the way --model names one (MatchModel's normalisation — case,
// spaces, a display name), so the model's "Model A" and the flag's reach the
// same alias (plan 026 §3.6). The list is built once; the table is the
// session's and never changes.
func nativeModelMatcher(table *modeltable.Table) func(string) (string, bool) {
	snap := Snapshot{Models: tableModels(table)}
	return func(raw string) (string, bool) {
		alias, err := MatchModel(snap, raw)
		return alias, err == nil
	}
}

// harnessWarn is harness.Options.Warn: a fall-through the harness reports
// rather than fails on, journaled. Which one it is follows from when it
// arrives. Before Open has returned (opened is still empty) it is Open's own,
// a resumed session's (plan 028 §3.3): one diagResumeWarning note, and the
// line on the session's diagnostics too, since it changes what the session
// runs on and the user would otherwise learn it only from the model label.
// After, it is a sub-agent call's, from inside a turn: one diagSubagentWarning
// note (plan 026 §3.4, §3.6).
//
// The line quotes content — a persona's model, as its file wrote it, or a
// transcript's model and mode, off disk — so it takes the discipline every
// such string does, redact, sanitize, redact (nativeSafe.line), before it is
// journaled or shown: a key split by a zero-width space passes the first
// redaction whole and is put back together by the sanitizer, so only the
// redaction after it can catch it (astra r1-c3 F4). The redactor is red, the
// keys this session started with, and once the harness is open its own as
// well, which also covers a key the session learned after Open
// (Session.Redact). The journal and the diagnostics carry the one line, so
// neither can hold what the other does not. opened is where open() puts the
// session when Open returns. It is called with no lock of the adapter's held
// — on Start's goroutine inside Open, or on the agent call's — and Note never
// blocks.
func (s *nativeSession) harnessWarn(red *redact.Replacer, opened *atomic.Pointer[harness.Session]) func(string) {
	return func(msg string) {
		hs := opened.Load()
		if hs == nil {
			text := nativeSafe{red: red.String}.line(msg)
			s.log.Note(journal.DiagNote{Kind: diagResumeWarning, Fields: map[string]any{"text": text}})
			s.note(text)
			return
		}
		text := nativeSafe{red: func(v string) string { return hs.Redact(red.String(v)) }}.line(msg)
		s.log.Note(journal.DiagNote{Kind: diagSubagentWarning, Fields: map[string]any{"text": text}})
	}
}
