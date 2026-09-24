package harness

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// systemPrompt builds the first half of a session's system prompt: the
// session's tool profile's (plan 019 §3.6), from its workspace and the
// operating system (runtime.GOOS). The profile writes the text for its own
// tools — the opencode profile's is opencode's default prompt reduced to what
// is true of craze, with H1's environment block — and the session fixes it
// when it opens, as it fixes the profile.
//
// The second half, from H4, is the caller's own content: the instruction
// documents craze read for this workspace and the catalog of the skills and
// commands it found, rendered after the profile's text by withPromptExtras
// (plan 022 §3.4). The harness reads no file for either; both arrive as data
// in Options.Prompt, resolved once, before Open.
//
// A sub-agent's prompt has a third part, its role, rendered after both with a
// budget of its own (child.go, plan 026 §3.2). A parent's prompt never has
// one, so nothing here changes for it.
//
// Open calls both once and the session sends the result, unchanged, with
// every request: it holds no clock, no git state, and nothing else that
// changes between requests, so every request in a session starts with the
// same bytes and a provider's prefix cache can hit (plan 018 §3.7, owner
// decision 6, D-30). The profile's text is a byte-identical prefix whether
// there are extras or not, and with none the prompt is the profile's text
// alone. Changing a byte of it changes every session's prompt-cache prefix
// and the hash in new transcripts' headers; testdata/system_prompt.golden
// pins the opencode profile's and testdata/system_prompt_extras.golden the
// rendering of the extras after it. It is never stored; the transcript
// header records its SHA-256.
func systemPrompt(p tool.Profile, workspace, goos string) string {
	return p.System(tool.SystemEnv{Workspace: workspace, OS: goos})
}

// PromptExtras is what a caller adds to the system prompt: the instruction
// documents the user and the project wrote, and the catalog of the skills and
// slash commands installed for this workspace (plan 022 §3.4). It is pure
// data: the adapter in internal/agent finds the files and reads them, because
// the harness imports nothing from the rest of craze and opens no file of the
// caller's (D-02). It is rendered once, in Open, so a file that changes
// mid-session is seen at the next session, not this one (R3).
type PromptExtras struct {
	// Instructions are the documents, in the order the model should read
	// them: the user's global file first, then the repository root, then the
	// directories below it, so the deepest file is the last word.
	Instructions []PromptDoc
	// Catalog are the entries, in the order the slash menu lists them.
	Catalog []CatalogRow
}

// PromptDoc is one instruction document: its absolute path, for provenance
// and so the model can go back to the file, and its text with whatever it
// imports already inlined, which is what the per-document budget bounds.
type PromptDoc struct{ Path, Text string }

// CatalogRow is one installed skill or slash command as the prompt lists it.
// Name is the name the user types and the model names; Path is the file that
// holds it, which is the only thing that loads it; Root is the plugin's root,
// which a plugin's own file expands ${CLAUDE_PLUGIN_ROOT} to, and is empty
// for an entry that is not a plugin's.
type CatalogRow struct{ Name, Kind, Description, WhenToUse, Path, Root string }

// The budgets, in bytes of the text that actually goes out (plan 022 §3.4).
// They bound the prompt a workspace can impose on every request of a session:
// the ceiling here is about 30k tokens, cached after the first request, with
// no compaction until H7 (R1).
const (
	// maxInstructionDoc bounds one document's text, imports included — the
	// document arrives inlined, so this is the whole of what that file says —
	// and maxInstructionAll every document's text together. The headings over
	// them are craze's own framing, and no document's budget pays for them.
	maxInstructionDoc = 32 << 10
	maxInstructionAll = 96 << 10
	// maxCatalogRows bounds the rows. The heading and the paragraph above
	// them are craze's own fixed framing and are not the caller's to spend.
	maxCatalogRows = 24 << 10
	// maxRowText bounds a row's description plus its when-to-use. Past it the
	// row keeps its names and its paths, which is what makes it usable at
	// all, and loses the prose, which is what makes it long.
	maxRowText = 400
	// truncatedLine ends text a budget cut, so the model knows it is reading
	// part of a file rather than all of it. The path above it is still there:
	// a model that needs the rest can read the file.
	truncatedLine = "[craze: truncated]\n"
)

