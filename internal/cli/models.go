package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/version"
)

// craze models (plan 050 C2): the native provider's models, for a person (a
// table) or a program (--json, one object on one line). Three forms:
//
//   - craze models --shipped: the catalog compiled into this craze alone
//     (modeltable.CatalogTable) — no file, no environment, no account list —
//     so it answers the same under `env -i` on any machine; it is the release's
//     models.json asset.
//   - craze models: this craze directory's table, as a session loads it (the
//     user's files over the catalog, the cached account lists), each row with
//     its machine state: whether a key or sign-in funds it, and whether a
//     picker offers it.
//   - craze models <alias>: one row, the alias read exactly as --model reads
//     it; a miss is exit 1 with the error object a waiting command's
//     pre-check gives, and, under a provider whose models are an account's
//     own when craze has no list of it to read, `unverifiable` beside it.
//
// No form makes a network request, refreshes a list, or prints a key, a base
// URL or a path. Exit 0 whenever it answered; 1 for a miss or a table that
// does not load; 2 for a usage error.

// modelsSchema is the document's `schema`: an integer that only increases.
const modelsSchema = 1

// The status, source, auth and state words of the document.
const (
	modelActive     = "active"
	modelDeprecated = "deprecated"
	sourceCatalog   = "catalog"
	sourceAccount   = "account"
	sourceUser      = "user"
	authByKey       = "key"
	authBySignIn    = "sign-in"
)

type modelsFlags struct {
	json    bool
	shipped bool
}

func newModelsCmd() *cobra.Command {
	f := &modelsFlags{}
	cmd := &cobra.Command{
		Use:   "models [alias]",
		Short: "List the built-in agent's models, or show one",
		Long: "craze models lists the models craze's built-in agent can run on this machine: each alias, " +
			"its provider (the id craze auth login takes), whether it is active or deprecated and what " +
			"replaces it, its efforts and context, and whether a key or sign-in funds it and a picker " +
			"offers it. With an alias, that model alone, read as --model reads it. --shipped answers " +
			"from the catalog compiled into this craze alone, reading no file and no environment. " +
			"--json prints one object.",
		Args: authArgs(1, "takes one argument at most, a model alias"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runModels(cmd, *f, args)
		},
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "print one JSON object")
	cmd.Flags().BoolVar(&f.shipped, "shipped", false, "the catalog compiled into this craze alone: no file, no environment, no account list")
	return cmd
}

// shippedModels says cmd, the command that ran, is craze models --shipped:
// the one form that reads no environment, so the root's check of it
// (paths.CheckEnv) is not made.
func shippedModels(cmd *cobra.Command) bool {
	if cmd.Name() != "models" || cmd.Parent() != cmd.Root() {
		return false
	}
	on, err := cmd.Flags().GetBool("shipped")
	return err == nil && on
}

// ShippedModelsRequest says args, a command line after the program's name, is
// craze models with --shipped, as the flag parser would read it (nothing after
// `--`): main asks before anything else, so that a reparenting spawn's mark
// (hostspawn.Reparent), which turns any craze into a spawn's intermediate,
// cannot make the form that reads no environment fail. No intermediate is
// ever started with this command line.
func ShippedModelsRequest(args []string) bool {
	if len(args) == 0 || args[0] != "models" {
		return false
	}
	on := false
	for _, a := range args[1:] {
		switch {
		case a == "--":
			return on
		case a == "--shipped":
			on = true
		case strings.HasPrefix(a, "--shipped="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--shipped="))
			on = err == nil && v
		}
	}
	return on
}

// modelsDoc is craze models --json.
type modelsDoc struct {
	Schema       int             `json:"schema"`
	CrazeVersion string          `json:"crazeVersion"`
	Models       []modelRow      `json:"models"`
	Retired      []retiredRow    `json:"retired"`
	Providers    []modelProvider `json:"providers"`
}

// modelRow is one model. Every key is always present but replacedBy (a
// deprecated model's alone) and state (the local forms' alone).
type modelRow struct {
	Alias          string      `json:"alias"`
	Provider       string      `json:"provider"`
	Maker          string      `json:"maker"`
	Name           string      `json:"name"`
	Status         string      `json:"status"`
	ReplacedBy     string      `json:"replacedBy,omitempty"`
	Source         string      `json:"source"`
	Efforts        []string    `json:"efforts"`
	DefaultEffort  string      `json:"defaultEffort"`
	Fast           bool        `json:"fast"`
	Vision         bool        `json:"vision"`
	ContextWindow  int         `json:"contextWindow"`
	DefaultContext int         `json:"defaultContext"`
	State          *modelState `json:"state,omitempty"`
}

// modelState is a row's machine state: funded is the model resolving (a key
// or sign-in that funds it, read with no network call), offered its being in
// what a picker lists (modeltable.Table.Offered).
type modelState struct {
	Funded  bool `json:"funded"`
	Offered bool `json:"offered"`
}

// retiredRow is one [[retired]] row: replacedBy when the catalog names one.
type retiredRow struct {
	Alias      string `json:"alias"`
	ReplacedBy string `json:"replacedBy,omitempty"`
}

// modelProvider is one model provider (what craze auth lists, not the agent
// providers craze providers lists).
type modelProvider struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Auth          string `json:"auth"`
	AccountModels string `json:"accountModels"`
}

