package harness

// Sub-agent model and effort resolution (plan 026 §3.6). C3b's runner calls
// resolveChildModel and, once it has an alias, resolveChildEffort, before
// opening the child (Options.Child, child.go): the pair travels on
// SubagentStarted (a later commit) as the alias and effort the child
// actually runs on.
//
// Both errors' text is read by the parent model, not logged: resolveChildModel's
// "Unknown model" and resolveChildEffort's "Effort ... is not offered" are
// the agent tool's result verbatim (§3.7's `tool_error` row), so their
// wording is fixed by the plan and is not framed as a Go error ("harness: ",
// a lowercase sentence) the way the rest of the package's errors are.

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// childModelInput is what resolveChildModel needs to pick a sub-agent's
// model: the call's own `model`, if any, and the persona's `model:`, if any.
// The third candidate, the configured default, is read off the table itself
// (Table.Subagents.Model), so it needs no field here.
//
// ParentAlias and ParentEffort are the RUNNING TURN's model and effort
// (turn.model, via t.model.r.Alias / t.model.effort), not the session's
// current one (Session.cur): a SetModel mid-turn must not move a sub-agent
// call that turn already made (plan 026 §3.6, panel CodeRabbit 12).
type childModelInput struct {
	Call, Persona             string
	ParentAlias, ParentEffort string
}

// resolveChildModel picks a sub-agent's model, first hit wins: the call's
// `model`, the persona's `model:`, the table's configured `subagents.model`,
// then the parent's own (plan 026 §3.6). warn is Session.warn normally;
// tests may hand in their own to capture what would have been journalled.
//
// Each candidate is read the same way (resolveModelValue): "inherit" is the
// parent's model; a recognised tier name — a BuiltinTiers name or a key
// under [subagents.tiers] — maps through the table's tier map, and an
// unmapped tier is the parent's model; anything else is matched as an alias
// (Session.matchModel, the adapter's `--model` normalisation). A candidate
// that resolves to a model whose provider has no key is treated as
// unresolved too: Open would only fail on it later, and content — a
// persona's or the default's choice — is not allowed to fail the call for a
// key it does not control.
//
// The call is the one candidate a failure to resolve is fatal for: an
// unrecognised value is the "Unknown model" error, naming every alias and
// tier the table has; one with no key errors naming the provider (never the
// key). A persona or default candidate that fails either way falls through
// to the next one with a single warn line, because content is not the
// model's choice.
func (s *Session) resolveChildModel(in childModelInput, warn func(string)) (alias string, err error) {
	type candidate struct {
		raw      string
		label    string
		required bool
	}
	candidates := []candidate{
		{in.Call, "the call's model", true},
		{in.Persona, "the persona's model", false},
		{s.table.Subagents.Model, "the configured sub-agent default model", false},
	}
	for _, c := range candidates {
		if c.raw == "" {
			continue
		}
		got, recognized := s.resolveModelValue(c.raw, in.ParentAlias)
		if !recognized {
			if c.required {
				return "", unknownModelError(s.quoteRaw(c.raw), s.table)
			}
			warnLine(warn, fmt.Sprintf("%s %q does not name a model, a tier, or \"inherit\"; falling through to the next default", c.label, c.raw))
			continue
		}
		if _, rerr := s.table.Resolve(got, s.getenv); rerr != nil {
			if c.required {
				return "", fmt.Errorf("%s %q: %w", c.label, c.raw, rerr)
			}
			warnLine(warn, fmt.Sprintf("%s %q resolves to %q, which has no usable API key; falling through to the next default", c.label, c.raw, got))
			continue
		}
		return got, nil
	}
	// The parent's own model: already open and resolved once this turn, so
	// it is not re-checked against the fall-through rules above.
	return in.ParentAlias, nil
}

// resolveChildEffort picks a sub-agent's effort once its model (alias, from
// resolveChildModel) is known, first hit wins: the call's `effort`, the
// persona's `effort:`, the table's configured `subagents.effort`, the
// parent's own effort — but only when the child ends up running the
// parent's own model, since an effort level from one model means nothing on
// another — then alias's default_effort (plan 026 §3.6).
//
// A call effort alias does not offer is the "Effort ... is not offered"
// error. A persona or default effort it does not offer falls through with a
// warn line. alias with no effort control (no Efforts at all) resolves to
// "", and a call effort on it fails the same way, its list read as "none".
func (s *Session) resolveChildEffort(alias, callEffort, personaEffort, parentAlias, parentEffort string, warn func(string)) (string, error) {
	m, ok := s.table.Models[alias]
	if !ok {
		// Unreachable: resolveChildModel only ever returns a table alias.
		return "", fmt.Errorf("harness: sub-agent model %q is not in the table", alias)
	}
	if callEffort != "" {
		if !slices.Contains(m.Efforts, callEffort) {
			return "", effortNotOfferedError(s.quoteRaw(callEffort), alias, m.Efforts)
		}
		return callEffort, nil
	}
	if personaEffort != "" {
		if slices.Contains(m.Efforts, personaEffort) {
			return personaEffort, nil
		}
		warnLine(warn, fmt.Sprintf("the persona's effort %q is not offered by %q; falling through to the next default", personaEffort, alias))
	}
	if s.table.Subagents.Effort != "" {
		if slices.Contains(m.Efforts, s.table.Subagents.Effort) {
			return s.table.Subagents.Effort, nil
		}
		warnLine(warn, fmt.Sprintf("the configured sub-agent default effort %q is not offered by %q; falling through to the next default", s.table.Subagents.Effort, alias))
	}
	if alias == parentAlias && parentEffort != "" {
		return parentEffort, nil
	}
	return m.DefaultEffort, nil
}

