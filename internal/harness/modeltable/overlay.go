package modeltable

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The user's two files over the shipped catalog (plan 031 §3.2–§3.3).
//
// Each file is optional. A file that exists is decoded strictly into an
// overlay — every field a pointer, so a key the file writes and a key it
// leaves out differ even when the value written is the zero one — and its
// entries are laid over the catalog's field by field. A problem the file has
// on its own (TOML syntax, an unknown key, a wrong version, a value no entry
// could hold, an invalid inline key) still fails the load, naming the file,
// the table and the key, as it always has. A problem an entry has only
// against what the catalog holds — its provider gone, a default_effort the
// model no longer offers, an override of a model craze stopped shipping —
// drops that key or that entry with one Table.Warnings line instead: a
// release never breaks a load (§3.2).

// legacySource is the source craze's former gx importer wrote on every entry
// it made (removed by plan 031 C1). Such an entry yields to the catalog (plan 031 §3.3): a model entry is
// ignored when its alias is shipped or retired, and a provider entry for a
// shipped id gives only its key and any env_keys names the catalog lacks.
// Nothing writes it any more; it is only read.
const legacySource = "gx"

// Origin says where a merged table's entry came from, for `craze auth list`
// and tests (plan 031 §3.2). It is kept beside the table, never persisted.
type Origin string

const (
	// OriginShipped is an entry exactly as the catalog ships it (an old
	// import's provider entry, which only lends it a key, included).
	OriginShipped Origin = "shipped"
	// OriginOverridden is a shipped entry with fields the user's file set.
	OriginOverridden Origin = "overridden"
	// OriginYours is an entry only the user's files define. Every entry of a
	// table loaded without a catalog, or built in memory, is the user's.
	OriginYours Origin = "yours"
)

// The overlay shapes. Their toml keys are exactly providerEntry's and
// modelEntry's, which Save writes (TestOverlayKeysMatchTheSavedShape pins
// that), plus models.toml's top-level `catalog`.
type providersOverlay struct {
	Version   int                        `toml:"version"`
	Providers map[string]providerOverlay `toml:"providers"`
}

func (d *providersOverlay) version() int { return d.Version }

type providerOverlay struct {
	Name    *string   `toml:"name"`
	Driver  *string   `toml:"driver"`
	BaseURL *string   `toml:"base_url"`
	EnvKeys *[]string `toml:"env_keys"`
	APIKey  *string   `toml:"api_key"`
	Source  *string   `toml:"source"`
}

type modelsOverlay struct {
	Version int `toml:"version"`
	// Catalog is `catalog = false` (plan 031 P10): the directory's two files
	// are then the whole table, under the rules that held before the catalog
	// existed.
	Catalog      *bool                   `toml:"catalog"`
	DefaultModel *string                 `toml:"default_model"`
	Subagents    *subagentsDoc           `toml:"subagents"`
	Compaction   *compactionDoc          `toml:"compaction"`
	Models       map[string]modelOverlay `toml:"models"`
}

func (d *modelsOverlay) version() int { return d.Version }

type modelOverlay struct {
	Provider        *string   `toml:"provider"`
	WireModel       *string   `toml:"wire_model"`
	Name            *string   `toml:"name"`
	ContextWindow   *int      `toml:"context_window"`
	MaxOutputTokens *int      `toml:"max_output_tokens"`
	Efforts         *[]string `toml:"efforts"`
	DefaultEffort   *string   `toml:"default_effort"`
	Vision          *bool     `toml:"vision"`
	ToolProfile     *string   `toml:"tool_profile"`
	Source          *string   `toml:"source"`
	// Cost's rates are pointers already: each one the file writes overrides
	// the shipped rate on its own, and one it leaves out keeps the shipped
	// rate (an inherited rate cannot be cleared).
	Cost *Cost `toml:"cost"`
}

