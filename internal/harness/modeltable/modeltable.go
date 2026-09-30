// Package modeltable is the native harness's model table: the catalog craze
// ships (catalog.toml, compiled into the binary, plan 031 §3.1) with the
// user's two small TOML files in the harness's directory merged over it (plan
// 031 §3.2). providers.toml holds keys and provider overrides — endpoints, the
// names of the environment variables that carry keys, and optional inline keys
// — and is the only place a secret lives, kept 0600. models.toml holds model
// overrides and models the user adds — aliases, wire model ids, limits and
// efforts — nothing secret, 0644, safe to paste. The split keeps the file the
// owner edits most free of keys (plan 018 §3.2, §3.3). Both files are
// optional; `catalog = false` in models.toml makes them the whole table, as
// they were before the catalog existed (P10).
//
// Load reads both and merges them over the catalog into one validated Table;
// Resolve turns an alias into everything the llm factory needs, key included;
// Save writes a Table back (test infrastructure: no product code writes these
// files).
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
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

	// SourceManual is the source of an entry the owner wrote; an entry with no
	// source loads as manual. Any other source string is accepted verbatim, so
	// a file an older craze wrote (source = "gx") still loads — and such an
	// entry yields to the catalog (legacySource, plan 031 §3.3).
	SourceManual = "manual"

	// MinKeyLen is the shortest API key craze accepts, in bytes, after
	// surrounding whitespace is trimmed. No real provider issues a shorter
	// one, so it is a placeholder or a typo; and every key is redacted from
	// tool output wherever it appears, so a key like "a" or "error" would
	// shred ordinary text (plan 019 §3.8). Load refuses an inline one, Keys
	// one from the environment, and the llm factory any it is handed. The
	// same two places refuse a key the redaction marker could print back
	// (ErrKeyOverlapsMarker). A value in the environment that fails either
	// rule is not a key at all: Resolve and Keys skip it, and EnvWarnings says
	// so (plan 031 §3.2).
	MinKeyLen = 8
)

// toolProfiles are the names a model's tool_profile may give, the default
// first. The native harness registers the profiles themselves (package
// internal/harness/tool/opencode); the catalog keeps its own copy of their
// names, so loading models.toml links no tool code. A test pins the copy to
// the profiles.
var toolProfiles = []string{"opencode"}

// ToolProfiles returns the names a model's tool_profile may give, the
// default first.
func ToolProfiles() []string { return slices.Clone(toolProfiles) }

// BuiltinTiers are the Claude-style tier names a sub-agent's `model` and a
// persona's `model:` recognise even with no [subagents.tiers] configured
// (plan 026 §3.6, owner decision 3): an unmapped one resolves to the
// parent's own model, so a persona that says "opus" still works on a machine
// with no tier map. The owner may add more tier names of their own under
// [subagents.tiers]; these four are always recognised, matched
// case-insensitively.
var BuiltinTiers = []string{"fable", "opus", "sonnet", "haiku"}

// The modes Save writes: the directory and the key file private, the model
// file shareable.
const (
	dirPerm       os.FileMode = 0o700
	providersPerm os.FileMode = 0o600
	modelsPerm    os.FileMode = 0o644
)

// Table is the whole model table: the shipped catalog with both files merged
// over it, validated.
type Table struct {
	// DefaultModel is the alias a session starts on when none is asked for:
	// the user's default_model when it names a model, else the catalog's. It
	// is required because TOML tables decode into Go maps, which have no
	// order to take "the first model" from.
	DefaultModel string
	// Providers is keyed by provider id, Models by alias (the key users
	// select, which may differ from the wire id).
	Providers map[string]Provider
	Models    map[string]Model
	// Subagents is models.toml's optional [subagents] section (plan 026
	// §3.6): a sub-agent's configured default model and effort, and what the
	// Claude-style tier names mean in this table. Its zero value is "no
	// section": a child defaults to the parent's own model and effort, and
	// only BuiltinTiers are recognised, each unmapped.
	Subagents Subagents
	// Compaction is models.toml's optional [compaction] section (plan 028
	// §3.6): when the native harness compacts a session's context on its own,
	// and how much of it a compaction keeps verbatim. Its zero value is "no
	// section": every setting at its default.
	Compaction Compaction
	// Warnings are problems Load fixed on its own, for the caller to print: a
	// providers.toml found readable by others and tightened, two priced
	// aliases of one identity that disagree, and each key or entry of the
	// user's files dropped because it could not stand against the merged
	// catalog (plan 031 §3.2). A warning names a path, a table, a key and a
	// reason, never a key's value. Save ignores it.
	Warnings []string
	// NoCatalog is models.toml's `catalog = false` (plan 031 P10): the
	// directory's two files are the whole table, under the rules that held
	// before the catalog existed. Save writes it back.
	NoCatalog bool

	// Where each merged entry came from (plan 031 §3.2), kept beside the
	// entries rather than in them, so Model(e) stays a plain conversion; nil
	// for a table loaded without a catalog or built in memory, whose entries
	// are all the user's. Read through ModelOrigin and ProviderOrigin.
	modelOrigins, providerOrigins map[string]Origin
	// redundant are RedundantOverrides' lines.
	redundant []string
}