// resolveModelValue reads one candidate's raw `model` text the way §3.6
// describes: "inherit" is parentAlias; a recognised tier name maps through
// the table's tiers, an unmapped one resolving to parentAlias; anything else
// is matched as an alias. recognized is false only in the last case, when
// nothing matches.
func (s *Session) resolveModelValue(raw, parentAlias string) (alias string, recognized bool) {
	if strings.EqualFold(raw, "inherit") {
		return parentAlias, true
	}
	if tierAlias, isTier := s.tierAlias(raw); isTier {
		if tierAlias == "" {
			return parentAlias, true
		}
		return tierAlias, true
	}
	return s.matchModelAlias(raw)
}

// tierAlias is what tier name raw maps to in the table: the mapped alias
// when [subagents.tiers] has it (case-insensitively, its keys already being
// lowercase by validateSubagents), else "" with isTier true when raw is one
// of BuiltinTiers (recognised, but unmapped, so the caller falls back to the
// parent's model), else isTier false when raw is not a tier name at all.
func (s *Session) tierAlias(raw string) (alias string, isTier bool) {
	lower := strings.ToLower(raw)
	if a, ok := s.table.Subagents.Tiers[lower]; ok {
		return a, true
	}
	for _, t := range modeltable.BuiltinTiers {
		if t == lower {
			return "", true
		}
	}
	return "", false
}

// matchModelAlias is raw matched against the table's aliases: Session.matchModel
// (the adapter's `--model` normalisation) when set, else an exact,
// case-sensitive alias match, which is what Options.MatchModel's doc
// promises for a nil matcher.
func (s *Session) matchModelAlias(raw string) (alias string, ok bool) {
	if s.matchModel != nil {
		return s.matchModel(raw)
	}
	if _, ok := s.table.Models[raw]; ok {
		return raw, true
	}
	return "", false
}

// warnLine calls warn with msg when warn is not nil; resolveChildModel and
// resolveChildEffort's shared "nil discards" rule (Options.Warn's doc).
func warnLine(warn func(string), msg string) {
	if warn != nil {
		warn(msg)
	}
}

// unknownModelCap bounds the "Unknown model" error like §3.4's unknown-type
// text (system.go's fitRows): the parent model gets back something it can
// read whole, however large the table, the tier map, or the raw value it
// sent (review r5, finding 8).
const unknownModelCap = 1024

// unknownModelRawCap bounds the raw value quoted inside "Unknown model
// `...`" once capModelList has to shorten something (review r5, finding 8).
// The original cap only ever trimmed the alias list, so a raw call value on
// its own — the one part of this message craze does not choose the length
// of — could still make the whole thing oversized, and so could the tier
// clause once [subagents.tiers] holds enough entries. raw is folded to one
// line first (an embedded newline would make the message hard to read long
// before it is oversized), then cut at a rune boundary with "…", the same
// way cutRole in native_personas.go cuts an over-budget persona body.
const unknownModelRawCap = 200

// unknownModelError is a call's `model` naming neither a table alias, a
// recognised tier, nor "inherit": every alias and every configured tier
// mapping, so the parent's model can retry with a name that exists (plan 026
// §3.6). raw is never omitted: the model needs to see what it sent to
// correct it.
func unknownModelError(raw string, table *modeltable.Table) error {
	aliases := table.Aliases() // sorted
	var tierParts []string
	for _, name := range slices.Sorted(maps.Keys(table.Subagents.Tiers)) {
		tierParts = append(tierParts, fmt.Sprintf("%s → %s", name, table.Subagents.Tiers[name]))
	}
	tiers := "none configured"
	if len(tierParts) > 0 {
		tiers = strings.Join(tierParts, ", ")
	}
	msg := fmt.Sprintf("Unknown model `%s`. Models: %s; tiers: %s. Omit `model` to use the parent's.",
		raw, strings.Join(aliases, ", "), tiers)
	if len(msg) <= unknownModelCap {
		return errors.New(msg)
	}
	return errors.New(capModelList(raw, aliases, tierParts))
}

