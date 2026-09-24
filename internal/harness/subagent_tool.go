package harness

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The agent tool's per-session description (plan 026 §3.3, owner decision 4).
// The profile's agent tool is static — its description is agent.txt, which
// the profile's specs.golden pins — and openTools wraps it, for a session that
// offers it, in describedTool, whose description adds a tail rendered for this
// session: the agent types a call may name and the models and tiers it may
// choose, which is how the parent knows them up front, as grok-build, opencode
// and codex tell theirs. The tail is part of the spec openTools collects, so
// it is what the model is offered, what the dispatcher holds, and what the
// header's tools_sha256 hashes: a session's digest now varies with the
// personas and models it offers (§9). A child is never offered the tool, so
// it never renders one.

// The tail's budgets, in bytes of what goes out (plan 026 §3.3). Each section
// is measured whole, its heading included: about 10 KiB on every request at
// most (§9), cached after the first.
const (
	// maxTailRow bounds one row of either section, its newline included, so a
	// persona with a long description — plugin descriptions carry whole
	// examples — cannot spend the section on itself.
	maxTailRow = 300
	// maxTypesSection and maxModelsSection bound the two sections.
	maxTypesSection  = 8 << 10
	maxModelsSection = 2 << 10
	// maxTiersLine bounds the models section's tier line, which is written
	// whole after the rows, so the rows always keep most of the section.
	maxTiersLine = 512

	agentTypesHeading = "## Agent types\n"
	modelsHeading     = "## Models\n"
)

// describedTool is a tool whose description has a tail after its own: the
// profile's agent tool, as a session that offers it offers it. It prepares
// calls exactly as the tool does.
type describedTool struct {
	tool.Tool
	tail string
}

// Spec is the tool's, with the tail after its description and a blank line
// between the two.
func (d *describedTool) Spec() tool.Spec {
	s := d.Tool.Spec()
	if !strings.HasSuffix(s.Description, "\n") {
		s.Description += "\n"
	}
	s.Description += "\n" + d.tail
	return s
}

// agentTail is the session's tail: the agent types in types (agentTypes), each
// with the tools it gives a child out of offered — the child tools of the
// session's profile, in its order — and the models of table a child can run
// on, getenv deciding which have a key.
//
// Every field is folded onto one line and redacted with red on its own, and
// the budgets are measured over that (X14's rule); openTools then redacts the
// whole description once more, which catches a key spelled across two fields
// or across a field and the words around it — at the price, as in the
// system prompt's extras, that such a key can grow a section a little past its
// budget.
func agentTail(types []tool.Persona, offered []string, table *modeltable.Table, getenv func(string) string, red *redact.Replacer) string {
	return renderAgentTypes(types, offered, red) + "\n" + renderSubagentModels(table, getenv, red)
}

// renderAgentTypes is the types section: a row per type, in the list's order —
// resolution's precedence, so what is listed first is what a name resolves to
// — within maxTypesSection, ending "… and N more" when cut.
func renderAgentTypes(types []tool.Persona, offered []string, red *redact.Replacer) string {
	rows := make([]string, 0, len(types))
	hidden := 0
	for _, p := range types {
		if r := typeRow(p, offered, red); r != "" {
			rows = append(rows, r)
		} else {
			hidden++
		}
	}
	return agentTypesHeading + fitRows(rows, hidden, maxTypesSection-len(agentTypesHeading))
}

// typeRow is one type's row, "- name: description (tools: a, b)", within
// maxTailRow bytes: a description too long for it is cut, with an ellipsis,
// and the name and the tools are kept whole, since the name is what the model
// sends back and the tools are what the type is for. A row whose name and
// tools alone are too long is "", counted in the section's "… and N more": a
// name the model could not read whole is one it could not send.
func typeRow(p tool.Persona, offered []string, red *redact.Replacer) string {
	all, ids := childToolSet(p, offered)
	if all {
		ids = offered
	}
	tools := "none"
	if len(ids) > 0 {
		tools = strings.Join(ids, ", ")
	}
	head := "- " + red.String(foldLine(p.Name))
	tail := " (tools: " + red.String(tools) + ")\n"
	room := maxTailRow - len(head) - len(tail)
	if room < 0 {
		return ""
	}
	// ": " and at least a character before the ellipsis, or nothing.
	desc := red.String(foldLine(p.Description))
	if desc == "" || room < len(": ")+len("…")+1 {
		return head + tail
	}
	return head + ": " + oneLine(desc, room-len(": ")) + tail
}

