// Package modeltable is the native harness's model catalog: two small TOML
// files in the harness's directory. providers.toml holds endpoints, the names
// of the environment variables that carry keys, and optional inline keys — it
// is the only place a secret lives, and it is kept 0600. models.toml holds
// aliases, wire model ids, limits and efforts — nothing secret, 0644, safe to
// paste. The split keeps the file the owner edits most free of keys (plan 018
// §3.2, §3.3).
//
// Load reads and validates both into one Table; Resolve turns an alias into
// everything the llm factory needs, key included; Save writes a Table back,
// which is what `craze import gx` does after merging (package gximport).
//
// Both files are decoded strictly: a key the schema does not have is a load
// error naming the file, the table and the key, because models.toml is edited
// by hand and a typo would otherwise be silently ignored. Nothing this package
// returns — no error, warning or formatted value — carries a key: a key
// travels as a Secret, and a TOML syntax error is reported by file and line
// only.
package modeltable

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/harness/redact"
)

const (
	// Version is the one schema version this package reads and writes. Each
	// file carries its own `version`, so the two can move independently.
	Version = 1

	// ProvidersFile and ModelsFile are the two files' names in the directory.
	ProvidersFile = "providers.toml"
	ModelsFile    = "models.toml"

	// DriverOpenAICompat and DriverOpenRouter are the two provider drivers:
	// any Chat Completions endpoint at a base URL, and OpenRouter, whose
	// client has a fixed endpoint and so takes no base URL.
	DriverOpenAICompat = "openai-compat"
	DriverOpenRouter   = "openrouter"

	// SourceGX marks an entry `craze import gx` owns: a re-import replaces it
	// wholesale. SourceManual marks one the owner does; an entry with no
	// source loads as manual. Any other source is accepted verbatim and, like
	// manual, never touched by an import — the safe reading of a typo.
	SourceGX     = "gx"
	SourceManual = "manual"

	// MinKeyLen is the shortest API key craze accepts, in bytes, after
	// surrounding whitespace is trimmed. No real provider issues a shorter
	// one, so it is a placeholder or a typo; and every key is redacted from
	// tool output wherever it appears, so a key like "a" or "error" would
	// shred ordinary text (plan 019 §3.8). Load refuses an inline one, Keys
	// one from the environment, and the llm factory any it is handed. The
	// same two places refuse a key the redaction marker could print back
	// (ErrKeyOverlapsMarker).
	MinKeyLen = 8
)

// toolProfiles are the names a model's tool_profile may give, the default
// first. The native harness registers the profiles themselves (package
// internal/harness/tool/opencode); the catalog keeps its own copy of their
// names, so loading models.toml — which `craze import gx` does too — links no
// tool code. A test pins the copy to the profiles.
var toolProfiles = []string{"opencode"}

// ToolProfiles returns the names a model's tool_profile may give, the
// default first.
func ToolProfiles() []string { return slices.Clone(toolProfiles) }

// The modes Save writes: the directory and the key file private, the model
// file shareable.
const (
	dirPerm       os.FileMode = 0o700
	providersPerm os.FileMode = 0o600
	modelsPerm    os.FileMode = 0o644
)

// Table is the whole catalog: both files, decoded and validated.
type Table struct {
	// DefaultModel is the alias a session starts on when none is asked for.
	// It is required because TOML tables decode into Go maps, which have no
	// order to take "the first model" from.
	DefaultModel string
	// Providers is keyed by provider id, Models by alias (the key users
	// select, which may differ from the wire id).
	Providers map[string]Provider
	Models    map[string]Model
	// Warnings are problems Load fixed on its own, for the caller to print:
	// today only a providers.toml found readable by others and tightened. A
	// warning names a path and a mode, never file contents. Save ignores it.
	Warnings []string
}

// Provider is one entry of providers.toml.
type Provider struct {
	Driver  string
	BaseURL string // required for openai-compat, absent for openrouter
	// EnvKeys are environment variable names, tried in order; the first one
	// set to a non-empty value is the key.
	EnvKeys []string
	// APIKey is the inline fallback when no EnvKeys variable is set.
	APIKey Secret
	Source string
}