// deref is *p, or the zero value when the file left the key out.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// differs reports whether the file wrote the key, to a value other than the
// shipped one.
func differs[T comparable](written *T, shipped T) bool {
	return written != nil && *written != shipped
}

// listDiffers is differs for a list. slices.Equal counts an explicit empty
// list as equal to none, which is how the merge reads one.
func listDiffers(written *[]string, shipped []string) bool {
	return written != nil && !slices.Equal(*written, shipped)
}

// complete is o as an entry of its own, every key it leaves out at its zero
// value: a provider the catalog does not ship, or any provider of a table
// loaded without one. It is o laid over the zero entry, where the
// dependent-field rules have nothing to inherit.
func (o providerOverlay) complete() Provider { return overlayProvider(Provider{}, o) }

// complete is o as a model entry of its own (see providerOverlay.complete).
func (o modelOverlay) complete() Model { return overlayModel(Model{}, o) }

// overlayProvider is base with the fields o writes (plan 031 §3.2), and the
// two dependent-field rules: a driver override keeps the shipped base_url only
// when the merged driver is still the shipped one (so `driver = "openrouter"`
// alone drops it), and an entry that changes the endpoint — driver or base_url
// — without writing env_keys does not inherit the shipped env_keys, because
// the shipped variable belongs to the shipped endpoint and an exported
// FIREWORKS_API_KEY must never reach the user's own. An explicit empty list is
// "no variables".
func overlayProvider(base Provider, o providerOverlay) Provider {
	p := base
	p.EnvKeys = slices.Clone(base.EnvKeys)
	if o.Name != nil {
		p.Name = *o.Name
	}
	if o.Driver != nil {
		p.Driver = *o.Driver
	}
	switch {
	case o.BaseURL != nil:
		p.BaseURL = *o.BaseURL
	case p.Driver != base.Driver:
		p.BaseURL = ""
	}
	switch {
	case o.EnvKeys != nil:
		p.EnvKeys = nilIfEmpty(slices.Clone(*o.EnvKeys))
	case p.Driver != base.Driver || p.BaseURL != base.BaseURL:
		p.EnvKeys = nil
	}
	if o.APIKey != nil {
		p.APIKey = Secret(*o.APIKey)
	}
	p.Source = sourceOrManual(deref(o.Source))
	return p
}

// overlayModel is base with the fields o writes (plan 031 §3.2). Lists
// replace the shipped list whole, and an explicit empty one means empty
// (`efforts = []` is no effort control). An efforts override that does not
// set default_effort keeps the shipped default only if the new list has it,
// and otherwise clears it. Each cost rate overrides on its own.
func overlayModel(base Model, o modelOverlay) Model {
	m := cloneModel(base)
	if o.Provider != nil {
		m.Provider = *o.Provider
	}
	if o.WireModel != nil {
		m.WireModel = *o.WireModel
	}
	if o.Name != nil {
		m.Name = *o.Name
	}
	if o.ContextWindow != nil {
		m.ContextWindow = *o.ContextWindow
	}
	if o.MaxOutputTokens != nil {
		m.MaxOutputTokens = *o.MaxOutputTokens
	}
	if o.Efforts != nil {
		m.Efforts = nilIfEmpty(slices.Clone(*o.Efforts))
		if !slices.Contains(m.Efforts, m.DefaultEffort) {
			m.DefaultEffort = ""
		}
	}
	if o.DefaultEffort != nil {
		m.DefaultEffort = *o.DefaultEffort
	}
	if o.Vision != nil {
		m.Vision = *o.Vision
	}
	if o.ToolProfile != nil {
		m.ToolProfile = *o.ToolProfile
	}
	if c := o.Cost; c != nil {
		if m.Cost == nil {
			m.Cost = &Cost{}
		}
		m.Cost.Input = cmp.Or(clonePtr(c.Input), m.Cost.Input)
		m.Cost.Output = cmp.Or(clonePtr(c.Output), m.Cost.Output)
		m.Cost.CacheRead = cmp.Or(clonePtr(c.CacheRead), m.Cost.CacheRead)
		m.Cost.CacheWrite = cmp.Or(clonePtr(c.CacheWrite), m.Cost.CacheWrite)
	}
	m.Source = sourceOrManual(deref(o.Source))
	return m
}