// modelsMiss is craze models <alias> --json's exit-1 object: the error a
// waiting command's pre-check gives, and unverifiable when craze could not
// check.
type modelsMiss struct {
	Error        *resultError `json:"error"`
	Unverifiable bool         `json:"unverifiable,omitempty"`
}

// modelsWhat names craze models in its lines.
const modelsWhat = "craze models"

// modelsGetenv is the environment the local forms judge funding by. A seam.
var modelsGetenv = os.Getenv

// runModels is craze models (the file's comment).
func runModels(cmd *cobra.Command, f modelsFlags, args []string) error {
	out := cmd.OutOrStdout()
	fail := func(err error) error {
		if f.json {
			_ = writeModelsJSON(out, modelsMiss{Error: kindError(strings.TrimPrefix(err.Error(), modelsWhat+": "), failureKind(err))})
		}
		return err
	}
	t, err := modelsTable(f.shipped)
	if err != nil {
		return fail(kindf(errkind.Config, "%s: %s", modelsWhat, modelsLoadFailure(err, f.shipped)))
	}
	var getenv func(string) string
	if !f.shipped {
		getenv = modelsGetenv
	}
	doc := modelsDocument(t, getenv, version.Version)
	if len(args) == 0 {
		if f.json {
			return writeModelsJSON(out, doc)
		}
		var notes []string
		if getenv != nil {
			notes = append(tableWarnings(t), sourceNotes(t, getenv)...)
		}
		return writeModelsTable(out, doc, notes)
	}
	raw := strings.TrimSpace(args[0])
	alias, err := agent.MatchNativeModel(t, raw)
	if err != nil {
		miss := nativeModelMiss(t, raw, err)
		exit := miss.exit(modelsWhat)
		if f.json {
			e := kindError(sanitizeLine(miss.msg), errkind.Config)
			e.Detail = miss.detail()
			_ = writeModelsJSON(out, modelsMiss{Error: e, Unverifiable: miss.reason == reasonModelUnknown && unverifiable(t, raw)})
		}
		return exit
	}
	i := slices.IndexFunc(doc.Models, func(r modelRow) bool { return r.Alias == alias })
	if f.json {
		return writeModelsJSON(out, doc.Models[i])
	}
	return writeModelsTable(out, modelsDoc{Models: doc.Models[i : i+1]}, nil)
}

// modelsTable is the table a form reads: the catalog alone, or this craze
// directory's (loadNativeTable).
func modelsTable(shipped bool) (*modeltable.Table, error) {
	if !shipped {
		return loadNativeTable()
	}
	cat, err := modelsCatalog()
	if err != nil {
		return nil, err
	}
	return modeltable.CatalogTable(cat)
}