// Model is one entry of models.toml.
type Model struct {
	Provider        string // a key of Table.Providers
	WireModel       string // the model id the provider's API expects
	Name            string // display name; "" shows the alias
	ContextWindow   int    // tokens; 0 = unknown
	MaxOutputTokens int    // 0 = absent: no output ceiling is sent
	// Efforts are the reasoning-effort levels the model accepts, in display
	// order. Empty means the model has no effort control: none is shown or
	// sent.
	Efforts       []string
	DefaultEffort string // "" or one of Efforts
	Vision        bool
	// ToolProfile names the tool profile — tools and system prompt — a
	// session started on this model gets (plan 019 §3.1, Seam 2). "" is the
	// default. Load refuses a name not in ToolProfiles. An import never
	// writes it, so on a gx-sourced model it lasts only until the next
	// import; source = "manual" keeps it.
	ToolProfile string
	Source      string
}

// Resolved is everything the llm factory needs to build one model's client.
type Resolved struct {
	Alias           string
	ProviderID      string
	Driver          string
	BaseURL         string
	APIKey          Secret
	WireModel       string
	Name            string // the model's name, or its alias when it has none
	ContextWindow   int
	MaxOutputTokens int
	Efforts         []string
	DefaultEffort   string
	Vision          bool
	ToolProfile     string // "" is the default profile
}

// The on-disk shapes. They are separate from the public types so the key is a
// plain string only here, at the file boundary, and a Secret everywhere else;
// encoding a Secret would write "[redacted]" into providers.toml.
//
// The map fields are omitempty only so encodeFile can encode a doc with a nil
// map as the file's top-level keys; decoding ignores the option.
type providersDoc struct {
	Version   int                      `toml:"version"`
	Providers map[string]providerEntry `toml:"providers,omitempty"`
}

type providerEntry struct {
	Driver  string   `toml:"driver"`
	BaseURL string   `toml:"base_url,omitempty"`
	EnvKeys []string `toml:"env_keys,omitempty"`
	APIKey  string   `toml:"api_key,omitempty"`
	Source  string   `toml:"source,omitempty"`
}

type modelsDoc struct {
	Version      int                   `toml:"version"`
	DefaultModel string                `toml:"default_model"`
	Models       map[string]modelEntry `toml:"models,omitempty"`
}

type modelEntry struct {
	Provider        string   `toml:"provider"`
	WireModel       string   `toml:"wire_model"`
	Name            string   `toml:"name,omitempty"`
	ContextWindow   int      `toml:"context_window,omitzero"`
	MaxOutputTokens int      `toml:"max_output_tokens,omitzero"`
	Efforts         []string `toml:"efforts,omitempty"`
	DefaultEffort   string   `toml:"default_effort,omitempty"`
	Vision          bool     `toml:"vision,omitempty"`
	ToolProfile     string   `toml:"tool_profile,omitempty"`
	Source          string   `toml:"source,omitempty"`
}

func (d *providersDoc) version() int { return d.Version }
func (d *modelsDoc) version() int    { return d.Version }

// Headers written above each file's body. Both files are machine-rewritten on
// every import, so a comment added by hand would be lost; the header says so
// and names the one rule that does keep a hand edit.
const (
	providersHeader = `# craze native harness: provider endpoints and API keys.
#
# This file is machine-rewritten by ` + "`craze import gx`" + `: comments and key order
# are not preserved. An import replaces every entry whose source is "gx"; to
# keep a hand edit, set that entry's source = "manual". API keys live only
# here: keep this file 0600 and never paste it anywhere.

`
	modelsHeader = `# craze native harness: model aliases, wire ids, limits and efforts.
#
# This file is machine-rewritten by ` + "`craze import gx`" + `: comments and key order
# are not preserved. An import replaces every entry whose source is "gx"; to
# keep a hand edit, set that entry's source = "manual". No secrets live here
# (API keys are in providers.toml), so it is safe to share.

`
)

// Load reads providers.toml and models.toml from dir and returns one
// validated Table. When dir holds neither file the error matches both
// ErrNotConfigured and fs.ErrNotExist; when it holds only one, the error
// names the missing one and matches fs.ErrNotExist alone, because running an
// import over the survivor would drop its hand-made entries.
//
// A providers.toml readable by group or others is tightened to 0600 before it
// is read, and the fix is reported in Table.Warnings.
func Load(dir string) (*Table, error) {
	return load(dir, false)
}

// LoadForImport is Load for `craze import gx`, the one caller that merges into
// what is on disk and then rewrites both files. It differs from Load in one
// case: when exactly one file exists — which an interrupted first Save leaves
// behind — the missing one reads as empty instead of failing, so the import
// can merge into the survivor, keep its hand-made entries, and write a
// complete pair. The survivor is still decoded strictly and validated on its
// own; only the checks that need the missing file (a model's provider, or
// default_model) are skipped, and a warning names the missing file. The table
// it returns can then be invalid on its own, which is fine for its one use:
// the merge's result is validated in full before it is saved.
//
// When neither file exists it returns nil and no error: there is nothing to
// merge into. When both exist it is exactly Load.
func LoadForImport(dir string) (*Table, error) {
	return load(dir, true)
}