// LoadWith is Load over cat instead of the shipped catalog, for tests (plan
// 031 §3.2). A nil or empty cat is no catalog at all, as is a models.toml that
// says `catalog = false`: the two files are then the whole table under the
// rules that held before the catalog existed — both needed, default_model
// required, every entry complete. cat itself is never changed.
func LoadWith(dir string, cat *Catalog) (*Table, error) {
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
	pb, pHas, err := readIfPresent(ppath)
	if err != nil {
		return nil, err
	}
	mb, mHas, err := readIfPresent(mpath)
	if err != nil {
		return nil, err
	}
	var pd providersOverlay
	if pHas {
		if err := decodeStrict(ppath, pb, &pd); err != nil {
			return nil, err
		}
	}
	var md modelsOverlay
	if mHas {
		if err := decodeStrict(mpath, mb, &md); err != nil {
			return nil, err
		}
	}

	noCatalog := md.Catalog != nil && !*md.Catalog
	var t *Table
	if noCatalog || cat.empty() {
		if t, err = loadAlone(dir, ppath, mpath, pHas, mHas, &pd, &md); err != nil {
			return nil, err
		}
		t.NoCatalog = noCatalog
	} else {
		if err := checkOverlays(ppath, mpath, &pd, &md); err != nil {
			return nil, err
		}
		t = merge(cat.Clone(), ppath, mpath, &pd, &md)
		// Every entry of the user's that could not stand was dropped above,
		// and every shipped entry passed TestShippedCatalog: a failure here is
		// a bug in the catalog or the merge, reported rather than papered over.
		if err := validate(t, ppath, mpath); err != nil {
			return nil, err
		}
	}
	t.Warnings = append(warnings, t.Warnings...)
	_, priceWarnings := pricedIdentities(t.Models)
	t.Warnings = append(t.Warnings, priceWarnings...)
	return t, nil
}

// readIfPresent reads path; a file that does not exist is not an error, only
// absent (has false).
func readIfPresent(path string) (data []byte, has bool, err error) {
	data, err = os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("modeltable: %w", err)
	}
	return data, true, nil
}

// loadAlone is the table of a directory whose files are the whole table (no
// catalog): the rules of every craze before plan 031, unchanged. Both files
// are needed; a missing one is named, and the error matches fs.ErrNotExist.
func loadAlone(dir, ppath, mpath string, pHas, mHas bool, pd *providersOverlay, md *modelsOverlay) (*Table, error) {
	switch {
	case !pHas && !mHas:
		return nil, fmt.Errorf("modeltable: %s holds neither %s nor %s (%w)", dir, ProvidersFile, ModelsFile, fs.ErrNotExist)
	case !pHas:
		return nil, fmt.Errorf("modeltable: %s is missing although %s exists (%w)", ppath, ModelsFile, fs.ErrNotExist)
	case !mHas:
		return nil, fmt.Errorf("modeltable: %s is missing although %s exists (%w)", mpath, ProvidersFile, fs.ErrNotExist)
	}
	t := &Table{
		DefaultModel: deref(md.DefaultModel),
		Providers:    make(map[string]Provider, len(pd.Providers)),
		Models:       make(map[string]Model, len(md.Models)),
		Subagents:    subagentsFromDoc(md.Subagents),
		Compaction:   compactionFromDoc(md.Compaction),
	}
	for id, o := range pd.Providers {
		t.Providers[id] = o.complete()
	}
	for alias, o := range md.Models {
		t.Models[alias] = o.complete()
	}
	if err := validate(t, ppath, mpath); err != nil {
		return nil, err
	}
	return t, nil
}