// modelsLoadFailure is what a table that does not load is called (craze
// providers' rule, loadErrFile): the file it is about and where to see why,
// never the error's text, which can quote a key pasted into the file.
func modelsLoadFailure(err error, shipped bool) string {
	switch {
	case shipped:
		return "the catalog compiled into this craze does not load"
	case errors.Is(err, errNoCrazeDir):
		return errNoCrazeDir.Error()
	}
	return loadErrFile(err, paths.NativeDir()) + ` could not be read; run "craze auth list" for the error`
}

// unverifiable says raw, which t has no model for, is under a provider whose
// models are an account's own (its "<provider id>/" prefix, as --model reads
// it) while t has no list of that account to read: craze could not check.
func unverifiable(t *modeltable.Table, raw string) bool {
	head, _, ok := strings.Cut(raw, "/")
	if !ok {
		return false
	}
	head = strings.ToLower(strings.TrimSpace(head))
	for _, id := range slices.Sorted(maps.Keys(t.Providers)) {
		if strings.ToLower(id) == head {
			return t.AccountModels(id) != modeltable.AccountModelsNone && !t.AccountListRead(id)
		}
	}
	return false
}

// modelsDocument is t as the document: rows sorted by alias, the retired rows
// by alias, the providers by id. getenv judges each row's state; nil leaves
// state out (the shipped form).
func modelsDocument(t *modeltable.Table, getenv func(string) string, crazeVersion string) modelsDoc {
	doc := modelsDoc{Schema: modelsSchema, CrazeVersion: crazeVersion,
		Models: []modelRow{}, Retired: []retiredRow{}, Providers: []modelProvider{}}
	var offered map[string]bool
	if getenv != nil {
		offered = t.Offered(getenv)
	}
	for _, alias := range t.Aliases() {
		doc.Models = append(doc.Models, modelRowOf(t, alias, getenv, offered))
	}
	for _, r := range t.RetiredRows() {
		doc.Retired = append(doc.Retired, retiredRow{Alias: r.Alias, ReplacedBy: r.ReplacedBy})
	}
	for _, id := range slices.Sorted(maps.Keys(t.Providers)) {
		p := t.Providers[id]
		auth := authByKey
		if p.Driver == modeltable.DriverChatGPT {
			auth = authBySignIn
		}
		doc.Providers = append(doc.Providers, modelProvider{ID: id, Name: sanitizeLine(t.ProviderName(id)), Auth: auth, AccountModels: t.AccountModels(id)})
	}
	return doc
}

// modelRowOf is alias's row: its state judged by getenv and offered, none
// when getenv is nil.
func modelRowOf(t *modeltable.Table, alias string, getenv func(string) string, offered map[string]bool) modelRow {
	m := t.Models[alias]
	row := modelRow{
		Alias: alias, Provider: m.Provider, Maker: m.Maker, Name: sanitizeLine(cmp.Or(m.Name, alias)), Status: modelActive,
		Source: modelSource(t, alias), Efforts: append([]string{}, m.Efforts...), DefaultEffort: m.DefaultEffort,
		Vision: m.Vision, ContextWindow: m.ContextWindow,
	}
	if m.Deprecated {
		row.Status, row.ReplacedBy = modelDeprecated, m.ReplacedBy
	}
	if m.DefaultContext > 0 && m.DefaultContext < m.ContextWindow {
		row.DefaultContext = m.DefaultContext
	}
	if getenv != nil {
		_, err := t.Resolve(alias, getenv)
		row.State = &modelState{Funded: err == nil, Offered: offered[alias]}
	}
	return row
}

// modelSource is where alias comes from: an account's own list, the catalog
// (the user's overrides of a shipped model included), or the user's files.
func modelSource(t *modeltable.Table, alias string) string {
	if t.FromAccount(alias) {
		return sourceAccount
	}
	switch t.ModelOrigin(alias) {
	case modeltable.OriginShipped, modeltable.OriginOverridden, modeltable.OriginDiscovered:
		return sourceCatalog
	}
	return sourceUser
}

// writeModelsJSON writes v as one object on one line.
func writeModelsJSON(out io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err == nil {
		_, err = fmt.Fprintf(out, "%s\n", b)
	}
	if err != nil {
		return exitf(1, "%s: %s", modelsWhat, sanitizeLine(err.Error()))
	}
	return nil
}