func load(dir string, forImport bool) (*Table, error) {
	if dir == "" {
		// paths.NativeDir is "" when there is no home directory; joining ""
		// would read providers.toml from the working directory instead.
		return nil, errors.New("modeltable: no directory to load from")
	}
	ppath := filepath.Join(dir, ProvidersFile)
	mpath := filepath.Join(dir, ModelsFile)

	var warnings []string
	if w := tighten(ppath); w != "" {
		warnings = append(warnings, w)
	}
	pb, perr := os.ReadFile(ppath)
	mb, merr := os.ReadFile(mpath)
	pMissing, mMissing := errors.Is(perr, fs.ErrNotExist), errors.Is(merr, fs.ErrNotExist)
	switch {
	case pMissing && mMissing && forImport:
		return nil, nil
	case pMissing && mMissing:
		return nil, fmt.Errorf("%w: %s holds neither %s nor %s (%w)",
			ErrNotConfigured, dir, ProvidersFile, ModelsFile, fs.ErrNotExist)
	case pMissing && forImport:
		warnings = append(warnings, fmt.Sprintf("modeltable: %s is missing; importing as if it were empty", ppath))
	case mMissing && forImport:
		warnings = append(warnings, fmt.Sprintf("modeltable: %s is missing; importing as if it were empty", mpath))
	case pMissing:
		return nil, fmt.Errorf("modeltable: %s is missing although %s exists (%w)", ppath, ModelsFile, fs.ErrNotExist)
	case mMissing:
		return nil, fmt.Errorf("modeltable: %s is missing although %s exists (%w)", mpath, ProvidersFile, fs.ErrNotExist)
	}
	if perr != nil && !pMissing {
		return nil, fmt.Errorf("modeltable: %w", perr)
	}
	if merr != nil && !mMissing {
		return nil, fmt.Errorf("modeltable: %w", merr)
	}

	var pd providersDoc
	if !pMissing {
		if err := decodeStrict(ppath, pb, &pd); err != nil {
			return nil, err
		}
	}
	var md modelsDoc
	if !mMissing {
		if err := decodeStrict(mpath, mb, &md); err != nil {
			return nil, err
		}
	}

	t := &Table{
		DefaultModel: md.DefaultModel,
		Providers:    make(map[string]Provider, len(pd.Providers)),
		Models:       make(map[string]Model, len(md.Models)),
		Warnings:     warnings,
	}
	for id, e := range pd.Providers {
		t.Providers[id] = Provider{
			Driver:  e.Driver,
			BaseURL: e.BaseURL,
			EnvKeys: nilIfEmpty(e.EnvKeys),
			APIKey:  Secret(e.APIKey),
			Source:  sourceOrManual(e.Source),
		}
	}
	for alias, e := range md.Models {
		m := Model(e) // see encodeModels
		m.Efforts = nilIfEmpty(m.Efforts)
		m.Source = sourceOrManual(m.Source)
		t.Models[alias] = m
	}
	if err := validate(t, ppath, mpath, crossFile{providers: !pMissing, models: !mMissing}); err != nil {
		return nil, err
	}
	return t, nil
}

// decodeStrict decodes one file into doc, then checks, in this order, that it
// declares the version this package reads and that it has no key the schema
// lacks. The version comes first so a file from a newer craze says "newer
// version" rather than "unknown key" about whatever that version added.
func decodeStrict(path string, data []byte, doc interface{ version() int }) error {
	md, err := toml.Decode(string(data), doc)
	if err != nil {
		return decodeError(path, err)
	}
	if !md.IsDefined("version") {
		return &FileError{File: path, Key: "version", Reason: fmt.Sprintf("missing: want version = %d", Version)}
	}
	if v := doc.version(); v != Version {
		return &FileError{File: path, Key: "version",
			Reason: fmt.Sprintf("unsupported version %d: this craze reads version %d", v, Version)}
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return unknownKey(path, undecoded[0])
	}
	return nil
}