// checkOverlays fails the load on the first problem a user file has on its
// own, whatever the catalog holds (plan 031 §3.2): a value no entry could
// hold — a driver craze does not know, a base URL that is not one, a blank
// env_keys name, an invalid inline key, a negative window, a repeated effort,
// a default_effort outside efforts the same entry lists, a tool profile or a
// cost out of range — and [subagents.tiers] names and [compaction] values.
// What an entry lacks, or names, is left to merge: whether it can stand
// depends on the catalog. Entries are checked in sorted order, so the same
// file always reports the same problem.
func checkOverlays(ppath, mpath string, pd *providersOverlay, md *modelsOverlay) error {
	for _, id := range slices.Sorted(maps.Keys(pd.Providers)) {
		if err := checkProviderOverlay(ppath, id, pd.Providers[id]); err != nil {
			return err
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(md.Models)) {
		if err := checkModelOverlay(mpath, alias, md.Models[alias]); err != nil {
			return err
		}
	}
	if err := validateTierNames(mpath, subagentsFromDoc(md.Subagents)); err != nil {
		return err
	}
	return validateCompaction(mpath, compactionFromDoc(md.Compaction))
}

func checkProviderOverlay(file, id string, o providerOverlay) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: providerTable(id), Key: key, Reason: reason}
	}
	if strings.TrimSpace(id) == "" {
		return at("", "a provider id must not be empty")
	}
	if o.Driver != nil {
		if r := driverProblem(*o.Driver); r != "" {
			return at("driver", r)
		}
	}
	if r := baseURLProblem(deref(o.BaseURL)); r != "" {
		return at("base_url", r)
	}
	if o.Driver != nil && o.BaseURL != nil {
		if r := endpointProblem(*o.Driver, *o.BaseURL); r != "" {
			return at("base_url", r)
		}
	}
	if o.EnvKeys != nil {
		if r := envKeysProblem(*o.EnvKeys); r != "" {
			return at("env_keys", r)
		}
	}
	if o.APIKey != nil {
		if err := keyProblem(strings.TrimSpace(*o.APIKey)); err != nil {
			return at("api_key", keyReason(err))
		}
	}
	return nil
}

func checkModelOverlay(file, alias string, o modelOverlay) error {
	at := func(key, reason string) error {
		return &FileError{File: file, Table: modelTable(alias), Key: key, Reason: reason}
	}
	switch {
	case strings.TrimSpace(alias) == "":
		return at("", "a model alias must not be empty")
	case o.Provider != nil && *o.Provider == "":
		return at("provider", "must not be empty: name a provider")
	case o.WireModel != nil && strings.TrimSpace(*o.WireModel) == "":
		return at("wire_model", "missing: the model id the provider's API expects")
	case o.ContextWindow != nil && *o.ContextWindow < 0:
		return at("context_window", "must not be negative")
	case o.MaxOutputTokens != nil && *o.MaxOutputTokens < 0:
		return at("max_output_tokens", "must not be negative")
	}
	if o.Efforts != nil {
		if r := effortsProblem(*o.Efforts); r != "" {
			return at("efforts", r)
		}
		if r := defaultEffortProblem(*o.Efforts, deref(o.DefaultEffort)); r != "" {
			return at("default_effort", r)
		}
	}
	if o.ToolProfile != nil {
		if r := toolProfileProblem(*o.ToolProfile); r != "" {
			return at("tool_profile", r)
		}
	}
	return validateCost(file, alias, o.Cost)
}

// merger lays one directory's files over a catalog. cat is the merge's own
// deep copy (Catalog.Clone), so its entries go into the table as they are and
// share nothing with the catalog LoadWith was given.
type merger struct {
	cat          *Catalog
	ppath, mpath string
	retiredAlias map[string]bool
	retiredID    map[identity]bool
	t            *Table
}