// The two headings craze frames the extras with, and the paragraphs under
// them. They are craze's own words in the same channel as the caller's text,
// so a document that writes one of them itself is escaped (escapeFraming):
// what the model must never lose is which words are craze's.
const (
	instructionsHeading  = "# Project and user instructions"
	instructionsPreamble = "The files below are the user's and the project's own instructions, ordered from\n" +
		"the user's global file to the repository root to the current directory. A\n" +
		"deeper file wins where two conflict. Follow them.\n"

	catalogHeading  = "# Skills and commands"
	catalogPreamble = "These skills and slash commands are installed. Each has a file at the absolute\n" +
		"path shown. When one fits the task, or when the user or another instruction\n" +
		"says to run /name, read that file and follow it; nothing else loads it. In\n" +
		"those files $ARGUMENTS is whatever followed the name, ${CLAUDE_PLUGIN_ROOT} is\n" +
		"the Root shown for that entry, and `!` followed by a command in backticks\n" +
		"means: run that command and use its output.\n"
)

// withPromptExtras is the whole system prompt: system, and after it x
// rendered, redacted with red (nil redacts nothing). With nothing to render
// it returns system itself, so a session with no extras sends the profile's
// text byte for byte as it did before H4, and every session's prompt starts
// with those same bytes.
//
// The renderer cannot trust what it is handed. Its caller assembled the text
// out of files a repository controls, so this reads as a parser of hostile
// input: line endings are normalised, every catalog field and every path is
// folded onto one line, a row whose path is not absolute and clean is
// dropped, and a line of a document that would forge craze's own framing is
// escaped. Instruction text is otherwise passed through exactly as written —
// it is the user's own file, and folding it would destroy it.
//
// Redaction happens twice, and both passes are needed.
//
// Per field, inside the renderers, because the budgets must be measured over
// redacted text: a marker is longer than most keys, so a pass that ran only
// at the end could be what pushes a document past a ceiling the renderer had
// already declared it inside (X14).
//
// Then once more here, over the whole assembled string. Every field is
// redacted alone, and the framing written between two of them creates byte
// boundaries that did not exist when the redactor ran: a key whose value is
// "Path: /tmp/keyfile" is in neither the row's path nor craze's own "Path: "
// label, and is in the line they make together. The same holds across Name
// and Kind, across a document's path and the "## From: " above it, across two
// sections, across the profile's text and the extras, and — the one case with
// no boundary at all in the input — across a cut and the truncation marker
// appended to it, where the document never held the whole key. Redacting the
// finished bytes closes all of them at once, and it is safe to run twice:
// redaction is idempotent for any key that does not overlap the marker, which
// is refused before it gets here (redact.MarkerOverlaps, modeltable).
//
// The price is that the budgets are measured before this pass, so a key that
// is genuinely reconstructed across a boundary grows the result a little past
// a ceiling. That is the right way round: a budget is a cost control, while a
// key on the wire is a leak. The case is pathological in any event — a
// workspace holding a key is refused at Open (errWorkspaceKey), and an
// ordinary prompt has no key in it anywhere, so this pass changes not a byte.
//
// # Where redaction is not the answer
//
// Two placements of a key cannot be redacted away, and both refuse the
// session instead (errProfileKey). One is a key that lies wholly inside the
// profile's own words: that text is craze's, not a caller's, so nothing about
// the session could be changed to remove it, and rewriting it would send the
// model a mangled description of its own tools — while returning it unchanged
// is the one path on which a configured key goes out verbatim, since nothing
// downstream looks at the frozen prompt again. The other is a key
// reconstructed across the profile's last bytes and the extras' first: the
// final pass would rewrite part of the profile's text and move with it the
// prefix D-30 exists for, that the profile's text is byte-identical at the
// head of every session's every request and a provider's prefix cache can hit
// on it. That guarantee is worth more unconditional than held until a key
// happens to straddle the join.
//
// One test answers both, because a key inside the profile breaks the prefix
// too: whether the redacted whole still begins with the profile's own bytes.
func withPromptExtras(system string, x PromptExtras, red *redact.Replacer) (string, error) {
	var sections []string
	if s := renderInstructions(x.Instructions, red); s != "" {
		sections = append(sections, s)
	}
	if s := renderCatalog(x.Catalog, red); s != "" {
		sections = append(sections, s)
	}
	if len(sections) == 0 {
		// The profile's text alone — and it is still measured, because a key
		// inside it has no extras to be found across a boundary with and would
		// otherwise be the one text on this path nothing ever looked at.
		if red.String(system) != system {
			return "", errProfileKey
		}
		return system, nil
	}
	// Each section ends in a newline and the profile's text does too, so one
	// newline between them is the blank line that separates two blocks.
	out := red.String(system + "\n" + strings.Join(sections, "\n"))
	if !strings.HasPrefix(out, system) {
		return "", errProfileKey
	}
	return out, nil
}