// tighten makes a providers.toml that group or others can read private, and
// says so. A file that is missing, not regular, or already private is left
// alone. A chmod that fails is itself a warning, not a load error: the models
// still work, and the owner is told the file is exposed.
//
// A symlink is never followed for the chmod: its target can be anywhere, and
// changing the mode of a file the owner placed elsewhere on purpose is not
// this package's call. It is reported instead, and the load carries on.
func tighten(path string) (warning string) {
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Sprintf("modeltable: %s is a symlink; permissions not changed (it holds API keys: keep what it points to 0600)", path)
	}
	if !info.Mode().IsRegular() {
		return ""
	}
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return ""
	}
	if err := os.Chmod(path, providersPerm); err != nil {
		return fmt.Sprintf("modeltable: %s holds API keys and is mode %04o; tightening it to 0600 failed: %v", path, perm, err)
	}
	return fmt.Sprintf("modeltable: %s holds API keys and was mode %04o; tightened it to 0600", path, perm)
}

// Save writes t to dir as the two files, each atomically: dir 0700,
// providers.toml 0600, models.toml 0644. An invalid table is refused before
// anything is written.
//
// providers.toml is written first, because it is the order whose
// interruption leaves something loadable. An import never deletes a
// provider, so if the second write never happens the old models.toml still
// names only providers the new providers.toml has. On a first import there
// is no old models.toml, and the lone providers.toml is what LoadForImport
// reads so that re-running the import completes the pair.
//
// Each file is replaced by renaming a temporary file over its path, so a
// providers.toml that is a symlink is replaced by a regular 0600 file; the
// file it pointed to is not written through, and is left as it was.
func Save(dir string, t *Table) error {
	if dir == "" {
		return errors.New("modeltable: no directory to save to")
	}
	if err := t.Validate(); err != nil {
		return err
	}
	pb, err := t.encodeProviders()
	if err != nil {
		return err
	}
	mb, err := t.encodeModels()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone; this one is the
	// harness's own and holds the key file.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(dir, ProvidersFile), pb, providersPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(dir, ModelsFile), mb, modelsPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	return nil
}

func (t *Table) encodeProviders() ([]byte, error) {
	entries := make(map[string]providerEntry, len(t.Providers))
	for id, p := range t.Providers {
		entries[id] = providerEntry{
			Driver:  p.Driver,
			BaseURL: p.BaseURL,
			EnvKeys: p.EnvKeys,
			APIKey:  p.APIKey.Reveal(), // the one place a key is written: its own file
			Source:  p.Source,
		}
	}
	return encodeFile(providersHeader, &providersDoc{Version: Version}, "providers", entries)
}

func (t *Table) encodeModels() ([]byte, error) {
	entries := make(map[string]modelEntry, len(t.Models))
	for alias, m := range t.Models {
		// A conversion, so a field added to Model and not to modelEntry (or
		// the reverse) is a compile error rather than a field Save drops.
		entries[alias] = modelEntry(m)
	}
	return encodeFile(modelsHeader, &modelsDoc{Version: Version, DefaultModel: t.DefaultModel}, "models", entries)
}

// encodeFile renders header, then top (a doc whose map is nil: the top-level
// keys), then one [table."id"] section per entry in sorted order, each after
// a blank line. The encoder's own rendering of the map would print an empty
// [table] header and run the sections together; this reads like a file
// written by hand, and the same table always encodes to the same bytes.
func encodeFile[E any](header string, top any, table string, entries map[string]E) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(top); err != nil {
		return nil, fmt.Errorf("modeltable: encoding: %w", err)
	}
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		fmt.Fprintf(&buf, "\n[%s]\n", toml.Key{table, id})
		if err := toml.NewEncoder(&buf).Encode(entries[id]); err != nil {
			return nil, fmt.Errorf("modeltable: encoding: %w", err)
		}
	}
	return buf.Bytes(), nil
}

// Validate checks t against every rule Load enforces and returns the first
// failure as a *FileError naming the file, table and key. Entries are checked
// in sorted order, so the same table always reports the same problem. The
// file is named by its base name: an in-memory table has no directory. Save
// calls it, so an invalid table is never written.
func (t *Table) Validate() error {
	return validate(t, ProvidersFile, ModelsFile, crossFile{providers: true, models: true})
}

// crossFile says which files were read, and so which checks can hold. A
// model's provider can only be checked when providers.toml was read, and
// default_model only when models.toml was. Every caller but LoadForImport
// reading a lone file has both.
type crossFile struct {
	providers, models bool
}