// writeModelsTable writes doc as craze models' table: a row a model — with
// its state, when the rows have one — then a line a retired alias, then a
// "note: " line each of notes (sourceNotes).
func writeModelsTable(out io.Writer, doc modelsDoc, notes []string) error {
	local := len(doc.Models) > 0 && doc.Models[0].State != nil
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "ALIAS\tPROVIDER\tSTATUS\tEFFORTS\tCONTEXT"
	if local {
		header += "\tFUNDED\tOFFERED"
	}
	fmt.Fprintln(tw, header)
	for _, r := range doc.Models {
		status := r.Status
		if r.ReplacedBy != "" {
			status += " (use " + r.ReplacedBy + ")"
		}
		cells := []string{r.Alias, r.Provider, status, cmp.Or(strings.Join(r.Efforts, ","), psNone), contextCell(r)}
		if r.State != nil {
			cells = append(cells, yesNo(r.State.Funded), yesNo(r.State.Offered))
		}
		for i, c := range cells {
			cells[i] = sanitizeLine(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return exitf(1, "%s: %v", modelsWhat, err)
	}
	var lines []string
	for _, r := range doc.Retired {
		line := "retired: " + r.Alias
		if r.ReplacedBy != "" {
			line += " (use " + r.ReplacedBy + ")"
		}
		lines = append(lines, sanitizeLine(line))
	}
	for _, line := range notes {
		lines = append(lines, "note: "+line)
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return exitf(1, "%s: %v", modelsWhat, err)
		}
	}
	return nil
}

// tableWarnings are the warnings a session prints when this craze
// directory's table loads (an unknown name in source_order, an entry the
// catalog dropped, …), as craze auth list notes them, each on one line with
// the craze directory's path left out — craze models prints no path — so a
// file is named by its base name.
func tableWarnings(t *modeltable.Table) []string {
	dir := paths.NativeDir()
	out := make([]string, 0, len(t.Warnings))
	for _, w := range t.Warnings {
		if dir != "" {
			w = strings.ReplaceAll(w, dir+string(os.PathSeparator), "")
		}
		out = append(out, sanitizeLine(strings.TrimPrefix(w, "modeltable: ")))
	}
	return out
}

// sourceNotes is one line for each maker whose funded models the source
// order hides now (plan 050 C5, modeltable.Table.HiddenSources): which source
// its models come from, and which others' are not listed, with how many — so
// a person who sees fewer rows than they have keys for knows why, that each
// still runs by name, and where the order is changed. craze auth list and
// craze models' table print it; --json has each row's state.offered instead.
func sourceNotes(t *modeltable.Table, getenv func(string) string) []string {
	name := func(id string) string { return sanitizeLine(t.ProviderName(id)) }
	var out []string
	for _, ms := range t.HiddenSources(getenv) {
		hidden := make([]string, 0, len(ms.Hidden))
		total := 0
		for i, h := range ms.Hidden {
			total += h.Count
			switch {
			case i > 0:
				hidden = append(hidden, fmt.Sprintf("%d of %s", h.Count, name(h.Provider)))
			case h.Count == 1:
				hidden = append(hidden, "1 model of "+name(h.Provider))
			default:
				hidden = append(hidden, fmt.Sprintf("%d models of %s", h.Count, name(h.Provider)))
			}
		}
		verb := "are not listed and still run"
		if total == 1 {
			verb = "is not listed and still runs"
		}
		out = append(out, fmt.Sprintf("%s models come from %s; %s %s by name (source_order in models.toml changes the order)",
			sanitizeLine(modeltable.MakerName(ms.Maker)), name(ms.Winner), strings.Join(hidden, " and "), verb))
	}
	return out
}

// contextCell is a row's context as the table says it: the context a session
// works within, and the window when that is smaller; "-" when unknown.
func contextCell(r modelRow) string {
	switch {
	case r.ContextWindow == 0:
		return psNone
	case r.DefaultContext > 0:
		return strconv.Itoa(r.DefaultContext) + " of " + strconv.Itoa(r.ContextWindow)
	}
	return strconv.Itoa(r.ContextWindow)
}

// yesNo is a state cell.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
