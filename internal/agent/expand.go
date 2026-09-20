package agent

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/craze/internal/acp"
)

// What one prompt may carry of craze's own. The block count is a bound on how
// much of a turn a draft can be, not a limit anyone is expected to reach: a
// line with nine plugin commands in it is a script, not a prompt. The byte
// ceiling is the same idea one entry down — a plugin shipping a megabyte of
// markdown must not become a megabyte of prompt.
const (
	maxPluginBlocks    = 8
	maxPluginBodyBytes = 64 << 10
	// pluginTruncated is the last line of a body that hit the ceiling, so the
	// model reads that it was cut rather than inferring it from a sentence
	// that stops mid-word.
	pluginTruncated = "[craze: body truncated at 64 KiB]"
)

// pluginTarget is what one typed spelling means: the names the menu, the event
// stream and the wire all use for it, and the entry whose body goes out. The
// two travel together because they are resolved together — a name means
// whatever the catalog said when it was resolved, and the body behind it must
// be the one that name was resolved from.
type pluginTarget struct {
	cmd   PluginCommand
	entry PluginEntry
}

// pluginRef is one resolved reference found in a draft: what it means, the
// spelling the user actually typed (which is what the block quotes back) and
// the arguments that followed it on that line.
type pluginRef struct {
	target pluginTarget
	typed  string
	args   string
}

// buildPluginLookup maps every spelling a user can type to the entry behind
// it: the displayed name and the qualified plugin:name, case-folded. An entry
// ResolvePluginNames dropped is in neither map — both its spellings mean
// something else — and a displayed name that stayed qualified is deliberately
// not reachable bare, so a bare /simplify goes to the agent that advertised it.
func buildPluginLookup(entries []PluginEntry, cmds []PluginCommand) map[string]pluginTarget {
	if len(cmds) == 0 {
		return nil
	}
	byKey := make(map[string]PluginEntry, len(entries))
	for _, e := range entries {
		key := strings.ToLower(e.Plugin + ":" + e.Name)
		if _, dup := byKey[key]; !dup {
			byKey[key] = e
		}
	}
	out := make(map[string]pluginTarget, 2*len(cmds))
	for _, c := range cmds {
		qualified := strings.ToLower(c.Qualified)
		e, ok := byKey[qualified]
		if !ok {
			continue
		}
		t := pluginTarget{cmd: c, entry: e}
		for _, key := range [2]string{strings.ToLower(c.Display), qualified} {
			if _, dup := out[key]; dup {
				continue
			}
			out[key] = t
		}
	}
	return out
}

// pluginRefs finds the plugin commands and skills a draft invokes. It is
// cursor's own scan (findCommandReferences): a slash at the start or right
// after whitespace, then the maximal run of [A-Za-z0-9_-] — optionally a colon
// and a second run, which is craze's qualified spelling and something cursor's
// name class can never hold — and the rest of that line as the arguments.
//
// A name that resolves to nothing is left alone: it is the agent's own command,
// or it is prose. The first reference to an entry wins and later ones are
// dropped, so one body is never sent twice; cursor attaches once per match,
// which is the deliberate difference.
func pluginRefs(text string, lookup map[string]pluginTarget) []pluginRef {
	if text == "" || len(lookup) == 0 {
		return nil
	}
	var out []pluginRef
	used := make(map[string]bool, maxPluginBlocks)
	eachPluginName(text, func(name string, end int) bool {
		target, ok := lookup[strings.ToLower(name)]
		if !ok {
			return true
		}
		key := strings.ToLower(target.cmd.Qualified)
		if used[key] {
			return true
		}
		used[key] = true
		out = append(out, pluginRef{target: target, typed: name, args: scanPluginArgs(text, end)})
		return len(out) < maxPluginBlocks
	})
	return out
}

// hasUnresolvedSlash reports whether the draft names something the lookup
// cannot answer. It is the question the catalog wait asks (§3.3): while the
// rows are still provisional every one of them is qualified, so a bare
// /probe-echo resolves to nothing — and waiting is only worth the delay when
// the draft holds a name the rename could rescue. An unknown slash such as
// /nope answers true too: craze cannot tell one the catalog will never explain
// from one it is about to, and the wait is bounded either way.
func hasUnresolvedSlash(text string, lookup map[string]pluginTarget) bool {
	unresolved := false
	eachPluginName(text, func(name string, _ int) bool {
		if _, ok := lookup[strings.ToLower(name)]; ok {
			return true
		}
		unresolved = true
		return false
	})
	return unresolved
}