// Provider is one provider: shipped, overridden in providers.toml, or the
// user's own.
type Provider struct {
	// Name is the display name ("Fireworks"), "" to show the id. Every
	// shipped provider has one; providers.toml may set or override it.
	Name    string
	Driver  string
	BaseURL string // required for openai-compat, absent for openrouter
	// EnvKeys are environment variable names, tried in order; the first one
	// set to a non-empty value is the key.
	EnvKeys []string
	// APIKey is the inline fallback when no EnvKeys variable is set.
	APIKey Secret
	Source string
}

// Model is one model: shipped, overridden in models.toml, or the user's own.
type Model struct {
	Provider        string // a key of Table.Providers
	WireModel       string // the model id the provider's API expects
	Name            string // display name; "" shows the alias
	ContextWindow   int    // tokens; 0 = unknown
	MaxOutputTokens int    // 0 = absent: Resolve supplies DefaultMaxOutputTokens, or less on a small window
	// Efforts are the reasoning-effort levels the model accepts, in display
	// order. Empty means the model has no effort control: none is shown or
	// sent.
	Efforts       []string
	DefaultEffort string // "" or one of Efforts
	Vision        bool
	// ToolProfile names the tool profile — tools and system prompt — a
	// session started on this model gets (plan 019 §3.1, Seam 2). "" is the
	// default. Load refuses a name not in ToolProfiles.
	ToolProfile string
	Source      string
	// Cost is [models."<alias>".cost] (plan 028 §3.14): nil when the file's
	// entry has no such table, so a model with no cost saves exactly as it
	// did before this section existed (mirrors [compaction]). Rates are
	// dollars per 1,000,000 tokens; a usage's price in picodollars per token
	// comes from Table.Price, not from reading this struct directly.
	Cost *Cost
}

// Cost is one model's [models."<alias>".cost] section (plan 028 §3.14): each
// rate is $ per 1,000,000 tokens, nil when the file leaves that key out —
// every key is independently optional, and there are no tiers (PD21).
// Validate checks a set rate is not negative and not more than
// MaxCostPerMillion. Output carries reasoning too (§2.2): a provider's
// reasoning tokens are billed as output, never split out.
type Cost struct {
	Input      *float64 `toml:"input,omitempty"`
	Output     *float64 `toml:"output,omitempty"`
	CacheRead  *float64 `toml:"cache_read,omitempty"`
	CacheWrite *float64 `toml:"cache_write,omitempty"`
}

// MaxCostPerMillion is the highest rate, in dollars per 1,000,000 tokens,
// Validate accepts. No real provider prices anywhere near this; a rate above
// it is almost certainly a misplaced decimal point or a per-token value
// written as though it were per-million.
const MaxCostPerMillion = 10000.0

// Clone is c with pointers of its own, nil when c is nil: changing the
// clone's rates through its pointers never changes c's (a merge that carries c
// forward must not alias the table it carries it from).
func (c *Cost) Clone() *Cost {
	if c == nil {
		return nil
	}
	return &Cost{Input: clonePtr(c.Input), Output: clonePtr(c.Output),
		CacheRead: clonePtr(c.CacheRead), CacheWrite: clonePtr(c.CacheWrite)}
}

// Subagents is one Table's [subagents] section (plan 026 §3.6): what a
// sub-agent call's `model` and `effort` fall back to when neither the call
// nor its persona names one, and what a tier name resolves to in this table.
// Model and every Tiers value, when set, are aliases in the same Table;
// Validate checks this. The zero value means the section was absent.
type Subagents struct {
	// Model is the alias a child defaults to; "" is the parent's own model.
	Model string
	// Effort is the effort a child defaults to; "" means it is worked out at
	// resolution, from the child's actual model (harness.Session's
	// resolveChildEffort), not fixed here.
	Effort string
	// Tiers maps a tier name (lowercase, [a-z0-9-]+) to an alias. A tier not
	// in this map, including every one of BuiltinTiers by default, resolves
	// to the parent's own model rather than failing. nil means the owner
	// configured none.
	Tiers map[string]string
}

// The [compaction] defaults (plan 028 §3.6, owner decision 1): compact on
// its own, at 85% of the model's context window, keeping a verbatim tail of
// at most 20,000 tokens.
const (
	DefaultCompactionAuto   = true
	DefaultThresholdPercent = 85
	DefaultTailTokens       = 20000
)

// Compaction is one Table's [compaction] section (plan 028 §3.6). Each field
// is nil when the file leaves its key out, which is its default, so a table
// saves exactly the keys its file wrote — none, and no section, for a file
// with none. Read the settings through Auto, ThresholdPercent and TailTokens,
// which apply the defaults. The section applies to every model; a per-model
// override is a follow-up.
type Compaction struct {
	// AutoSet is `auto`: whether a session compacts on its own — before a
	// turn and between its steps — when its context reaches the threshold.
	// Off, only /compact and the recovery from a request the provider
	// refused as too large compact.
	AutoSet *bool
	// ThresholdPercentSet is `threshold_percent`: the share of a model's
	// context window, 1 to 99, at which a session compacts on its own.
	ThresholdPercentSet *int
	// TailTokensSet is `tail_tokens`: the most, in estimated tokens, a
	// compaction keeps of the newest conversation verbatim, whole steps only;
	// 0 keeps none. The harness also caps it at a quarter of the threshold.
	TailTokensSet *int
}

