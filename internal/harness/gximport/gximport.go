// Package gximport turns gx's model configuration into the native harness's
// model table. It reads <grok home>/config.toml, layers <grok home>/providers.toml
// over it exactly as gx does, keeps the providers and models the harness can
// drive, and merges them into an existing table under the source rule (plan
// 018 §3.3). It is pure apart from reading those two files: it writes nothing
// — the caller saves the result with modeltable.Save — and never reads the
// environment, so nothing at runtime depends on gx, which the owner may
// retire.
//
// gx's files are not strict: a key the importer does not use is ignored, as
// gx ignores it. What the importer cannot carry over faithfully — a
// Responses backend, an auth helper, extra headers — is skipped with a
// reason in the Report rather than imported half-working. Nothing in a
// Report or an error carries a key value.
package gximport

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/charliek/craze/internal/harness/modeltable"
)

const (
	// ConfigFile and ProvidersFile are gx's two files under its home.
	ConfigFile    = "config.toml"
	ProvidersFile = "providers.toml"
)

// layerTables are the only top-level tables gx takes from providers.toml
// (PROVIDERS_LAYER_TABLES in grok-build's
// crates/codegen/xai-grok-config/src/providers_layer.rs); anything else there
// — a [models] default, say — gx ignores, and so does the importer.
var layerTables = []string{"model", "model_providers", "auth_provider"}

// modelConnectionKeys are a gx model's per-model overrides of its provider's
// connection. The harness connects per provider, so a model that sets one is
// skipped rather than imported talking to the wrong endpoint or with the
// wrong key.
var modelConnectionKeys = []string{
	"base_url", "api_base_url", "api_key", "env_key", "auth_provider",
	"extra_headers", "query_params", "env_http_headers",
}

// ErrNothingToImport is Import's error when there is no existing table and
// no gx model passed the filters: a table with no models has no default and
// cannot be saved. The Report returned with it still says why each entry was
// skipped.
var ErrNothingToImport = errors.New("gximport: no model could be imported")

// Report says what an import did, per providers and per models. Every list is
// sorted. An id can be both Stale and Skipped: an entry an earlier import
// brought in that gx still has but that no longer passes the filters.
type Report struct {
	Providers Changes
	Models    Changes
	// DefaultModel is the merged table's default_model, and DefaultRule which
	// rule chose it.
	DefaultModel string
	DefaultRule  DefaultRule
}

// Changes sorts one kind of entry (provider ids or model aliases) by what the
// merge did with it.
type Changes struct {
	Added []string // new, source = "gx"
	// Updated were gx-sourced and replaced because their content changed;
	// Unchanged were gx-sourced and re-imported identical. They are separate
	// buckets so a re-import that changed nothing reports nothing changed.
	Updated   []string
	Unchanged []string
	// Kept are every existing entry whose source is not "gx", whether or not
	// gx has an entry by the same name: the owner's entries are never touched.
	Kept []string
	// Stale are gx-sourced entries this import did not produce. They are kept
	// (an import never deletes) and reported so the owner can remove them.
	Stale   []string
	Skipped []Skip
	// Notes are about gx entries that passed the filters but lost a field on
	// the way in, as read from gx (a note stands even when a manual entry of
	// the same name is kept instead).
	Notes []Note
}

// Skip is one gx entry the importer left out, and why. Reason names fields,
// never their values.
type Skip struct {
	ID     string
	Reason string
}

// Note is one gx entry the importer took in without one of its fields, and
// why. Like a Skip's reason, Text names fields, never their values.
type Note struct {
	ID   string
	Text string
}

// noteVarAPIKey is the note for a provider whose inline api_key is a $VAR
// reference: gx expands it at load, an import cannot, and the variable's
// value is a key that belongs in the environment, not in providers.toml.
const noteVarAPIKey = "inline api_key references an env var ($VAR); not imported — set it via env_keys"

// DefaultRule is which of §3.3's default_model rules chose the default.
type DefaultRule string