func validate(t *Table, pfile, mfile string, read crossFile) error {
	for _, id := range slices.Sorted(maps.Keys(t.Providers)) {
		if err := validateProvider(pfile, id, t.Providers[id]); err != nil {
			return err
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(t.Models)) {
		if err := validateModel(mfile, alias, t.Models[alias], t.Providers, read.providers); err != nil {
			return err
		}
	}
	if !read.models {
		return nil
	}
	if t.DefaultModel == "" {
		return &FileError{File: mfile, Key: "default_model", Reason: "missing: name the alias a session starts on"}
	}
	if _, ok := t.Models[t.DefaultModel]; !ok {
		return &FileError{File: mfile, Key: "default_model",
			Reason: fmt.Sprintf("%q is not a model in %s", t.DefaultModel, ModelsFile)}
	}
	return nil
}

func validateProvider(file, id string, p Provider) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: toml.Key{"providers", id}.String(), Key: key, Reason: reason}
	}
	if strings.TrimSpace(id) == "" {
		return at("", "a provider id must not be empty")
	}
	switch p.Driver {
	case DriverOpenAICompat:
		if p.BaseURL == "" {
			return at("base_url", `missing: driver "openai-compat" needs the endpoint's base URL`)
		}
		// The value is not echoed: a base URL can carry a key in its query.
		if !httpURL(p.BaseURL) {
			return at("base_url", "not an absolute http or https URL")
		}
	case DriverOpenRouter:
		if p.BaseURL != "" {
			return at("base_url", `must be absent for driver "openrouter", whose endpoint is fixed`)
		}
	case "":
		return at("driver", `missing: want "openai-compat" or "openrouter"`)
	default:
		return at("driver", fmt.Sprintf(`unknown driver %q: want "openai-compat" or "openrouter"`, p.Driver))
	}
	for i, name := range p.EnvKeys {
		if strings.TrimSpace(name) == "" {
			return at("env_keys", fmt.Sprintf("entry %d is empty", i+1))
		}
	}
	// Every provider's key is checked, not only the ones a session uses:
	// the redactor covers every loaded key, and one it cannot redact could
	// reach a model through a file or a command's output. The reason names
	// the rule, never the value.
	if err := keyProblem(strings.TrimSpace(p.APIKey.Reveal())); err != nil {
		return at("api_key", keyReason(err))
	}
	return nil
}

// keyProblem says why k, already trimmed, cannot be a key craze redacts —
// ErrKeyTooShort or ErrKeyOverlapsMarker, never quoting k — or returns nil,
// as it does for "" (no key).
func keyProblem(k string) error {
	switch {
	case k == "":
		return nil
	case len(k) < MinKeyLen:
		return ErrKeyTooShort
	case redact.MarkerOverlaps(k):
		return ErrKeyOverlapsMarker
	}
	return nil
}

// keyReason is keyProblem's error as a FileError reason. It neither quotes
// the marker nor uses its words ("redacted", "credential"): a key that
// overlaps the marker may be one of them.
func keyReason(err error) string {
	if errors.Is(err, ErrKeyTooShort) {
		return fmt.Sprintf("shorter than %d bytes: too short to be a real key, or to redact from tool output", MinKeyLen)
	}
	return "overlaps craze's redaction marker, which would print the key back in its own place"
}

func validateModel(file, alias string, m Model, providers map[string]Provider, checkProvider bool) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: toml.Key{"models", alias}.String(), Key: key, Reason: reason}
	}
	if strings.TrimSpace(alias) == "" {
		return at("", "a model alias must not be empty")
	}
	if m.Provider == "" {
		return at("provider", "missing: name a provider in "+ProvidersFile)
	}
	if _, ok := providers[m.Provider]; checkProvider && !ok {
		return at("provider", fmt.Sprintf("%q is not a provider in %s", m.Provider, ProvidersFile))
	}
	if strings.TrimSpace(m.WireModel) == "" {
		return at("wire_model", "missing: the model id the provider's API expects")
	}
	if m.ContextWindow < 0 {
		return at("context_window", "must not be negative")
	}
	if m.MaxOutputTokens < 0 {
		return at("max_output_tokens", "must not be negative")
	}
	seen := make(map[string]bool, len(m.Efforts))
	for i, e := range m.Efforts {
		if strings.TrimSpace(e) == "" {
			return at("efforts", fmt.Sprintf("entry %d is empty", i+1))
		}
		if seen[e] {
			return at("efforts", fmt.Sprintf("%q is listed twice", e))
		}
		seen[e] = true
	}
	if m.DefaultEffort != "" && !seen[m.DefaultEffort] {
		return at("default_effort", fmt.Sprintf("%q is not one of efforts", m.DefaultEffort))
	}
	if m.ToolProfile != "" && !slices.Contains(toolProfiles, m.ToolProfile) {
		names := make([]string, len(toolProfiles))
		for i, p := range toolProfiles {
			names[i] = strconv.Quote(p)
		}
		return at("tool_profile", fmt.Sprintf("unknown tool profile %q: want one of %s, or leave it out for the default",
			m.ToolProfile, strings.Join(names, ", ")))
	}
	return nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Resolve returns everything needed to build alias's client. The key is the
