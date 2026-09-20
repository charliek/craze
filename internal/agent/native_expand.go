package agent

import (
	"path/filepath"
	"strings"
)

// Native's half of the expansion path. The scan, the substitution and the
// joining all differ from cursor's, and each difference is a decision rather
// than an accident, so they are written here rather than bent into expand.go:
// cursor's bytes are a promise craze has already made (its goldens, its
// unedited tests), and native's content is the owner's own Claude files, which
// Claude Code and grok-build read by rules of their own.

// maxNativeBlockBytes is what one prompt's expansions may add, over and above
// expand.go's eight blocks and 64 KiB per body. It exists because the native
// harness replays the whole user message on every later request of the turn
// (store.Context): a draft that expanded to half a megabyte would be sent
// again with every tool result, where an ACP agent sends the prompt once. A
// block that would cross the line, and everything after it, is left out; the
// absence of its ⤷ row is what says so, since only what was sent is announced.
const maxNativeBlockBytes = 128 << 10

// nativeBlockSep is what joins the draft to the first block and one block to
// the next. It counts against maxNativeBlockBytes like anything else the
// blocks add: a total that measured only the blocks would be exceeded by two
// bytes per block, which is not what a ceiling means.
const nativeBlockSep = "\n\n"

// nativeRefs finds the commands and skills a native draft invokes. A reference
// counts only as the first token of a line, after any leading spaces or tabs:
// native's content is full of shell, and `cd /build && make` must not expand a
// project command called build. Several references compose by being written on
// separate lines.
//
// Everything else is cursor's, deliberately: the name class and the argument
// rule are scanPluginName's and scanPluginArgs', so a name means the same
// thing wherever it is typed, and the first reference to an entry wins so one
// body is never sent twice.
func nativeRefs(text string, lookup map[string]pluginTarget) []pluginRef {
	if text == "" || len(lookup) == 0 {
		return nil
	}
	var out []pluginRef
	used := make(map[string]bool, maxPluginBlocks)
	eachNativeName(text, func(name string, end int) bool {
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

// eachNativeName walks the /name tokens that begin a line and hands each to
// fn with the offset that ends it; fn stops the walk by returning false. One
// name per line at most — what follows the first token is that reference's
// arguments, not another reference.
func eachNativeName(text string, fn func(name string, end int) bool) {
	for at := 0; at <= len(text); {
		end := len(text)
		if nl := strings.IndexByte(text[at:], '\n'); nl >= 0 {
			end = at + nl
		}
		i := at
		for i < end && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
		if i < end && text[i] == '/' {
			if name, stop := scanPluginName(text, i+1); name != "" && !fn(name, stop) {
				return
			}
		}
		if end == len(text) {
			return
		}
		at = end + 1
	}
}

// expandNativeBody is native's substitution, for a command and a skill alike:
// Claude Code's own placeholders, which grok-build also honours, over whatever
// the author wrote. Unlike cursor, native substitutes into a skill too — the
// file is the owner's own, and a skill that names ${CLAUDE_SKILL_DIR} means it.
//
// The indexing is deliberately craze's and not grok's: $1 is the first
// argument, as Claude Code's command files and cursor's expansion both read
// it, and $ARGUMENTS[N] is not built at all (§3.3).
//
// With no placeholder in the body and arguments given anyway, they are
// appended under a heading rather than bare: the body is a whole instruction
// file here, often ending in prose, and a line of loose words under it would
// read as part of the last paragraph.
func expandNativeBody(body string, vars pluginVars, args string) string {
	out, spent := substituteBody(body, vars, strings.Fields(args))
	if !spent && args != "" {
		return out + "\n\n**ARGUMENTS:** " + args
	}
	return out
}

// nativeSourcePhrase says where a block came from, in the sentence's words.
// Native's own two sources are not plugins and must not claim to be: a command
// under the project's .claude has no manifest, no install and no marketplace,
// and calling it one would tell the model something false about the file it is
// being shown.
func nativeSourcePhrase(id string) string {
	switch id {
	case nativeProjectID:
		return "from this project"
	case nativeUserID:
		return "from the user's own commands"
	}
	return pluginSourcePhrase(id)
}

// nativeBlock is one native expansion as it goes on the wire. sessionID is
// what ${CLAUDE_SESSION_ID} means, and ${CLAUDE_SKILL_DIR} is the entry file's
// own directory — for a command as much as a skill, since that is the only
// reading of "the directory this file is in" that holds for both.
//
// The order is substitute, escape, redact, cap, frame. Redaction sits before
// the ceiling because it grows what it rewrites — a key of a dozen bytes
// becomes a marker of twenty-seven — so a body densely seeded with one and
// capped first would swell back through the ceiling afterwards, and that
// ceiling exists to bound what a single file can add to every request of the
// turn. It sits after the escape because escaping is about the element and
// redaction is about the bytes, and an escaped closing tag holds no key.
func nativeBlock(ref pluginRef, sessionID string, redact func(string) string) string {
	e := ref.target.entry
	kind := PluginKindCommand
	if e.Kind == PluginKindSkill {
		kind = PluginKindSkill
	}
	vars := pluginVars{
		Root:    e.Root,
		Skill:   filepath.Dir(e.Path),
		Session: sessionID,
		Extras:  true,
	}
	body := escapeClosingTag(expandNativeBody(e.Body, vars, ref.args), kind)
	body = capPluginBody(nativeRedacted(redact, body))
	// The frame is redacted as well, and not only the body: its sentence names
	// the entry's path and its attributes carry the entry's name, its plugin id
	// and the user's own arguments, and a key in any of those reaches the wire
	// exactly as a key in the body would. The second pass over the body costs
	// one scan and changes nothing — a Replacer's output holds no key, so
	// redacting it again is a no-op (package redact).
	return nativeRedacted(redact, pluginFrame(ref, kind, nativeSourcePhrase(sanitizeLine(e.Plugin)), body))
}

// nativeRedacted is redact applied when there is one. The nil redactor is the
// unstarted session and the unit tests, which have no harness to borrow one
// from; every path that reaches the wire passes the session's own.
func nativeRedacted(redact func(string) string, text string) string {
	if redact == nil {
		return text
	}
	return redact(text)
}

// nativePrompt is the whole of what one native turn sends: the draft as the
// user typed it, then a block per reference, blank-line separated. The harness
// takes one string rather than content blocks (turn.go), so the join is
// craze's to make, and the draft comes first for the reason block 1 does on
// the ACP path — the model has to see the /name that was typed, not only what
// craze made of it.
//
// redact is the session's own redactor. Run persists and sends the user's text
// as it is given, so a provider key inside a command body would otherwise
// reach the wire, the transcript, the journal and --json; nativeBlock applies
// it to the block, which is also what the event carries, so the two can never
// disagree about what was sent.
func nativePrompt(text string, refs []pluginRef, sessionID string, redact func(string) string) (string, []ExpandedCommand) {
	if len(refs) == 0 {
		return text, nil
	}
	var b strings.Builder
	b.WriteString(text)
	cmds := make([]ExpandedCommand, 0, len(refs))
	spent := 0
	for _, ref := range refs {
		block := nativeBlock(ref, sessionID, redact)
		// Measured after redaction, because that is the string that goes out,
		// and with the separator, because that is what the join actually adds.
		// The first block that does not fit ends the run: dropping it and
		// taking a smaller one after it would make what the model is shown
		// depend on the sizes of files the user never mentioned.
		cost := len(nativeBlockSep) + len(block)
		if spent+cost > maxNativeBlockBytes {
			break
		}
		spent += cost
		b.WriteString(nativeBlockSep)
		b.WriteString(block)
		cmds = append(cmds, ExpandedCommand{
			PluginCommand: ref.target.cmd,
			// The path as data rather than as a handle: nothing opens it from
			// here — the body was read at discovery — and this copy is what the
			// event, the journal and --json carry, so a key in it is a leak like
			// any other. The entry keeps the real spelling, which is what
			// ${CLAUDE_SKILL_DIR} and any later reader have to mean.
			Path: nativeRedacted(redact, ref.target.entry.Path),
			Text: block,
		})
	}
	if len(cmds) == 0 {
		return text, nil
	}
	return b.String(), cmds
}