const (
	// DefaultKept: the existing table's default_model is still a model.
	DefaultKept DefaultRule = "kept"
	// DefaultFromGX: gx's [models] default, because that model was imported.
	DefaultFromGX DefaultRule = "gx"
	// DefaultFirst: the lexicographically first alias.
	DefaultFirst DefaultRule = "first"
)

// Import reads grokHome's two files, layers them, and merges what passes the
// filters into a copy of existing (nil when there is none; it is never
// modified). The result has passed modeltable's validation.
//
// Merge rule, the same for providers and models: an entry whose source is
// "gx" is replaced wholesale or added; an entry with any other source is kept
// untouched; a gx-sourced entry this import did not produce is kept and
// reported stale. Nothing is deleted.
func Import(grokHome string, existing *modeltable.Table) (*modeltable.Table, Report, error) {
	var report Report
	if grokHome == "" {
		return nil, report, errors.New("gximport: no grok home directory to import from")
	}
	cfg, cfgFound, err := readTOML(filepath.Join(grokHome, ConfigFile))
	if err != nil {
		return nil, report, err
	}
	layer, layerFound, err := readTOML(filepath.Join(grokHome, ProvidersFile))
	if err != nil {
		return nil, report, err
	}
	if !cfgFound && !layerFound {
		return nil, report, fmt.Errorf("gximport: %s holds neither %s nor %s (%w)",
			grokHome, ConfigFile, ProvidersFile, fs.ErrNotExist)
	}
	for _, name := range layerTables {
		if over, ok := layer[name].(map[string]any); ok { // gx ignores a non-table too
			cfg[name] = mergeTOML(cfg[name], over)
		}
	}

	providers, providerCtx, providerSkips, providerNotes := importProviders(subtable(cfg, "model_providers"))
	models, modelSkips := importModels(subtable(cfg, "model"), providers, providerCtx, providerSkips)

	merged := clone(existing)
	report.Providers = merge(merged.Providers, providers, func(p modeltable.Provider) string { return p.Source }, sameProvider)
	report.Providers.Skipped = sortedSkips(providerSkips)
	for _, id := range slices.Sorted(maps.Keys(providerNotes)) {
		report.Providers.Notes = append(report.Providers.Notes, Note{ID: id, Text: providerNotes[id]})
	}
	report.Models = merge(merged.Models, models, func(m modeltable.Model) string { return m.Source }, sameModel)
	report.Models.Skipped = sortedSkips(modelSkips)

	if len(merged.Models) == 0 {
		return nil, report, ErrNothingToImport
	}
	gxDefault, _ := subtable(cfg, "models")["default"].(string)
	merged.DefaultModel, report.DefaultRule = chooseDefault(existing, merged, strings.TrimSpace(gxDefault), models)
	report.DefaultModel = merged.DefaultModel

	if err := merged.Validate(); err != nil {
		// Only an invalid existing table (one that did not come from
		// modeltable.Load) can get here: every imported entry is built to pass.
		return nil, report, fmt.Errorf("gximport: the merged table is invalid: %w", err)
	}
	return merged, report, nil
}

// readTOML decodes one gx file into a generic map. A missing file is empty,
// as it is to gx. A syntax error carries the file and line only: BurntSushi's
// ParseError quotes the offending line, and a malformed api_key line is the
// key. (gx skips a providers.toml it cannot parse; an import fails instead,
// because the entries the owner expects would silently be missing.)
func readTOML(path string) (m map[string]any, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("gximport: %w", err)
	}
	m = map[string]any{}
	if _, err := toml.Decode(string(b), &m); err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, false, fmt.Errorf("gximport: %s: line %d: not valid TOML", path, pe.Position.Line)
		}
		// Decoding into a map has no other failure; if one appears, its
		// message is not trusted either.
		return nil, false, fmt.Errorf("gximport: %s: not valid TOML", path)
	}
	return m, true, nil
}