// renderSubagentModels is the models section: a row per model a child can
// run on (modelRow), in subagentModelOrder, within maxModelsSection and ending
// "… and N more" when cut, then the tier line (tiersLine), which the rows'
// budget makes room for first.
//
// Only a model whose provider has a key now is listed, and only a tier mapped
// to one: a call naming any other fails with its provider named (§3.6), and
// listing it would spend the budget offering the model a choice it cannot
// make. The list is fixed at Open like the rest of the tools; a key exported
// later is used when a call names the model anyway, since resolution reads the
// environment at the call.
func renderSubagentModels(table *modeltable.Table, getenv func(string) string, red *redact.Replacer) string {
	usable := func(alias string) bool {
		_, err := table.Resolve(alias, getenv)
		return err == nil
	}
	var rows []string
	hidden := 0
	for _, alias := range subagentModelOrder(table) {
		if !usable(alias) {
			continue
		}
		if r := modelRow(alias, table.Models[alias], red); r != "" {
			rows = append(rows, r)
		} else {
			hidden++
		}
	}
	tiers := tiersLine(table, usable, red)
	return modelsHeading + fitRows(rows, hidden, maxModelsSection-len(modelsHeading)-len(tiers)) + tiers
}

// subagentModelOrder is the table's aliases in the order the section lists
// them, which decides what survives a cut: the ones the owner configured for
// children first — [subagents] model, then the tiers' in tierOrder — then the
// table's default, then the rest by alias. Each once.
func subagentModelOrder(table *modeltable.Table) []string {
	out := make([]string, 0, len(table.Models))
	add := func(alias string) {
		if _, ok := table.Models[alias]; ok && !slices.Contains(out, alias) {
			out = append(out, alias)
		}
	}
	add(table.Subagents.Model)
	for _, tier := range tierOrder(table.Subagents.Tiers) {
		add(table.Subagents.Tiers[tier])
	}
	add(table.DefaultModel)
	for _, alias := range table.Aliases() {
		add(alias)
	}
	return out
}

// tierOrder is tiers' names in the order they are listed: modeltable's
// BuiltinTiers first, in their own order — fable, opus, sonnet, haiku, largest
// to smallest — then the owner's own names, sorted.
func tierOrder(tiers map[string]string) []string {
	var out []string
	for _, t := range modeltable.BuiltinTiers {
		if _, ok := tiers[t]; ok {
			out = append(out, t)
		}
	}
	for _, t := range slices.Sorted(maps.Keys(tiers)) {
		if !slices.Contains(modeltable.BuiltinTiers, t) {
			out = append(out, t)
		}
	}
	return out
}

// modelRow is one model's row, "- alias (Name): low, medium, high; default
// high", within maxTailRow bytes: the name, in parentheses, only when it is
// not the alias, and dropped when the row would be too long with it; the
// efforts, and the default, only for a model with effort control. A row too
// long even without its name is "", counted in "… and N more".
func modelRow(alias string, m modeltable.Model, red *redact.Replacer) string {
	shown := red.String(foldLine(alias))
	head := "- " + shown
	tail := "\n"
	if len(m.Efforts) > 0 {
		efforts := make([]string, len(m.Efforts))
		for i, e := range m.Efforts {
			efforts[i] = red.String(foldLine(e))
		}
		tail = ": " + strings.Join(efforts, ", ")
		if m.DefaultEffort != "" {
			tail += "; default " + red.String(foldLine(m.DefaultEffort))
		}
		tail += "\n"
	}
	if name := red.String(foldLine(m.Name)); name != "" && name != shown {
		if row := head + " (" + name + ")" + tail; len(row) <= maxTailRow {
			return row
		}
	}
	if len(head)+len(tail) > maxTailRow {
		return ""
	}
	return head + tail
}

// tiersLine is "Tiers: opus → a, sonnet → b\n": every tier the table maps to a
// model usable reports as having a key, in tierOrder, within maxTiersLine and
// ending ", … and N more" when cut; "" when no tier qualifies. An unmapped
// tier is not listed: it means the parent's own model, which the model
// parameter's description says.
func tiersLine(table *modeltable.Table, usable func(alias string) bool, red *redact.Replacer) string {
	var parts []string
	for _, tier := range tierOrder(table.Subagents.Tiers) {
		if alias := table.Subagents.Tiers[tier]; alias != "" && usable(alias) {
			parts = append(parts, red.String(foldLine(tier))+" → "+red.String(foldLine(alias)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	for keep := len(parts); ; keep-- {
		list := slices.Clone(parts[:keep])
		if keep < len(parts) {
			list = append(list, fmt.Sprintf("… and %d more", len(parts)-keep))
		}
		if line := "Tiers: " + strings.Join(list, ", ") + "\n"; len(line) <= maxTiersLine || keep == 0 {
			return line
		}
	}
}