// warn records that the user's entry at err — a *FileError, which locates it
// — could not stand against the merged catalog, and what craze does instead.
func (m *merger) warn(err error, instead string) {
	m.t.Warnings = append(m.t.Warnings, err.Error()+"; "+instead)
}

func (m *merger) redundant(fe *FileError) {
	m.t.redundant = append(m.t.redundant, fe.Error())
}

// merge lays pd and md over cat (plan 031 §3.2–§3.3). checkOverlays has
// already refused every problem the files have on their own, so each drop
// here is of something the catalog changed under the user's entry.
func merge(cat *Catalog, ppath, mpath string, pd *providersOverlay, md *modelsOverlay) *Table {
	m := &merger{
		cat: cat, ppath: ppath, mpath: mpath,
		retiredAlias: cat.retiredAliases(),
		retiredID:    cat.retiredIdentities(),
		t: &Table{
			Providers:       make(map[string]Provider, len(cat.Providers)+len(pd.Providers)),
			Models:          make(map[string]Model, len(cat.Models)+len(md.Models)),
			Compaction:      compactionFromDoc(md.Compaction),
			providerOrigins: make(map[string]Origin),
			modelOrigins:    make(map[string]Origin),
		},
	}
	for id, p := range cat.Providers {
		m.t.Providers[id] = p
		m.t.providerOrigins[id] = OriginShipped
	}
	for alias, mod := range cat.Models {
		m.t.Models[alias] = mod
		m.t.modelOrigins[alias] = OriginShipped
	}
	for _, id := range slices.Sorted(maps.Keys(pd.Providers)) {
		m.provider(id, pd.Providers[id])
	}
	for _, alias := range slices.Sorted(maps.Keys(md.Models)) {
		m.model(alias, md.Models[alias])
	}
	m.defaultModel(md.DefaultModel)
	m.subagents(subagentsFromDoc(md.Subagents))
	return m.t
}

func (m *merger) provider(id string, o providerOverlay) {
	shipped, isShipped := m.cat.Providers[id]
	if !isShipped {
		p := o.complete()
		if err := validateProvider(m.ppath, id, p); err != nil {
			m.warn(err, "craze does not ship this provider, so the entry is ignored")
			return
		}
		m.t.Providers[id] = p
		m.t.providerOrigins[id] = OriginYours
		return
	}
	if deref(o.Source) == legacySource {
		// An old import's copy of a provider craze now ships (§3.3): only
		// its key counts, and any variable names the catalog lacks, after
		// the catalog's own — a machine that relied on a gx-era variable
		// stays funded.
		p := shipped
		p.EnvKeys = slices.Clone(shipped.EnvKeys)
		if o.APIKey != nil {
			p.APIKey = Secret(*o.APIKey)
		}
		for _, name := range deref(o.EnvKeys) {
			if !slices.Contains(p.EnvKeys, name) {
				p.EnvKeys = append(p.EnvKeys, name)
			}
		}
		m.t.Providers[id] = p
		return
	}
	p := overlayProvider(shipped, o)
	if err := validateProvider(m.ppath, id, p); err != nil {
		// checkOverlays passed every value, and a pair the entry writes
		// itself: what fails is the endpoint the entry half-writes against
		// the shipped half. The shipped endpoint is always valid.
		m.warn(err, "craze's shipped driver and base_url are used")
		o.Driver, o.BaseURL = nil, nil
		p = overlayProvider(shipped, o)
	} else if r := redundantProvider(shipped, o); r != "" {
		m.redundant(&FileError{File: m.ppath, Table: providerTable(id), Reason: r})
	}
	m.t.Providers[id] = p
	m.t.providerOrigins[id] = OriginOverridden
}