// mergeTOML is gx's deep_merge_toml (xai-grok-config loader.rs): tables merge
// key by key, recursively; anything else — an array included — is replaced
// by the override. base may be modified in place.
func mergeTOML(base, over any) any {
	bt, bok := base.(map[string]any)
	ot, ook := over.(map[string]any)
	if !bok || !ook {
		return over
	}
	for k, v := range ot {
		if cur, ok := bt[k]; ok {
			bt[k] = mergeTOML(cur, v)
		} else {
			bt[k] = v
		}
	}
	return bt
}

// subtable is m[name] when it is a table, else nil (which reads as empty).
func subtable(m map[string]any, name string) map[string]any {
	t, _ := m[name].(map[string]any)
	return t
}

// importProviders keeps each [model_providers.<id>] the harness can drive. It
// also returns each kept provider's context_window, which gx lets its models
// inherit, the reason every other provider was skipped, and a note for each
// kept provider that lost a field on the way in.
func importProviders(raw map[string]any) (out map[string]modeltable.Provider, ctx map[string]int, skips, notes map[string]string) {
	out, ctx = map[string]modeltable.Provider{}, map[string]int{}
	skips, notes = map[string]string{}, map[string]string{}
	for id, v := range raw {
		entry, ok := v.(map[string]any)
		switch {
		case strings.TrimSpace(id) == "":
			skips[id] = "an empty provider id"
			continue
		case !ok:
			skips[id] = "not a table"
			continue
		}
		p, contextWindow, note, reason := importProvider(entry)
		if reason != "" {
			skips[id] = reason
			continue
		}
		out[id], ctx[id] = p, contextWindow
		if note != "" {
			notes[id] = note
		}
	}
	return out, ctx, skips, notes
}

func importProvider(entry map[string]any) (p modeltable.Provider, contextWindow int, note, reason string) {
	skip := func(reason string) (modeltable.Provider, int, string, string) {
		return modeltable.Provider{}, 0, "", reason
	}
	f := fields{t: entry}
	if reason := backendReason(f.str("api_backend")); reason != "" {
		return skip(reason)
	}
	switch {
	case f.set("auth"):
		return skip("auth command providers are not supported (ChatGPT-plan auth is deferred)")
	case f.set("auth_provider"):
		return skip("auth_provider helpers are not supported")
	// An orchestrator decision the plan left open: a provider whose requests
	// carry extra headers is skipped, visibly, rather than imported without
	// them to fail at its first request with an error that never mentions
	// headers. The three below are the same case.
	case f.set("extra_headers"):
		return skip("extra_headers not supported yet")
	case f.set("query_params"):
		return skip("query_params not supported yet")
	case f.set("env_http_headers"):
		return skip("env_http_headers not supported yet")
	case f.set("api_base_url"):
		return skip("api_base_url not supported yet")
	}
	baseURL := strings.TrimSpace(f.str("base_url"))
	apiKey := strings.TrimSpace(f.str("api_key"))
	envKeys := f.strs("env_key")
	contextWindow = f.integer("context_window")
	if f.bad != "" {
		return skip(f.bad + " has the wrong type")
	}
	if baseURL == "" {
		return skip("no base_url")
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return skip("base_url is not an absolute http or https URL")
	}
	// gx expands $VAR and ${VAR} in every string when it loads a file. The
	// importer reads the files, not gx's view of them, and copying a variable
	// reference — or expanding one into providers.toml — would both be wrong.
	// A reference in the endpoint or in a variable name leaves nothing correct
	// to import, so the provider is skipped; one in the inline key only costs
	// the inline key, since the key can come from env_keys instead.
	if strings.Contains(baseURL, "$") {
		return skip("base_url holds a $VAR reference, which gx expands at load and an import cannot")
	}
	if slices.ContainsFunc(envKeys, func(name string) bool { return strings.Contains(name, "$") }) {
		return skip("env_key holds a $VAR reference, which gx expands at load and an import cannot")
	}
	if strings.Contains(apiKey, "$") {
		apiKey, note = "", noteVarAPIKey
	}
	if contextWindow < 0 {
		return skip("context_window is negative")
	}
	p = modeltable.Provider{
		Driver:  modeltable.DriverOpenAICompat,
		BaseURL: baseURL,
		EnvKeys: envKeys,
		APIKey:  modeltable.Secret(apiKey),
		Source:  modeltable.SourceGX,
	}
	if strings.EqualFold(u.Hostname(), "openrouter.ai") {
		if reason := openRouterReason(u); reason != "" {
			return skip(reason)
		}
		p.Driver, p.BaseURL = modeltable.DriverOpenRouter, ""
	}
	return p, contextWindow, note, ""
}