// Auto is `auto`, or DefaultCompactionAuto when the section leaves it out.
func (c Compaction) Auto() bool {
	if c.AutoSet == nil {
		return DefaultCompactionAuto
	}
	return *c.AutoSet
}

// ThresholdPercent is `threshold_percent`, or DefaultThresholdPercent.
func (c Compaction) ThresholdPercent() int {
	if c.ThresholdPercentSet == nil {
		return DefaultThresholdPercent
	}
	return *c.ThresholdPercentSet
}

// TailTokens is `tail_tokens`, or DefaultTailTokens.
func (c Compaction) TailTokens() int {
	if c.TailTokensSet == nil {
		return DefaultTailTokens
	}
	return *c.TailTokensSet
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

type Resolved struct {
	Alias           string
	ProviderID      string
	Driver          string
	BaseURL         string
	APIKey          Secret
	WireModel       string
	Name            string // the model's name, or its alias when it has none
	ContextWindow   int
	MaxOutputTokens int // the ceiling every request sends: the file's value, else the default (outputCeiling)
	Efforts         []string
	DefaultEffort   string
	Vision          bool
	ToolProfile     string // "" is the default profile
}

// The on-disk shapes Save writes. They are separate from the public types so
// the key is a plain string only here, at the file boundary, and a Secret
// everywhere else; encoding a Secret would write "[redacted]" into
// providers.toml. Load reads the same keys through the overlay shapes
// (overlay.go), whose fields are pointers.
//
// The map fields are omitempty only so encodeFile can encode a doc with a nil
// map as the file's top-level keys.
type providersDoc struct {
	Version   int                      `toml:"version"`
	Providers map[string]providerEntry `toml:"providers,omitempty"`
}

type providerEntry struct {
	Name    string   `toml:"name,omitempty"`
	Driver  string   `toml:"driver"`
	BaseURL string   `toml:"base_url,omitempty"`
	EnvKeys []string `toml:"env_keys,omitempty"`
	APIKey  string   `toml:"api_key,omitempty"`
	Source  string   `toml:"source,omitempty"`
}

type modelsDoc struct {
	Version int `toml:"version"`
	// Catalog is written only as `catalog = false`, for a Table whose
	// NoCatalog is set; nil otherwise, so a table saves as it always did.
	Catalog      *bool  `toml:"catalog,omitempty"`
	DefaultModel string `toml:"default_model"`
	// Subagents is nil whenever Table.Subagents is its zero value, so a
	// table with no sub-agent configuration saves byte-identically to a
	// models.toml written before this section existed (plan 026 §3.6).
	Subagents *subagentsDoc `toml:"subagents,omitempty"`
	// Compaction is nil whenever Table.Compaction sets nothing, for the same
	// reason (plan 028 §3.6).
	Compaction *compactionDoc        `toml:"compaction,omitempty"`
	Models     map[string]modelEntry `toml:"models,omitempty"`
}

// compactionDoc is [compaction]'s on-disk shape: Compaction's, a nil pointer
// a key the file leaves out.
type compactionDoc struct {
	Auto             *bool `toml:"auto,omitempty"`
	ThresholdPercent *int  `toml:"threshold_percent,omitempty"`
	TailTokens       *int  `toml:"tail_tokens,omitempty"`
}

// subagentsDoc is [subagents]'s on-disk shape. Tiers is a free-form map (any
// key decodes), so decodeStrict's unknown-key check only ever fires on
// Model, Effort or a key that is not one of the three.
type subagentsDoc struct {
	Model  string            `toml:"model,omitempty"`
	Effort string            `toml:"effort,omitempty"`
	Tiers  map[string]string `toml:"tiers,omitempty"`
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
	// Cost is encoded separately, by encodeModels, as its own
	// [models."<alias>".cost] table: encoding it here, nested inside this
	// struct's own standalone Encode call, would print an unqualified
	// [cost] header instead (BurntSushi has no notion of the enclosing
	// manual header at that point). Load reads the same key through
	// modelOverlay, whose Cost is this type too.
	Cost *Cost `toml:"cost,omitempty"`
}

// Headers written above each file's body. Save rewrites both files whole, so
// a comment added by hand would be lost; the header says so.
const (
	providersHeader = `# craze native harness: provider endpoints and API keys.
#
# This file was written by craze: comments and key order are not preserved if
# craze rewrites it. API keys live only here: keep this file 0600 and never
# paste it anywhere.

`
	modelsHeader = `# craze native harness: model aliases, wire ids, limits and efforts.
#
# This file was written by craze: comments and key order are not preserved if
# craze rewrites it. No secrets live here (API keys are in providers.toml), so
# it is safe to share.

`
)

// Load reads providers.toml and models.toml from dir and merges them over the
// catalog compiled into this binary (plan 031 §3.2). Either file may be
// missing, or both: an empty directory is the shipped catalog alone. A
// models.toml that says `catalog = false` is the exception, and needs both
// (LoadWith).
//
// A providers.toml readable by group or others is tightened to 0600 before it
// is read, and the fix is reported in Table.Warnings, as is every key or entry
// of the user's files dropped because it could not stand against the catalog.
func Load(dir string) (*Table, error) {
	cat, err := shippedCatalog()
	if err != nil {
		return nil, fmt.Errorf("modeltable: the shipped catalog: %w", err)
	}
	return LoadWith(dir, cat)
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
// interruption leaves something loadable: a save never deletes a provider, so
// if the second write never happens the old models.toml still names only
// providers the new providers.toml has.
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
			Name:    p.Name,
			Driver:  p.Driver,
			BaseURL: p.BaseURL,
			EnvKeys: p.EnvKeys,
			APIKey:  p.APIKey.Reveal(), // the one place a key is written: its own file
			Source:  p.Source,
		}
	}
	return encodeFile(providersHeader, &providersDoc{Version: Version}, "providers", entries)
}