// renderInstructions is the documents' section, or "" when none of them has
// anything to say.
//
// Each document is normalised, escaped, redacted and only then measured, in
// that order, because every one of those steps can change the length of what
// goes out: redaction in particular grows text, so a budget checked before it
// would not be a budget on the bytes the model sees (X14's rule). What this
// order cannot promise is that no later step joins two fragments of a key
// back together — the cut's marker and craze's own framing are both written
// after it — which is what withPromptExtras' final pass over the assembled
// prompt is for.
func renderInstructions(docs []PromptDoc, red *redact.Replacer) string {
	var b strings.Builder
	left := maxInstructionAll
	for _, d := range docs {
		// A document whose budget would buy nothing but the marker is left
		// out whole: its heading alone would tell the model a file exists
		// without telling it anything the file says.
		if left <= len(truncatedLine) {
			break
		}
		text := red.String(escapeFraming(normalizeLines(d.Text)))
		if strings.TrimSpace(text) == "" {
			continue // an empty file contributes nothing but a heading
		}
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text = cutToBudget(text, min(maxInstructionDoc, left))
		left -= len(text)
		b.WriteString("\n## From:")
		// The path is folded like a catalog row's: it is a heading in craze's
		// own framing, and a newline in it would put the rest of the document
		// under a heading of its own.
		if p := red.String(foldLine(d.Path)); p != "" {
			b.WriteString(" " + p)
		}
		b.WriteString("\n")
		b.WriteString(text)
	}
	if b.Len() == 0 {
		return ""
	}
	return instructionsHeading + "\n\n" + instructionsPreamble + b.String()
}