// openRouterReason is why an openrouter.ai base_url cannot become the
// openrouter driver, or "" when it can. That driver has a fixed endpoint and
// no base-URL option, so the URL is dropped on import, and it may only be
// dropped when it says nothing the fixed endpoint does not: https, the
// default port, the path /api/v1 (trailing slash optional), and no userinfo,
// query or fragment. Anything else would be silently lost — userinfo worst of
// all, which is why a skip never stores or echoes it.
func openRouterReason(u *url.URL) string {
	switch {
	case u.Scheme != "https":
		return "an openrouter.ai base_url must be https: the openrouter driver's endpoint is fixed"
	case u.User != nil:
		return "an openrouter.ai base_url with userinfo is not supported: the openrouter driver's endpoint is fixed"
	case u.Port() != "" && u.Port() != "443":
		return "an openrouter.ai base_url with a non-default port is not supported: the openrouter driver's endpoint is fixed"
	case strings.TrimSuffix(u.Path, "/") != "/api/v1":
		return "an openrouter.ai base_url must have the path /api/v1: the openrouter driver's endpoint is fixed"
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "an openrouter.ai base_url with a query or fragment is not supported: the openrouter driver's endpoint is fixed"
	}
	return ""
}

// backendReason is why a provider or model on api_backend b is skipped, or ""
// when b is Chat Completions — which is also gx's default when it is absent
// (ApiBackend in grok-build's xai-grok-sampling-types). An unknown value is
// not echoed.
func backendReason(b string) string {
	switch strings.TrimSpace(b) {
	case "", "chat_completions":
		return ""
	case "responses":
		return "Responses API providers are not supported yet"
	case "messages":
		return "Messages API providers are not supported yet"
	default:
		return "api_backend is not chat_completions"
	}
}

// importModels keeps each [model.<alias>] whose provider was imported.
func importModels(raw map[string]any, providers map[string]modeltable.Provider,
	providerCtx map[string]int, providerSkips map[string]string) (map[string]modeltable.Model, map[string]string) {
	out := map[string]modeltable.Model{}
	skips := map[string]string{}
	for alias, v := range raw {
		entry, ok := v.(map[string]any)
		switch {
		case strings.TrimSpace(alias) == "":
			skips[alias] = "an empty alias"
			continue
		case !ok:
			skips[alias] = "not a table"
			continue
		}
		m, reason := importModel(alias, entry, providers, providerCtx, providerSkips)
		if reason != "" {
			skips[alias] = reason
			continue
		}
		out[alias] = m
	}
	return out, skips
}