// encodeModels does not use encodeFile: an entry's Cost, when set, is written
// as its own [models."<alias>".cost] table rather than through the entry's
// own Encode call, which knows nothing of the manual header already written
// and would print an unqualified [cost] instead (a sibling of [models], not
// a child of the alias's table).
func (t *Table) encodeModels() ([]byte, error) {
	entries := make(map[string]modelEntry, len(t.Models))
	for alias, m := range t.Models {
		// A conversion, so a field added to Model and not to modelEntry (or
		// the reverse) is a compile error rather than a field Save drops.
		entries[alias] = modelEntry(m)
	}
	top := &modelsDoc{Version: Version, DefaultModel: t.DefaultModel, Subagents: subagentsToDoc(t.Subagents),
		Compaction: compactionToDoc(t.Compaction)}
	if t.NoCatalog {
		top.Catalog = new(bool)
	}

	var buf bytes.Buffer
	buf.WriteString(modelsHeader)
	if err := toml.NewEncoder(&buf).Encode(top); err != nil {
		return nil, fmt.Errorf("modeltable: encoding: %w", err)
	}
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		e := entries[id]
		cost := e.Cost
		e.Cost = nil // written after, at its own full table path
		fmt.Fprintf(&buf, "\n[%s]\n", toml.Key{"models", id})
		if err := toml.NewEncoder(&buf).Encode(e); err != nil {
			return nil, fmt.Errorf("modeltable: encoding: %w", err)
		}
		if cost != nil {
			fmt.Fprintf(&buf, "\n[%s]\n", toml.Key{"models", id, "cost"})
			if err := toml.NewEncoder(&buf).Encode(cost); err != nil {
				return nil, fmt.Errorf("modeltable: encoding: %w", err)
			}
		}
	}
	return buf.Bytes(), nil
}

// compactionFromDoc is the empty Compaction when d is nil (no [compaction] in
// the file), else what it sets.
func compactionFromDoc(d *compactionDoc) Compaction {
	if d == nil {
		return Compaction{}
	}
	return Compaction{AutoSet: d.Auto, ThresholdPercentSet: d.ThresholdPercent, TailTokensSet: d.TailTokens}
}

// compactionToDoc is nil exactly when c sets nothing, so Save omits
// [compaction] entirely rather than writing an empty table: a table with no
// compaction settings saves byte-identically to a models.toml written before
// the section existed.
func compactionToDoc(c Compaction) *compactionDoc {
	if c.AutoSet == nil && c.ThresholdPercentSet == nil && c.TailTokensSet == nil {
		return nil
	}
	return &compactionDoc{Auto: c.AutoSet, ThresholdPercent: c.ThresholdPercentSet, TailTokens: c.TailTokensSet}
}

// subagentsFromDoc is the empty Subagents when d is nil (no [subagents] in
// the file), else its fields, with a present-but-empty tiers table folded to
// nil like every other map here (nilIfEmpty's map counterpart).
func subagentsFromDoc(d *subagentsDoc) Subagents {
	if d == nil {
		return Subagents{}
	}
	return Subagents{Model: d.Model, Effort: d.Effort, Tiers: nilIfEmptyMap(d.Tiers)}
}

// subagentsToDoc is nil exactly when s is Subagents' zero value, so Save
// omits [subagents] entirely rather than writing an empty table (decision 2:
// a table with no sub-agent configuration saves byte-identically to today).
func subagentsToDoc(s Subagents) *subagentsDoc {
	if s.Model == "" && s.Effort == "" && len(s.Tiers) == 0 {
		return nil
	}
	return &subagentsDoc{Model: s.Model, Effort: s.Effort, Tiers: s.Tiers}
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
	return validate(t, ProvidersFile, ModelsFile)
}