// renderCatalog is the catalog's section, or "" when no row survives. Rows
// that do not fit are dropped from the end rather than from the middle: the
// caller hands them over in the slash menu's order, which is the order the
// user would find them in, and a prefix of it is still that order.
func renderCatalog(rows []CatalogRow, red *redact.Replacer) string {
	blocks := make([]string, 0, len(rows))
	for _, r := range rows {
		if s := renderRow(r, red); s != "" {
			blocks = append(blocks, s)
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return catalogHeading + "\n\n" + catalogPreamble + "\n" + fitRows(blocks, 0, maxCatalogRows)
}

// renderRow is one row's block of lines, or "" when the row cannot be listed.
//
// A row is a pointer: its whole value to the model is that the name can be
// resolved to a file it can read. So a row with no name, or whose path is not
// an absolute, clean path, is dropped — listing it would offer the model
// something it cannot act on and, with a relative path, something it would
// resolve against a directory nobody chose. Root is a path in the same sense
// (a plugin's file expands ${CLAUDE_PLUGIN_ROOT} to it), so an unusable one
// costs its line, not the row.
func renderRow(r CatalogRow, red *redact.Replacer) string {
	// Folded first, redacted after: folding drops control runes and collapses
	// whitespace, so a key split by either is whole by the time redaction
	// looks for it.
	name := red.String(foldLine(r.Name))
	path := red.String(foldLine(r.Path))
	if name == "" || !absClean(path) {
		return ""
	}
	kind := red.String(foldLine(r.Kind))
	desc := red.String(foldLine(r.Description))
	when := red.String(foldLine(r.WhenToUse))
	if len(desc)+len(when) > maxRowText {
		desc, when = "", ""
	}
	var b strings.Builder
	b.WriteString("- " + name)
	if kind != "" {
		b.WriteString(" (" + kind + ")")
	}
	if desc != "" {
		b.WriteString(": " + desc)
	}
	b.WriteString("\n")
	if when != "" {
		b.WriteString("  Use when: " + when + "\n")
	}
	b.WriteString("  Path: " + path + "\n")
	if root := red.String(foldLine(r.Root)); absClean(root) {
		b.WriteString("  Root: " + root + "\n")
	}
	return b.String()
}

// fitRows is as many of blocks, from the front, as limit holds, with a last
// line counting what was left out: the blocks that did not fit, and hidden,
// the ones a caller could not list at all (the agent tool's tail drops a row
// too long to be read whole, and counts it here). The count is inside the
// budget because it grows with the number dropped, so the loop asks what fits
// with the line that would then be written, not without it. The catalog hides
// nothing and passes maxCatalogRows.
func fitRows(blocks []string, hidden, limit int) string {
	total := 0
	for _, b := range blocks {
		total += len(b)
	}
	for keep := len(blocks); keep >= 0; keep-- {
		if keep < len(blocks) {
			total -= len(blocks[keep])
		}
		more := ""
		if n := len(blocks) - keep + hidden; n > 0 {
			more = fmt.Sprintf("… and %d more\n", n)
		}
		if total+len(more) <= limit {
			return strings.Join(blocks[:keep], "") + more
		}
	}
	// Unreachable while the budget is kilobytes and the line is bytes; it is
	// here so the function has no case that returns more than the budget.
	return fmt.Sprintf("… and %d more\n", len(blocks)+hidden)
}

// cutToBudget is text within limit bytes, with truncatedLine in place of what
// it drops. It cuts at a line boundary, so what is left is whole lines of the
// file rather than half a sentence; a first line longer than the budget has
// no boundary to cut at, and is cut at a rune boundary instead, because a
// half-written rune is not text at all. The marker counts against limit: the
// budget is on what is sent.
func cutToBudget(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	room := limit - len(truncatedLine)
	if room <= 0 {
		// A budget that cannot even hold the marker buys nothing; the
		// caller leaves such a document out before it gets here.
		return ""
	}
	cut := strings.LastIndexByte(text[:room], '\n') + 1
	if cut == 0 {
		for cut = room; cut > 0 && !utf8.RuneStart(text[cut]); cut-- {
		}
	}
	return text[:cut] + truncatedLine
}

// normalizeLines is text with every line ending as a newline: a file written
// on Windows, or by a tool that still emits a bare carriage return, otherwise
// puts a control character in the middle of the prompt's lines.
func normalizeLines(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// foldLine is s on one line: every run of whitespace becomes a single space,
// the ends are trimmed, control runes are dropped and invalid bytes become
// U+FFFD. It is the harness's own — internal/agent's sanitizeLine is the same
// idea and the harness may not import it (D-02) — and every field of a
// catalog row goes through it, because each of them is written inside craze's
// framing: a newline in a description would put the rest of it where the
// model reads craze's own lines.
func foldLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0 // never a leading space, so no trim is needed
		case unicode.IsControl(r):
			// Dropped: it says nothing to the model, and an escape sequence
			// would be acted on by whatever later prints the prompt back.
		default:
			if space {
				b.WriteRune(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// absClean reports whether p is a path craze can hand a model: absolute, so
// it resolves the same wherever the model is working, and already clean, so
// what is shown is what is opened.
func absClean(p string) bool { return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p }

// framingTexts are the words that open craze's own sections, lowercased.
var framingTexts = []string{"project and user instructions", "skills and commands", "from:"}

// escapeFraming is text with a backslash before every line that would read as
// one of craze's own headings, and nothing else touched. The headings are the
// one thing in the prompt that is craze speaking rather than a file craze is
// quoting: a document free to write them could end its own quotation and
// start a section of instructions in craze's voice. The escape is cheap and
// so is what it answers — an instruction file can already say anything in its
// own voice, and the rule that matters is that craze never fetches for it
// (R5, §3.4's confinement).
func escapeFraming(text string) string { return escapeFramingOf(text, framingTexts) }

// escapeFramingOf is escapeFraming against words, lowercased: the headings
// the text is quoted under and every one before it. A sub-agent's role is
// escaped against its own section's heading as well (child.go).
func escapeFramingOf(text string, words []string) string {
	if !containsFold(text, words) {
		return text // the common case: no line can match, so nothing is split
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		next := ""
		if i+1 < len(lines) {
			next = lines[i+1]
		}
		if forgesFraming(line, next, words) {
			lines[i] = `\` + line
		}
	}
	return strings.Join(lines, "\n")
}

// containsFold reports whether text holds any of the (lowercase) words.
func containsFold(text string, words []string) bool {
	low := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// forgesFraming reports whether line, followed by next, reads as one of
// craze's headings, whose words are words: up to three leading spaces (four
// would be an indented code block), a run of hashes, optional space, and then
// craze's words, with or without the closing hashes a heading may carry — or
// the same words with no hashes at all under a line of = or -, which is
// markdown's other heading. The comparison is case-insensitive and matches a
// prefix, so a heading with anything appended to craze's words is escaped too.
func forgesFraming(line, next string, words []string) bool {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 {
		return false
	}
	rest := line[i:]
	hashes := 0
	for hashes < len(rest) && rest[hashes] == '#' {
		hashes++
	}
	rest = strings.TrimLeft(rest[hashes:], " \t")
	low := strings.ToLower(rest)
	matched := false
	for _, w := range words {
		if strings.HasPrefix(low, w) {
			matched = true
			break
		}
	}
	return matched && (hashes > 0 || setextUnderline(next))
}

// setextUnderline reports whether line is markdown's underlined-heading rule:
// a run of one character, = or -, alone on its line.
func setextUnderline(line string) bool {
	t := strings.TrimRight(line, " \t")
	i := 0
	for i < len(t) && t[i] == ' ' {
		i++
	}
	if i > 3 {
		return false
	}
	if t = t[i:]; t == "" || (t[0] != '=' && t[0] != '-') {
		return false
	}
	return strings.Trim(t, t[:1]) == ""
}