func importModel(alias string, entry map[string]any, providers map[string]modeltable.Provider,
	providerCtx map[string]int, providerSkips map[string]string) (modeltable.Model, string) {
	f := fields{t: entry}
	pid := strings.TrimSpace(f.str("model_provider"))
	if f.bad != "" {
		return modeltable.Model{}, f.bad + " has the wrong type"
	}
	if pid == "" {
		return modeltable.Model{}, "no model_provider (a gx built-in model override, not a provider's model)"
	}
	if reason, skipped := providerSkips[pid]; skipped {
		return modeltable.Model{}, fmt.Sprintf("provider %q was skipped: %s", pid, reason)
	}
	if _, ok := providers[pid]; !ok {
		return modeltable.Model{}, fmt.Sprintf("provider %q is not defined", pid)
	}
	if reason := backendReason(f.str("api_backend")); reason != "" {
		return modeltable.Model{}, reason
	}
	for _, key := range modelConnectionKeys {
		if f.set(key) {
			return modeltable.Model{}, fmt.Sprintf("a model-level %s is not supported: set it on the provider", key)
		}
	}
	wire := strings.TrimSpace(f.str("model"))
	if wire == "" {
		wire = alias // gx's own fallback: the table key is the wire id
	}
	// See importProvider: gx would expand it, and the model it names is
	// unknown until then.
	if strings.Contains(wire, "$") {
		return modeltable.Model{}, "model (the wire id) holds a $VAR reference, which gx expands at load and an import cannot"
	}
	contextWindow := f.integer("context_window")
	if _, ok := entry["context_window"]; !ok {
		contextWindow = providerCtx[pid]
	}
	m := modeltable.Model{
		Provider:        pid,
		WireModel:       wire,
		Name:            strings.TrimSpace(f.str("name")),
		ContextWindow:   contextWindow,
		MaxOutputTokens: f.integer("max_completion_tokens"),
		Vision:          f.boolean("supports_vision"),
		Source:          modeltable.SourceGX,
	}
	supports := f.boolean("supports_reasoning_effort")
	effort := strings.TrimSpace(f.str("reasoning_effort"))
	efforts, flagged := f.efforts("reasoning_efforts")
	switch {
	case f.bad != "":
		return modeltable.Model{}, f.bad + " has the wrong type"
	case m.ContextWindow < 0:
		return modeltable.Model{}, "context_window is negative"
	case m.MaxOutputTokens < 0:
		return modeltable.Model{}, "max_completion_tokens is negative"
	}
	// Efforts only when gx says the model takes them (plan 018 §3.3). The
	// default is gx's reasoning_effort when it is one of them, else the
	// entry a table-form list flags default = true, else none; a default the
	// list does not offer is dropped rather than failing validation.
	if supports {
		m.Efforts = efforts
		switch {
		case slices.Contains(efforts, effort):
			m.DefaultEffort = effort
		case flagged != "":
			m.DefaultEffort = flagged
		}
	}
	return m, ""
}

// fields reads typed values out of one gx table. A key the importer uses that
// holds the wrong type is recorded (the first one only) in bad, and the entry
// is then skipped naming the key.
type fields struct {
	t   map[string]any
	bad string
}

func (f *fields) fail(key string) {
	if f.bad == "" {
		f.bad = key
	}
}

func (f *fields) str(key string) string {
	v, ok := f.t[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		f.fail(key)
	}
	return s
}

func (f *fields) integer(key string) int {
	v, ok := f.t[key]
	if !ok {
		return 0
	}
	n, ok := v.(int64)
	if !ok {
		f.fail(key)
	}
	return int(n)
}

func (f *fields) boolean(key string) bool {
	v, ok := f.t[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		f.fail(key)
	}
	return b
}