func validate(t *Table, pfile, mfile string) error {
	for _, id := range slices.Sorted(maps.Keys(t.Providers)) {
		if err := validateProvider(pfile, id, t.Providers[id]); err != nil {
			return err
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(t.Models)) {
		if err := validateModel(mfile, alias, t.Models[alias], t.Providers); err != nil {
			return err
		}
	}
	if t.DefaultModel == "" {
		return &FileError{File: mfile, Key: "default_model", Reason: "missing: name the alias a session starts on"}
	}
	if _, ok := t.Models[t.DefaultModel]; !ok {
		return &FileError{File: mfile, Key: "default_model",
			Reason: fmt.Sprintf("%q is not a model in %s", t.DefaultModel, ModelsFile)}
	}
	if err := validateSubagents(mfile, t.Subagents, t.Models); err != nil {
		return err
	}
	return validateCompaction(mfile, t.Compaction)
}

// providerTable and modelTable are an entry's TOML table path, as a
// FileError names it: providers.fireworks, models."fireworks/kimi-k3".
func providerTable(id string) string { return toml.Key{"providers", id}.String() }
func modelTable(alias string) string { return toml.Key{"models", alias}.String() }

func validateProvider(file, id string, p Provider) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: providerTable(id), Key: key, Reason: reason}
	}
	if strings.TrimSpace(id) == "" {
		return at("", "a provider id must not be empty")
	}
	if r := driverProblem(p.Driver); r != "" {
		return at("driver", r)
	}
	if r := baseURLProblem(p.BaseURL); r != "" {
		return at("base_url", r)
	}
	if r := endpointProblem(p.Driver, p.BaseURL); r != "" {
		return at("base_url", r)
	}
	if r := envKeysProblem(p.EnvKeys); r != "" {
		return at("env_keys", r)
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

// driverProblem is why driver is not one craze has, "" when it is.
func driverProblem(driver string) string {
	switch driver {
	case DriverOpenAICompat, DriverOpenRouter:
		return ""
	case "":
		return `missing: want "openai-compat" or "openrouter"`
	}
	return fmt.Sprintf(`unknown driver %q: want "openai-compat" or "openrouter"`, driver)
}

// baseURLProblem is why a base URL is not one, "" when it is (or is ""). The
// value is not echoed: a base URL can carry a key in its query.
func baseURLProblem(baseURL string) string {
	if baseURL != "" && !httpURL(baseURL) {
		return "not an absolute http or https URL"
	}
	return ""
}

// endpointProblem is why a known driver and a base URL do not go together,
// "" when they do: openai-compat needs one, openrouter's is fixed.
func endpointProblem(driver, baseURL string) string {
	switch {
	case driver == DriverOpenAICompat && baseURL == "":
		return `missing: driver "openai-compat" needs the endpoint's base URL`
	case driver == DriverOpenRouter && baseURL != "":
		return `must be absent for driver "openrouter", whose endpoint is fixed`
	}
	return ""
}

// envKeysProblem is why an env_keys list is not one, "" when it is.
func envKeysProblem(names []string) string {
	for i, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Sprintf("entry %d is empty", i+1)
		}
	}
	return ""
}

// effortsProblem is why an efforts list is not one, "" when it is.
func effortsProblem(efforts []string) string {
	seen := make(map[string]bool, len(efforts))
	for i, e := range efforts {
		if strings.TrimSpace(e) == "" {
			return fmt.Sprintf("entry %d is empty", i+1)
		}
		if seen[e] {
			return fmt.Sprintf("%q is listed twice", e)
		}
		seen[e] = true
	}
	return ""
}

// defaultEffortProblem is why def is not a default effort for efforts, ""
// when it is (or is "", no default).
func defaultEffortProblem(efforts []string, def string) string {
	if def != "" && !slices.Contains(efforts, def) {
		return fmt.Sprintf("%q is not one of efforts", def)
	}
	return ""
}

// toolProfileProblem is why a tool_profile is not one craze has, "" when it
// is ("" itself is the default).
func toolProfileProblem(profile string) string {
	if profile == "" || slices.Contains(toolProfiles, profile) {
		return ""
	}
	names := make([]string, len(toolProfiles))
	for i, p := range toolProfiles {
		names[i] = strconv.Quote(p)
	}
	return fmt.Sprintf("unknown tool profile %q: want one of %s, or leave it out for the default",
		profile, strings.Join(names, ", "))
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

func validateModel(file, alias string, m Model, providers map[string]Provider) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: modelTable(alias), Key: key, Reason: reason}
	}
	if strings.TrimSpace(alias) == "" {
		return at("", "a model alias must not be empty")
	}
	if m.Provider == "" {
		return at("provider", "missing: name a provider in "+ProvidersFile)
	}
	if _, ok := providers[m.Provider]; !ok {
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
	if r := effortsProblem(m.Efforts); r != "" {
		return at("efforts", r)
	}
	if r := defaultEffortProblem(m.Efforts, m.DefaultEffort); r != "" {
		return at("default_effort", r)
	}
	if r := toolProfileProblem(m.ToolProfile); r != "" {
		return at("tool_profile", r)
	}
	return validateCost(file, alias, m.Cost)
}

// validateCost checks c's set rates (plan 028 §3.14): each, when set, is a
// finite number, not negative and not more than MaxCostPerMillion. NaN is
// refused by name, because it compares false with both ends of the range
// (TOML spells it `nan`). c is nil for a model with no [cost] table, which is
// always valid. Checked in a fixed field order so the same table always
// reports the same problem first.
func validateCost(file, alias string, c *Cost) error {
	if c == nil {
		return nil
	}
	at := func(key string, v float64) error {
		return &FileError{File: file, Table: toml.Key{"models", alias, "cost"}.String(), Key: key,
			Reason: fmt.Sprintf("%v is out of range: want a rate in dollars per 1,000,000 tokens from 0 to %v", v, MaxCostPerMillion)}
	}
	for _, f := range []struct {
		key string
		v   *float64
	}{{"input", c.Input}, {"output", c.Output}, {"cache_read", c.CacheRead}, {"cache_write", c.CacheWrite}} {
		if f.v == nil {
			continue
		}
		if math.IsNaN(*f.v) || math.IsInf(*f.v, 0) || *f.v < 0 || *f.v > MaxCostPerMillion {
			return at(f.key, *f.v)
		}
	}
	return nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Resolve returns everything needed to build alias's client. The key is the
// first of the provider's env_keys that getenv reports set to a usable key
// (surrounding whitespace trimmed; a value that fails MinKeyLen or overlaps
// the redaction marker is skipped, plan 031 §3.2), else the inline api_key,
// else ErrNoAPIKey naming the provider and the variable names tried. A nil
// getenv is os.Getenv; the harness injects its own so tests never read the
// real environment.
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
		MaxOutputTokens: outputCeiling(m),
		Efforts:         slices.Clone(m.Efforts),
		DefaultEffort:   m.DefaultEffort,
		Vision:          m.Vision,
		ToolProfile:     m.ToolProfile,
	}, nil
}