// eachPluginName walks the /name tokens of a draft in order and hands each to
// fn with the offset that ends it; fn stops the walk by returning false. One
// scanner and not two, because the wait that precedes an expansion and the
// expansion itself have to agree, byte for byte, on what a token is.
func eachPluginName(text string, fn func(name string, end int) bool) {
	// Offset 0 counts as "after whitespace": a draft that is nothing but
	// /name is the common case.
	afterSpace := true
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r != '/' || !afterSpace {
			afterSpace = unicode.IsSpace(r)
			i += size
			continue
		}
		name, end := scanPluginName(text, i+1)
		afterSpace = false
		if name == "" {
			i += size
			continue
		}
		// Whatever the name was, the scan resumes past it; a slash inside it
		// was never the start of another one.
		i = end
		if !fn(name, end) {
			return
		}
	}
}

// scanPluginName reads the name at i (just past the slash) and returns it with
// the offset that ends it. Anything outside the class ends it, so /watch-pr,
// names watch-pr exactly as cursor's regex does.
func scanPluginName(text string, i int) (string, int) {
	j := scanNameRun(text, i)
	if j == i {
		return "", i
	}
	if j < len(text) && text[j] == ':' {
		if k := scanNameRun(text, j+1); k > j+1 {
			j = k
		}
	}
	return text[i:j], j
}

func scanNameRun(text string, i int) int {
	for i < len(text) && isPluginNameByte(text[i]) {
		i++
	}
	return i
}

// scanPluginArgs is the rest of the name's own line, after at least one space
// or tab (cursor's [ \t]+([^\n]+)). A name followed by anything else — a comma,
// a newline, the end of the draft — took no arguments.
func scanPluginArgs(text string, i int) string {
	j := i
	for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
		j++
	}
	if j == i {
		return ""
	}
	end := len(text)
	if nl := strings.IndexByte(text[j:], '\n'); nl >= 0 {
		end = j + nl
	}
	return strings.TrimSpace(text[j:end])
}

// pluginVars is what a ${…} token in a body may stand for. Root is the entry's
// install directory, which every provider substitutes. Skill and Session are
// native's alone — grok-build's ${CLAUDE_SKILL_DIR} and ${CLAUDE_SESSION_ID} —
// and are recognised only when Extras says so, because a body craze expands on
// cursor's behalf has to reach cursor's agent as its author wrote it: cursor
// substitutes neither, and craze filling one in would be craze inventing
// content for another agent's content.
type pluginVars struct {
	Root    string
	Skill   string
	Session string
	Extras  bool
}

// expandCommandBody is cursor's command expansion: $ARGUMENTS for the whole
// argument list, $1..$99 for one of them, and — when the body asked for
// neither and arguments were given anyway — the arguments appended under it,
// which is how a command with no placeholder still sees them.
func expandCommandBody(body, root, args string) string {
	out, spent := substituteBody(body, pluginVars{Root: root}, strings.Fields(args))
	if !spent && args != "" {
		return out + "\n\n" + args
	}
	return out
}

// expandSkillBody is cursor's skill rule, which is no rule at all beyond the
// plugin's own path: the file is attached as it stands and the arguments live
// in the invocation line only. grok substitutes into skills; cursor does not,
// and this is cursor's content.
func expandSkillBody(body, root string) string {
	out, _ := substituteBody(body, pluginVars{Root: root}, nil)
	return out
}

// substituteBody is one left-to-right pass over a body, replacing the plugin's
// install path — cursor writes both spellings over plugin content, so craze
// does too — and, when fields is non-nil, cursor's argument placeholders. It
// reports whether an argument placeholder was spent, which is what decides
// whether the arguments are appended instead.
//
// One pass, and what a replacement produced is never read again: expanding
// $ARGUMENTS first and scanning the result for $1 afterwards would substitute
// into the user's own arguments, so an argument spelled "$2" would come out as
// some other argument entirely.
func substituteBody(body string, vars pluginVars, fields []string) (string, bool) {
	if !strings.ContainsRune(body, '$') {
		return body, false
	}
	var b strings.Builder
	started, spent, last := false, false, 0
	for i := 0; i < len(body); i++ {
		if body[i] != '$' {
			continue
		}
		repl, end, arg := substituteToken(body, i, vars, fields)
		if end == 0 {
			continue
		}
		if !started {
			b.Grow(len(body))
			started = true
		}
		b.WriteString(body[last:i])
		b.WriteString(repl)
		spent = spent || arg
		i, last = end-1, end
	}
	if !started {
		return body, false
	}
	b.WriteString(body[last:])
	return b.String(), spent
}