// first of the provider's env_keys that getenv reports non-empty (surrounding
// whitespace trimmed), else the inline api_key, else ErrNoAPIKey naming the
// provider and the variable names tried. A nil getenv is os.Getenv; the
// harness injects its own so tests never read the real environment.
//
// A missing key fails only the models that need it: Load accepts a provider
// with no key at all, so one unfunded provider never hides the others.
func (t *Table) Resolve(alias string, getenv func(string) string) (Resolved, error) {
	m, ok := t.Models[alias]
	if !ok {
		return Resolved{}, fmt.Errorf("%w %q", ErrUnknownModel, alias)
	}
	p, ok := t.Providers[m.Provider]
	if !ok {
		// Unreachable for a table from Load or one that passed Validate.
		return Resolved{}, fmt.Errorf("modeltable: model %q names provider %q, which does not exist", alias, m.Provider)
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	key, err := resolveKey(m.Provider, p, getenv)
	if err != nil {
		return Resolved{}, err
	}
	name := m.Name
	if name == "" {
		name = alias
	}
	return Resolved{
		Alias:           alias,
		ProviderID:      m.Provider,
		Driver:          p.Driver,
		BaseURL:         p.BaseURL,
		APIKey:          key,
		WireModel:       m.WireModel,
		Name:            name,
		ContextWindow:   m.ContextWindow,
		MaxOutputTokens: m.MaxOutputTokens,
		Efforts:         slices.Clone(m.Efforts),
		DefaultEffort:   m.DefaultEffort,
		Vision:          m.Vision,
		ToolProfile:     m.ToolProfile,
	}, nil
}

func resolveKey(id string, p Provider, getenv func(string) string) (Secret, error) {
	for _, name := range p.EnvKeys {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return Secret(v), nil
		}
	}
	if v := strings.TrimSpace(p.APIKey.Reveal()); v != "" {
		return Secret(v), nil
	}
	return "", noAPIKey(id, p.EnvKeys)
}

// Keys returns every key the table knows of, for the redactor that keeps
// them out of tool output (plan 019 §3.8): for every provider, used or not,
// the value of each of its env_keys variables that getenv reports set — all
// of them, not only the first, which is the one Resolve picks — and its
// inline api_key, each with surrounding whitespace trimmed. A nil getenv is
// os.Getenv.
//
// A value the redactor cannot handle fails, naming the provider and the
// variable, never the value: under MinKeyLen bytes is ErrKeyTooShort, and
// one that overlaps the redaction marker is ErrKeyOverlapsMarker. Load
// already refuses such an inline key, but it reads no environment — a table
// loads the same whatever is exported, and `craze import gx` never depends
// on it — so one in an env var is caught here, when a session opens, for
// every provider.
func (t *Table) Keys(getenv func(string) string) ([]Secret, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var keys []Secret
	for _, id := range slices.Sorted(maps.Keys(t.Providers)) {
		p := t.Providers[id]
		for _, name := range p.EnvKeys {
			v := strings.TrimSpace(getenv(name))
			if err := keyProblem(v); err != nil {
				return nil, fmt.Errorf("%w: provider %q: the value of %s", err, id, name)
			}
			if v != "" {
				keys = append(keys, Secret(v))
			}
		}
		v := strings.TrimSpace(p.APIKey.Reveal())
		if err := keyProblem(v); err != nil { // a table built in memory, not loaded
			return nil, fmt.Errorf("%w: provider %q: its api_key in %s", err, id, ProvidersFile)
		}
		if v != "" {
			keys = append(keys, Secret(v))
		}
	}
	return keys, nil
}

// Aliases returns every model alias, sorted.
func (t *Table) Aliases() []string {
	return slices.Sorted(maps.Keys(t.Models))
}

func sourceOrManual(s string) string {
	if s == "" {
		return SourceManual
	}
	return s
}

// nilIfEmpty folds a present-but-empty TOML array into nil, so a Table reads
// the same whether a file wrote `efforts = []` or left the key out.
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