// capModelList is unknownModelError's message cut to fit unknownModelCap,
// however large raw, aliases or tierParts are (review r5, finding 8: the
// original only ever trimmed aliases, so a long raw value or a large tier
// map could still exceed the cap on their own).
//
// raw is folded and cut first (cutRawValue, unknownModelRawCap): it is the
// one input craze does not otherwise bound. The tier clause is then tried at
// its full length before anything is cut from it — it is usually the shorter
// half, and a tier name is what a persona is most likely to have sent — with
// aliases cut first, from the front, the same "… and N more" shape as
// before; only once dropping every alias still does not fit does the tier
// clause give way too, the same way.
func capModelList(raw string, aliases, tierParts []string) string {
	prefix := fmt.Sprintf("Unknown model `%s`. Models: ", cutRawValue(raw))
	for tierKeep := len(tierParts); tierKeep >= 0; tierKeep-- {
		tiers := "none configured"
		if len(tierParts) > 0 {
			tiers = capJoin(tierParts, tierKeep)
		}
		suffix := fmt.Sprintf("; tiers: %s. Omit `model` to use the parent's.", tiers)
		for aliasKeep := len(aliases); aliasKeep >= 0; aliasKeep-- {
			msg := prefix + capJoin(aliases, aliasKeep) + suffix
			if len(msg) <= unknownModelCap {
				return msg
			}
		}
	}
	// Unreachable: cutRawValue bounds raw to unknownModelRawCap bytes, and
	// the two fixed sentences plus that cap are within unknownModelCap even
	// with every alias and every tier dropped; here so the loop cannot fall
	// through.
	return prefix + "; tiers: none configured. Omit `model` to use the parent's."
}

// capJoin is items joined from the front, keep of them, plus a count of what
// was left out — the same shape for the alias list and, since review r5's
// finding 8, the tier list too. keep <= 0 is "" for an empty items (nothing
// to count) and "… and N more" for a non-empty one, never a bare comma with
// nothing before it.
func capJoin(items []string, keep int) string {
	if keep <= 0 {
		if len(items) == 0 {
			return ""
		}
		return fmt.Sprintf("… and %d more", len(items))
	}
	if keep >= len(items) {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:keep], ", ") + fmt.Sprintf(", … and %d more", len(items)-keep)
}

// quoteRaw is a value the parent's model sent, made safe to quote back in a
// refusal before anything shortens it: redacted, folded onto one line, and
// redacted again. The refusals cut a long value (cutRawValue), and a cut made
// first can leave all but the last byte of a known key, which no later pass
// recognises (review r9); folding comes between the two passes because
// collapsing a whitespace run can itself spell a key out. What it returns is
// cut afterwards by the refusal's own renderer, and cutting redacted text can
// shorten a marker but never rebuild a key.
func (s *Session) quoteRaw(raw string) string {
	// Session.Redact's set, the children's keys included (review r10).
	red := s.redactor()
	return red.String(strings.Join(strings.Fields(red.String(raw)), " "))
}

// cutRawValue is raw folded to one line — its whitespace runs, newlines
// included, collapsed to single spaces — and cut at a rune boundary with "…"
// past unknownModelRawCap (review r5, finding 8). The unknown effort's text
// quotes the call's effort through it as well (review r7).
func cutRawValue(raw string) string {
	folded := strings.Join(strings.Fields(raw), " ")
	if len(folded) <= unknownModelRawCap {
		return folded
	}
	cut := unknownModelRawCap
	for cut > 0 && !utf8.RuneStart(folded[cut]) {
		cut--
	}
	return folded[:cut] + "…"
}

// effortNotOfferedError is a call's `effort` alias does not offer, naming
// what it does (plan 026 §3.6). An empty Efforts (no effort control at all)
// reads as "none".
//
// effort is the call's own, which the model may have sent any size and any
// shape, so it is folded to one line and cut (cutRawValue), as the unknown
// model's raw value is: the refusal stays one short line the model reads
// whole, rather than one the runner's cut then shortens to 50 KiB (review r7,
// finding 2). alias and the list are the table's.
func effortNotOfferedError(effort, alias string, efforts []string) error {
	list := "none"
	if len(efforts) > 0 {
		list = strings.Join(efforts, ", ")
	}
	// Built into a variable, then wrapped, like unknownModelError: this
	// sentence is the model-facing text itself (§3.6), not a Go-style
	// lowercase, unpunctuated error, so it is deliberately not a literal
	// fmt.Errorf format string (which staticcheck's ST1005 would flag).
	msg := fmt.Sprintf("Effort `%s` is not offered by `%s`. Its efforts: %s.", cutRawValue(effort), alias, list)
	return errors.New(msg)
}