// redundantProvider is the note for a provider entry whose every endpoint
// field it writes (name, driver, base_url, env_keys) equals the shipped value
// (plan 031 §3.3): legal, but it pins those values against later catalog
// updates. "" when it is not one — including an entry that writes only a key.
func redundantProvider(shipped Provider, o providerOverlay) string {
	wrote := o.Name != nil || o.Driver != nil || o.BaseURL != nil || o.EnvKeys != nil
	if o.APIKey != nil && !wrote {
		return ""
	}
	if differs(o.Name, shipped.Name) || differs(o.Driver, shipped.Driver) ||
		differs(o.BaseURL, shipped.BaseURL) || listDiffers(o.EnvKeys, shipped.EnvKeys) {
		return ""
	}
	if o.APIKey != nil {
		return "repeats craze's shipped settings — keep only api_key to follow craze's updates"
	}
	return "repeats craze's shipped entry — delete it to follow craze's updates"
}

func (m *merger) model(alias string, o modelOverlay) {
	shipped, isShipped := m.cat.Models[alias]
	retiredAlias := m.retiredAlias[alias]
	if deref(o.Source) == legacySource && (isShipped || retiredAlias) {
		return // an old import's copy (§3.3): the catalog's own, or nothing, stands
	}
	var mod Model
	if isShipped {
		mod = overlayModel(shipped, o)
	} else {
		mod = o.complete()
	}
	if m.retiredID[identity{mod.Provider, mod.WireModel}] {
		// A dead wire id is dead whoever wrote it (§3.3). For a shipped alias
		// the shipped definition stays (r2-6); only the user's entry goes.
		return
	}
	if !isShipped && (mod.Provider == "" || mod.WireModel == "") {
		if !retiredAlias { // a retired alias's leftover override goes silently
			m.warn(&FileError{File: m.mpath, Table: modelTable(alias),
				Reason: "craze does not ship this model, so its entry needs provider and wire_model"}, "the entry is ignored")
		}
		return
	}
	instead := "the entry is ignored"
	if isShipped {
		instead = "craze's shipped entry is used"
	}
	err := validateModel(m.mpath, alias, mod, m.t.Providers)
	clean := err == nil
	if fe := (*FileError)(nil); errors.As(err, &fe) && fe.Key == "default_effort" && o.DefaultEffort != nil {
		// A default_effort the model no longer offers: the key goes, the
		// entry stays, on the shipped default (or none).
		m.warn(fe, "the key is ignored")
		mod.DefaultEffort = ""
		if isShipped && slices.Contains(mod.Efforts, shipped.DefaultEffort) {
			mod.DefaultEffort = shipped.DefaultEffort
		}
		err = validateModel(m.mpath, alias, mod, m.t.Providers)
	}
	if err != nil {
		m.warn(err, instead)
		return
	}
	m.t.Models[alias] = mod
	if !isShipped {
		m.t.modelOrigins[alias] = OriginYours
		return
	}
	m.t.modelOrigins[alias] = OriginOverridden
	if clean && sameAsShippedModel(shipped, o) {
		m.redundant(&FileError{File: m.mpath, Table: modelTable(alias),
			Reason: "repeats craze's shipped entry — delete it to follow craze's updates"})
	}
}

// sameAsShippedModel reports whether every field o writes (source aside)
// equals the shipped value (plan 031 §3.3).
func sameAsShippedModel(s Model, o modelOverlay) bool {
	if differs(o.Provider, s.Provider) || differs(o.WireModel, s.WireModel) ||
		differs(o.Name, s.Name) || differs(o.ContextWindow, s.ContextWindow) ||
		differs(o.MaxOutputTokens, s.MaxOutputTokens) || listDiffers(o.Efforts, s.Efforts) ||
		differs(o.DefaultEffort, s.DefaultEffort) || differs(o.Vision, s.Vision) ||
		differs(o.ToolProfile, s.ToolProfile) {
		return false
	}
	if o.Cost == nil {
		return true
	}
	var sc Cost
	if s.Cost != nil {
		sc = *s.Cost
	}
	rate := func(u, v *float64) bool { return u == nil || (v != nil && *u == *v) }
	return rate(o.Cost.Input, sc.Input) && rate(o.Cost.Output, sc.Output) &&
		rate(o.Cost.CacheRead, sc.CacheRead) && rate(o.Cost.CacheWrite, sc.CacheWrite)
}