// substituteToken reads the placeholder starting at the $ at i. It returns what
// to write, the offset just past what it consumed — zero when there is no
// placeholder here — and whether it was an argument placeholder rather than the
// plugin root.
//
// The positional rule is cursor's (?<!\w)\$(\d{1,2})\b: a $ that is not part
// of a word, one or two digits, and a word boundary after them. So a$1 and $1x
// are text, and $123 is text too — the third digit denies the boundary and the
// backtrack to one digit denies it again. The root and $ARGUMENTS tokens carry
// no such rule, because cursor replaces those as plain strings.
func substituteToken(body string, i int, vars pluginVars, fields []string) (string, int, bool) {
	for _, token := range [2]string{"${CLAUDE_PLUGIN_ROOT}", "${CURSOR_PLUGIN_ROOT}"} {
		if strings.HasPrefix(body[i:], token) {
			return vars.Root, i + len(token), false
		}
	}
	// Native's two extra variables, in the same pass and under the same
	// one-pass rule: a path or an id they put into the body is never rescanned
	// for a placeholder of its own.
	if vars.Extras {
		for _, v := range [2]struct{ token, value string }{
			{"${CLAUDE_SKILL_DIR}", vars.Skill},
			{"${CLAUDE_SESSION_ID}", vars.Session},
		} {
			if strings.HasPrefix(body[i:], v.token) {
				return v.value, i + len(v.token), false
			}
		}
	}
	if fields == nil {
		return "", 0, false
	}
	if strings.HasPrefix(body[i:], "$ARGUMENTS") {
		return strings.Join(fields, " "), i + len("$ARGUMENTS"), true
	}
	if i > 0 && isWordByte(body[i-1]) {
		return "", 0, false
	}
	// The maximal digit run, which is where the regex ends up whichever way it
	// backtracks: three digits or more can never find the boundary, and a run
	// of two followed by a word byte cannot either, because the only way back
	// is onto a digit, and a digit is a word byte.
	run := i + 1
	for run < len(body) && isDigitByte(body[run]) {
		run++
	}
	digits := run - i - 1
	if digits == 0 || digits > 2 || (run < len(body) && isWordByte(body[run])) {
		return "", 0, false
	}
	// A $1 with no first argument is still a placeholder the body spent, so
	// the arguments must not also be appended under it.
	if n, _ := strconv.Atoi(body[i+1 : run]); n >= 1 && n <= len(fields) {
		return fields[n-1], run, true
	}
	return "", run, true
}

// isWordByte is the ASCII \w cursor's positional regex spells: isPluginNameByte
// without the "-", because a hyphen ends a word where it does not end a name.
func isWordByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_':
		return true
	}
	return false
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// capPluginBody holds one body to maxPluginBodyBytes, cut at the last line
// boundary under the ceiling so the model never reads half a sentence as if it
// were the whole instruction.
func capPluginBody(body string) string {
	if len(body) <= maxPluginBodyBytes {
		return body
	}
	// The marker goes out with the body, so it comes out of the same budget:
	// a ceiling the result is allowed to exceed is not a ceiling.
	cut := body[:maxPluginBodyBytes-len(pluginTruncated)-1]
	if i := strings.LastIndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i]
	} else {
		// One long line: back off to a rune boundary rather than sending the
		// first half of a multi-byte character.
		for len(cut) > 0 && !utf8.RuneStart(body[len(cut)]) {
			cut = cut[:len(cut)-1]
		}
	}
	return cut + "\n" + pluginTruncated
}

// escapeClosingTag rewrites every literal closing tag of the block's own kind,
// whatever its case, so a body that talks about </command> cannot end the
// element it is inside. The rest of the occurrence keeps the case it was
// written in; only the slash is escaped.
//
// XML lets an end tag carry whitespace before its ">", so "</command >" closes
// the element just as well and is matched too. The comparison is EqualFold
// against the body itself rather than against a lowercased copy: folding a
// 64 KiB body would allocate the whole thing to answer a question about six
// bytes, and a rune that folds to a different width would put every offset
// after it out by that much.
func escapeClosingTag(body, tag string) string {
	open := "</" + tag
	var b strings.Builder
	// Everything before copied is already in b; zero means nothing matched.
	copied := 0
	for at := 0; at+len(open) < len(body); at++ {
		if body[at] != '<' || !strings.EqualFold(body[at:at+len(open)], open) {
			continue
		}
		end := closingTagEnd(body, at+len(open))
		if end == 0 {
			continue
		}
		if copied == 0 {
			b.Grow(len(body) + 8)
		}
		b.WriteString(body[copied:at])
		b.WriteString(`<\/`)
		b.WriteString(body[at+2 : end])
		at = end - 1
		copied = end
	}
	if copied == 0 {
		return body
	}
	b.WriteString(body[copied:])
	return b.String()
}