// strs reads gx's string-or-list shape (env_key): blank names are dropped, as
// gx drops them, and nil means none.
func (f *fields) strs(key string) []string {
	var items []any
	switch v := f.t[key].(type) {
	case nil:
		return nil
	case string:
		items = []any{v}
	case []any:
		items = v
	default:
		f.fail(key)
		return nil
	}
	var out []string
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			f.fail(key)
			return nil
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// efforts reads gx's reasoning_efforts: each entry a bare value ("high") or a
// table with a required value and an optional default flag. Blank and
// repeated values are dropped. flagged is the first entry marked default.
func (f *fields) efforts(key string) (values []string, flagged string) {
	var items []any
	switch v := f.t[key].(type) {
	case nil:
		return nil, ""
	case []any:
		items = v
	case []map[string]any:
		for _, t := range v {
			items = append(items, t)
		}
	default:
		f.fail(key)
		return nil, ""
	}
	seen := map[string]bool{}
	for _, it := range items {
		var value string
		var isDefault bool
		switch it := it.(type) {
		case string:
			value = it
		case map[string]any:
			s, ok := it["value"].(string)
			if !ok {
				f.fail(key)
				return nil, ""
			}
			value = s
			isDefault, _ = it["default"].(bool)
		default:
			f.fail(key)
			return nil, ""
		}
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
		if isDefault && flagged == "" {
			flagged = value
		}
	}
	return values, flagged
}

// set reports whether key holds anything: present and not an empty string,
// table or list. An empty [x.extra_headers] adds no headers, so it is no
// reason to skip.
func (f *fields) set(key string) bool {
	switch v := f.t[key].(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	case []map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

// merge applies the source rule to one kind of entry, adding and replacing
// in dst, and says what it did. Skipped is the caller's to fill.
func merge[T any](dst, imported map[string]T, source func(T) string, same func(a, b T) bool) Changes {
	var c Changes
	for _, id := range slices.Sorted(maps.Keys(dst)) {
		if source(dst[id]) != modeltable.SourceGX {
			c.Kept = append(c.Kept, id)
		} else if _, ok := imported[id]; !ok {
			c.Stale = append(c.Stale, id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(imported)) {
		cur, ok := dst[id]
		switch {
		case !ok:
			dst[id] = imported[id]
			c.Added = append(c.Added, id)
		case source(cur) != modeltable.SourceGX:
			// Listed as kept above: the owner's entry wins over gx's.
		case same(cur, imported[id]):
			c.Unchanged = append(c.Unchanged, id)
		default:
			dst[id] = imported[id]
			c.Updated = append(c.Updated, id)
		}
	}
	return c
}

// sameProvider and sameModel compare whole entries (so a field added later is
// compared without anyone remembering to), with an empty list and an absent
// one counted equal, as they are on disk.
func sameProvider(a, b modeltable.Provider) bool {
	a.EnvKeys, b.EnvKeys = nilIfEmpty(a.EnvKeys), nilIfEmpty(b.EnvKeys)
	return reflect.DeepEqual(a, b)
}

func sameModel(a, b modeltable.Model) bool {
	a.Efforts, b.Efforts = nilIfEmpty(a.Efforts), nilIfEmpty(b.Efforts)
	return reflect.DeepEqual(a, b)
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// clone deep-copies t so the merge never writes through to the caller's
// table. Warnings are not carried: they described the load that produced t.
func clone(t *modeltable.Table) *modeltable.Table {
	out := &modeltable.Table{Providers: map[string]modeltable.Provider{}, Models: map[string]modeltable.Model{}}
	if t == nil {
		return out
	}
	out.DefaultModel = t.DefaultModel
	for id, p := range t.Providers {
		p.EnvKeys = slices.Clone(p.EnvKeys)
		out.Providers[id] = p
	}
	for alias, m := range t.Models {
		m.Efforts = slices.Clone(m.Efforts)
		out.Models[alias] = m
	}
	return out
}

// chooseDefault is §3.3's default_model rule: an existing default that is
// still a model is kept; otherwise gx's [models] default when that model was
// imported; otherwise the lexicographically first alias.
func chooseDefault(existing, merged *modeltable.Table, gxDefault string, imported map[string]modeltable.Model) (string, DefaultRule) {
	if existing != nil && existing.DefaultModel != "" {
		if _, ok := merged.Models[existing.DefaultModel]; ok {
			return existing.DefaultModel, DefaultKept
		}
	}
	if _, ok := imported[gxDefault]; ok && gxDefault != "" {
		return gxDefault, DefaultFromGX
	}
	return merged.Aliases()[0], DefaultFirst
}

func sortedSkips(m map[string]string) []Skip {
	var out []Skip
	for _, id := range slices.Sorted(maps.Keys(m)) {
		out = append(out, Skip{ID: id, Reason: m[id]})
	}
	return out
}