// DefaultMaxOutputTokens is the output ceiling a model with no
// max_output_tokens gets (D-74, discovery/native-harness/08-decisions.md). The
// plan 029 eval saw craze on fireworks/deepseek-v4p1-flash run away on 5 of 25
// plan-mode runs: one response repeated itself up to the provider's own cap,
// 66-82k output tokens and 9-14 minutes. 32,000 is opencode's own default
// ceiling (min(model output limit, 32k), opencode
// packages/opencode/src/provider/transform.ts). An explicit max_output_tokens
// always wins, even above it.
const DefaultMaxOutputTokens = 32000

// outputCeiling is the output ceiling Resolve reports for m: the file's
// max_output_tokens when set, whatever its size (an explicit value wins even
// above the default, even at or above the window; that is the owner's own
// choice); otherwise DefaultMaxOutputTokens, held to a quarter of a known
// window. craze knows no model's own output limit apart from max_output_tokens
// itself (the table carries no separate one), so the window is all a default
// can be checked against: one that took most of a small window would make a strict
// server (vLLM refuses prompt + max_tokens above the context length) fail every
// request, and would turn off automatic compaction, which compactionThreshold
// skips when the ceiling takes the window. A quarter keeps that threshold at
// 75% on a small window and leaves every window of 128,000 tokens or more at
// the full default. It is never under 1, so every request names a ceiling,
// even on a window too small for a quarter of it to be a whole token.
func outputCeiling(m Model) int {
	if m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	if m.ContextWindow > 0 {
		return max(min(DefaultMaxOutputTokens, m.ContextWindow/4), 1)
	}
	return DefaultMaxOutputTokens
}

// resolveKey is Resolve's key for provider id: see Resolve. An env value that
// cannot be a key is skipped as though unset — it is not craze's credential,
// and one unrelated short META_API_KEY must not fail every session (plan 031
// §3.2) — and the error for a provider with none left says which were.
func resolveKey(id string, p Provider, getenv func(string) string) (Secret, error) {
	var unusable []string
	for _, name := range p.EnvKeys {
		k, err := envKey(getenv, name)
		switch {
		case err != nil:
			unusable = append(unusable, name)
		case k != "":
			return k, nil
		}
	}
	if v := strings.TrimSpace(p.APIKey.Reveal()); v != "" {
		return Secret(v), nil
	}
	return "", noAPIKey(id, p.EnvKeys, unusable)
}

// envKey is the one rule every reader of a key variable follows (plan 031
// §3.2), so "funded" means the same to Resolve, Keys and EnvWarnings: name's
// value through getenv, surrounding whitespace trimmed — "" when it is unset
// or blank, and keyProblem's error, with no value, when it is set to one that
// cannot be a key, which every reader skips.
func envKey(getenv func(string) string, name string) (Secret, error) {
	v := strings.TrimSpace(getenv(name))
	if err := keyProblem(v); err != nil {
		return "", err
	}
	return Secret(v), nil
}