// defaultModel is the user's default_model when it names a merged model, else
// the catalog's: silently when it names a retired alias, with a warning for
// anything else (plan 031 §3.2). The catalog's default is always a merged
// model, since a dropped override restores the shipped entry.
func (m *merger) defaultModel(user *string) {
	m.t.DefaultModel = m.cat.DefaultModel
	if user == nil {
		return
	}
	switch u := *user; {
	case hasModel(m.t.Models, u):
		m.t.DefaultModel = u
		if u == m.cat.DefaultModel {
			m.redundant(&FileError{File: m.mpath, Key: "default_model",
				Reason: "repeats craze's shipped default — delete it to follow craze's updates"})
		}
	case m.retiredAlias[u]:
	default:
		m.warn(&FileError{File: m.mpath, Key: "default_model", Reason: fmt.Sprintf("%q is not a model", u)},
			fmt.Sprintf("craze's default %q is used", m.cat.DefaultModel))
	}
}

// subagents degrades [subagents] instead of failing the load (plan 031 §3.3):
// a model or tier target that is not a merged model is dropped — silently for
// a retired alias — falling through to plan 026 §3.6's defaults, and an
// effort the model it is set with does not offer is dropped with a warning.
func (m *merger) subagents(s Subagents) {
	if s.Model != "" && !hasModel(m.t.Models, s.Model) {
		if !m.retiredAlias[s.Model] {
			m.warn(&FileError{File: m.mpath, Table: "subagents", Key: "model", Reason: fmt.Sprintf("%q is not a model", s.Model)},
				"a sub-agent defaults to its parent's model")
		}
		s.Model = ""
	}
	for _, tier := range slices.Sorted(maps.Keys(s.Tiers)) {
		if alias := s.Tiers[tier]; !hasModel(m.t.Models, alias) {
			if !m.retiredAlias[alias] {
				m.warn(&FileError{File: m.mpath, Table: "subagents.tiers", Key: tier, Reason: fmt.Sprintf("%q is not a model", alias)},
					"the tier means the parent's model")
			}
			delete(s.Tiers, tier)
		}
	}
	s.Tiers = nilIfEmptyMap(s.Tiers)
	if s.Model != "" && s.Effort != "" && !slices.Contains(m.t.Models[s.Model].Efforts, s.Effort) {
		m.warn(&FileError{File: m.mpath, Table: "subagents", Key: "effort",
			Reason: fmt.Sprintf("%q is not offered by %q", s.Effort, s.Model)}, "the model's own default effort is used")
		s.Effort = ""
	}
	m.t.Subagents = s
}

// ModelOrigin is where alias's entry came from (plan 031 §3.2), "" when the
// table has no such model. A table loaded without a catalog, or built in
// memory, is all the user's.
func (t *Table) ModelOrigin(alias string) Origin {
	if _, ok := t.Models[alias]; !ok {
		return ""
	}
	return cmp.Or(t.modelOrigins[alias], OriginYours)
}

// ProviderOrigin is ModelOrigin for a provider id.
func (t *Table) ProviderOrigin(id string) Origin {
	if _, ok := t.Providers[id]; !ok {
		return ""
	}
	return cmp.Or(t.providerOrigins[id], OriginYours)
}

// RedundantOverrides are the user's entries that repeat what craze ships
// (plan 031 §3.3): a model entry for a shipped alias whose every field it
// writes equals the shipped value, a provider entry whose endpoint fields do,
// and a default_model equal to the catalog's. Each is legal but pins those
// values against later catalog updates, so `craze auth list` names it. One
// line each, located like a FileError, in file order; never a key.
func (t *Table) RedundantOverrides() []string {
	return slices.Clone(t.redundant)
}