// closingTagEnd is the offset just past the ">" of an end tag whose name ended
// at i, or zero when what follows the name is neither whitespace nor that ">".
func closingTagEnd(body string, i int) int {
	for i < len(body) {
		switch body[i] {
		case '>':
			return i + 1
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return 0
		}
	}
	return 0
}

// attrEscaper is the whole of XML attribute escaping craze needs. It is not
// html.EscapeString: that spells a quote &#34; and escapes an apostrophe too,
// and the block format is a golden.
var attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// escapeAttr makes one already-folded value safe inside a double-quoted XML
// attribute, so neither a quote nor an angle bracket can close it. Callers fold
// first with sanitizeLine: a path or an argument carrying a newline would
// otherwise put the rest of the tag on a line of its own.
func escapeAttr(s string) string {
	if !strings.ContainsAny(s, `&<>"`) {
		return s
	}
	return attrEscaper.Replace(s)
}

// pluginBlockFormat is the block as the model reads it. It is one template
// rather than a chain of writes so that the bytes on the wire can be read off
// the source, beside the golden that pins them. The first line is a sentence
// rather than a header because the model reads these blocks with no system
// prompt to explain them: it has to say what it is looking at, which name
// produced it and where the text came from, all by itself.
const pluginBlockFormat = `The user invoked %s (the "%s" %s %s, %s). Follow its instructions:` + "\n" +
	`<%s name="%s" plugin="%s" args="%s">` + "\n%s\n" + `</%s>`

// pluginSourcePhrase is the fourth field of that sentence: where the content
// came from, in words. Every entry a provider's own scan found belongs to a
// plugin, so this is the whole of it for cursor; native has two sources that
// are not plugins at all and phrases those itself (nativeSourcePhrase). The
// phrase is the caller's rather than a switch on the id here, because an id
// means different things to different providers — a cursor plugin really
// called "project" is a plugin, where native's "project" never is.
func pluginSourcePhrase(plugin string) string {
	return `from the "` + plugin + `" plugin`
}

// pluginBlock is one expansion as it goes on the wire, for a provider whose
// content craze scanned out of plugin roots.
func pluginBlock(ref pluginRef) string {
	e := ref.target.entry
	// A skill is attached as it stands and a command spends its arguments;
	// picking the branch first is what keeps the rule craze does not apply
	// from running anyway.
	kind, body := PluginKindCommand, ""
	if e.Kind == PluginKindSkill {
		kind, body = PluginKindSkill, expandSkillBody(e.Body, e.Root)
	} else {
		body = expandCommandBody(e.Body, e.Root, ref.args)
	}
	return pluginBlockOf(ref, kind, pluginSourcePhrase(sanitizeLine(e.Plugin)), body)
}

// pluginBlockOf is the block itself, once the caller has expanded the body its
// provider's rules call for and said where it came from. Everything that makes
// the bytes safe — the escaped closing tag, the ceiling, the folded attributes
// — lives here and not in either caller, so a second provider's expansion
// cannot arrive at a differently-escaped block.
func pluginBlockOf(ref pluginRef, kind, source, body string) string {
	e := ref.target.entry
	// Capped last: escaping can only grow a body, and a ceiling something is
	// applied after is not a ceiling.
	body = capPluginBody(escapeClosingTag(body, kind))

	name, plugin := sanitizeLine(e.Name), sanitizeLine(e.Plugin)
	args := sanitizeLine(ref.args)
	invoked := "/" + sanitizeLine(ref.typed)
	if args != "" {
		invoked += " " + args
	}
	return fmt.Sprintf(pluginBlockFormat,
		invoked, name, kind, source, sanitizeLine(e.Path),
		kind, escapeAttr(name), escapeAttr(plugin), escapeAttr(args),
		strings.TrimRight(body, "\n"), kind)
}

// promptBlocks is the whole of what one session/prompt carries: the draft
// verbatim as block 1 — the agent must see the /name the user typed, and its
// own catalog still owns every name craze did not resolve — then one block per
// reference, in the order they were written.
func promptBlocks(text string, refs []pluginRef) ([]acp.ContentBlock, []ExpandedCommand) {
	blocks := make([]acp.ContentBlock, 0, len(refs)+1)
	blocks = append(blocks, acp.ContentBlock{Type: "text", Text: text})
	if len(refs) == 0 {
		return blocks, nil
	}
	cmds := make([]ExpandedCommand, 0, len(refs))
	for _, ref := range refs {
		block := pluginBlock(ref)
		blocks = append(blocks, acp.ContentBlock{Type: "text", Text: block})
		cmds = append(cmds, ExpandedCommand{
			PluginCommand: ref.target.cmd,
			Path:          ref.target.entry.Path,
			Text:          block,
		})
	}
	return blocks, cmds
}