// envKeyNames is every env_keys name of providers, sorted and without
// repeats.
func envKeyNames(providers map[string]Provider) []string {
	var names []string
	for _, p := range providers {
		names = append(names, p.EnvKeys...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Keys returns every key the table knows of, for the redactor that keeps
// them out of tool output (plan 019 §3.8): for every provider, used or not,
// the value of each of its env_keys variables that getenv reports set to a
// usable key — all of them, not only the first, which is the one Resolve
// picks — and its inline api_key, each with surrounding whitespace trimmed. A
// nil getenv is os.Getenv.
//
// An env value that cannot be a key — under MinKeyLen bytes, or overlapping
// the redaction marker — is skipped, exactly as Resolve skips it, so "funded"
// means the same everywhere (plan 031 §3.2): it is never sent to a provider,
// so it is not a credential to redact, and EnvWarnings names the variable for
// the session to say so. An inline key that fails either rule still fails,
// naming the provider, never the value: Load refuses one, so only a table
// built in memory can hold it.
func (t *Table) Keys(getenv func(string) string) ([]Secret, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var keys []Secret
	for _, id := range slices.Sorted(maps.Keys(t.Providers)) {
		p := t.Providers[id]
		for _, name := range p.EnvKeys {
			if k, err := envKey(getenv, name); err == nil && k != "" {
				keys = append(keys, k)
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

// EnvWarnings is one line for each variable a provider takes its key from
// that getenv reports set to a value that cannot be a key (plan 031 §3.2):
// Resolve and Keys skip it, and the session says so, naming the variable and
// the rule, never the value. Each variable is named once, in sorted order. A
// nil getenv is os.Getenv.
func (t *Table) EnvWarnings(getenv func(string) string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	var out []string
	for _, name := range envKeyNames(t.Providers) {
		_, err := envKey(getenv, name)
		switch {
		case errors.Is(err, ErrKeyTooShort):
			out = append(out, fmt.Sprintf("modeltable: %s is set to a value shorter than %d bytes, which cannot be an API key; it is ignored", name, MinKeyLen))
		case err != nil:
			out = append(out, fmt.Sprintf("modeltable: %s is set to a value that overlaps craze's redaction marker, which would print the key back in its own place; it is ignored", name))
		}
	}
	return out
}

// Aliases returns every model alias, sorted.
func (t *Table) Aliases() []string {
	return slices.Sorted(maps.Keys(t.Models))
}

// Rates is one identity's per-token rates, in picodollars — an integer
// number of trillionths of a dollar, so that a usage's cost is exact in
// int64 (PD22): $1 per 1,000,000 tokens is 1,000,000 p$/token. Each is the
// model's Cost rate rounded to the nearest picodollar exactly once, by
// ratesFromCost; a rate the Cost left unset is 0.
type Rates struct {
	Input, Output, CacheRead, CacheWrite int64
}

// identity is (provider, wire model): what Table prices by (R2-7), because a
// resumed session's alias may no longer exist while the identity it named
// still does, and two aliases of one identity should price the same way.
type identity struct {
	Provider, WireModel string
}

// Price returns the picodollar-per-token rates priced for the (provider,
// wire model) identity, and whether one was found. It is the canonical map's
// lookup (R2-7): among every alias sharing that identity, the first in
// sorted order that has a Cost. C16 prices every usage record through it,
// resolving the record's alias to its identity first — Price itself never
// takes an alias, so a record whose alias no longer resolves can still be
// priced from its identity.
func (t *Table) Price(provider, wireModel string) (Rates, bool) {
	prices, _ := pricedIdentities(t.Models)
	r, ok := prices[identity{provider, wireModel}]
	return r, ok
}

// pricedIdentities builds the canonical (provider, wire model) → Rates map
// (R2-7): for each identity, the first alias in sorted order that has a Cost.
// warn is one line per identity where two or more priced aliases disagree on
// the configured cost, naming every alias whose cost differs from the one
// used — the table's existing warning channel, appended by load. The
// comparison is of the Cost as written, all four keys, not of the rounded
// Rates: two prices the owner wrote differently are a conflict even when
// both round to the same picodollars (0.0000001 and 0.0000002 are both 0
// p$/token), and so is a key one alias sets and the other leaves out. An
// identity no priced alias names is simply absent from prices (Price's
// ok = false).
func pricedIdentities(models map[string]Model) (prices map[identity]Rates, warn []string) {
	type priced struct {
		alias string
		cost  *Cost
		rates Rates
	}
	byIdentity := make(map[identity][]priced)
	for _, alias := range slices.Sorted(maps.Keys(models)) {
		m := models[alias]
		if m.Cost == nil {
			continue
		}
		id := identity{m.Provider, m.WireModel}
		byIdentity[id] = append(byIdentity[id], priced{alias, m.Cost, ratesFromCost(m.Cost)})
	}
	if len(byIdentity) == 0 {
		return nil, nil
	}
	prices = make(map[identity]Rates, len(byIdentity))
	for _, id := range sortedIdentities(byIdentity) {
		entries := byIdentity[id] // already alias-sorted, from the range above
		prices[id] = entries[0].rates
		var diffs []string
		for _, e := range entries[1:] {
			if !e.cost.sameAs(entries[0].cost) {
				diffs = append(diffs, e.alias)
			}
		}
		if len(diffs) > 0 {
			warn = append(warn, fmt.Sprintf(
				"modeltable: %s and %s are both %s %s but set different cost; %s's price (sorted first) is used",
				entries[0].alias, strings.Join(diffs, ", "), id.Provider, id.WireModel, entries[0].alias))
		}
	}
	return prices, warn
}

// sameAs reports whether c and o configure the same cost: each of the four
// keys is left out of both, or set in both to the same value. Neither is nil
// here (pricedIdentities only compares models that have a Cost).
func (c *Cost) sameAs(o *Cost) bool {
	same := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return same(c.Input, o.Input) && same(c.Output, o.Output) &&
		same(c.CacheRead, o.CacheRead) && same(c.CacheWrite, o.CacheWrite)
}

// sortedIdentities returns m's keys in a deterministic order (by provider,
// then wire model), so pricedIdentities' warnings are stable from one load
// to the next.
func sortedIdentities[V any](m map[identity]V) []identity {
	ids := make([]identity, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b identity) int {
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.WireModel, b.WireModel)
	})
	return ids
}

// ratesFromCost rounds c's set rates to picodollars per token exactly once
// (PD22). c is never nil: callers only call it for a Model whose Cost is set.
func ratesFromCost(c *Cost) Rates {
	return Rates{
		Input:      picodollarsPerToken(c.Input),
		Output:     picodollarsPerToken(c.Output),
		CacheRead:  picodollarsPerToken(c.CacheRead),
		CacheWrite: picodollarsPerToken(c.CacheWrite),
	}
}

// picodollarsPerToken rounds a $-per-1,000,000-token rate to the nearest
// integer number of picodollars per token: $1/M is 1,000,000 p$/token, so
// the conversion is ×1,000,000 rounded to the nearest integer, half up. It
// is done in decimal, not in float64: the rate is read back as its shortest
// decimal spelling — what the file wrote — and shifted exactly, because a
// binary product can land a decimal half step on the wrong side (0.0001245 ×
// 1e6 is 124.49999999999999 in float64, while 124.5 rounds to 125). A nil
// rate (the file left the key out) prices as 0, as does a non-finite one,
// which Validate refuses before any table is priced.
func picodollarsPerToken(dollarsPerMillion *float64) int64 {
	if dollarsPerMillion == nil || math.IsNaN(*dollarsPerMillion) || math.IsInf(*dollarsPerMillion, 0) {
		return 0
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(*dollarsPerMillion, 'f', -1, 64))
	if !ok { // unreachable: 'f' spells every finite float64 as a decimal SetString reads
		return 0
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	// Half up, away from zero: q is the quotient truncated toward zero, and
	// a remainder of at least half the denominator moves it one step out.
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Abs(m).Lsh(m, 1).Cmp(r.Denom()) >= 0 {
		q.Add(q, big.NewInt(int64(r.Sign())))
	}
	return q.Int64()
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

// nilIfEmptyMap is nilIfEmpty for a TOML table: a Table reads the same
// whether a file wrote an empty `[subagents.tiers]` or left it out.
func nilIfEmptyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// validateCompaction checks [compaction]'s settings (plan 028 §3.6):
// threshold_percent, when set, is 1 to 99 — 0 would compact before every turn,
// and 100 never before the provider refuses the request — and tail_tokens,
// when set, is not negative.
func validateCompaction(file string, c Compaction) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: "compaction", Key: key, Reason: reason}
	}
	if p := c.ThresholdPercentSet; p != nil && (*p < 1 || *p > 99) {
		return at("threshold_percent", fmt.Sprintf("%d is out of range: want a percentage of the context window from 1 to 99", *p))
	}
	if n := c.TailTokensSet; n != nil && *n < 0 {
		return at("tail_tokens", "must not be negative")
	}
	return nil
}

// tierKeyPattern is the format a [subagents.tiers] key must match: lowercase
// letters, digits and hyphens, so it reads as a bare word in the resolved
// model's error text and so a tier name is always ready to compare against a
// caller's value case-insensitively without a second normalisation step.
var tierKeyPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// validateSubagents checks Subagents against the rules Load enforces
// (decision 1): Model, when set, and every Tiers value must be an alias in
// models; a Tiers key must match tierKeyPattern and its value must not be
// empty; Effort, when set together with Model, must be one of that model's
// efforts. Effort set with Model absent is not checked here — resolution
// checks it against whichever model the child actually ends up on
// (harness.Session.resolveChildEffort, plan 026 §3.6).
func validateSubagents(file string, s Subagents, models map[string]Model) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: "subagents", Key: key, Reason: reason}
	}
	if s.Model != "" {
		m, ok := models[s.Model]
		if !ok {
			return at("model", fmt.Sprintf("%q is not a model in %s", s.Model, ModelsFile))
		}
		if s.Effort != "" && !slices.Contains(m.Efforts, s.Effort) {
			return at("effort", fmt.Sprintf("%q is not offered by %q", s.Effort, s.Model))
		}
	}
	if err := validateTierNames(file, s); err != nil {
		return err
	}
	for _, tier := range slices.Sorted(maps.Keys(s.Tiers)) {
		if _, ok := models[s.Tiers[tier]]; !ok {
			return &FileError{File: file, Table: "subagents.tiers", Key: tier,
				Reason: fmt.Sprintf("%q is not a model in %s", s.Tiers[tier], ModelsFile)}
		}
	}
	return nil
}

// validateTierNames checks what [subagents.tiers] says on its own, whatever
// the table holds: each key matches tierKeyPattern and is not "inherit", and
// no value is empty. Which models the values name is validateSubagents' (or,
// over a catalog, the merge's) to check.
func validateTierNames(file string, s Subagents) error {
	for _, tier := range slices.Sorted(maps.Keys(s.Tiers)) {
		tierAt := func(reason string) error {
			return &FileError{File: file, Table: "subagents.tiers", Key: tier, Reason: reason}
		}
		// review r5, finding 1: resolveModelValue checks "inherit" before it
		// ever consults [subagents.tiers] (it always means the parent's
		// model), so a mapping filed under that key could never be used.
		// Checked case-insensitively, ahead of tierKeyPattern, so every
		// spelling gets this reason rather than "must match [a-z0-9-]+".
		if strings.EqualFold(tier, "inherit") {
			return tierAt(`"inherit" always means the parent's model; a tier cannot be named that`)
		}
		if !tierKeyPattern.MatchString(tier) {
			return tierAt("a tier name must match [a-z0-9-]+")
		}
		if strings.TrimSpace(s.Tiers[tier]) == "" {
			return tierAt("missing: name an alias in " + ModelsFile)
		}
	}
	return nil
}
