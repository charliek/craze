package modeltable

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// release never breaks a load (§3.2). A provider_order name that is no
// provider is passed over silently (plan 038 §2.4).

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
	// OriginDiscovered is a model the load learned from the ChatGPT plan's
	// own list (withDiscovered, plan 033 §3.11), as the list and craze's
	// defaults make it; one the user's models.toml overrides is
	// OriginOverridden.
	OriginDiscovered Origin = "discovered"
)

// The overlay shapes. Their toml keys are exactly providerEntry's and
// modelEntry's, which Save writes (TestOverlayKeysMatchTheSavedShape pins
// that), plus models.toml's top-level `catalog`.
type providersOverlay struct {
	Version   int                        `toml:"version"`
	Providers map[string]providerOverlay `toml:"providers"`
	// withheld are the credentials a lock took out of an entry before the
	// merge (lockAnthropic: a key written beside a base_url it dropped) — an
	// api_key and env_keys alone — which fund nothing but are credentials all
	// the same: credentialKeys and credentialNames read them as they read an
	// entry. Never in a file.
	withheld []providerOverlay `toml:"-"`
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
	Catalog      *bool   `toml:"catalog"`
	DefaultModel *string `toml:"default_model"`
	// ProviderOrder is provider_order (plan 038 §2.4): written, it replaces
	// the catalog's whole.
	ProviderOrder *[]string `toml:"provider_order"`
	// SourceOrder is source_order (plan 050 C5): written, it goes before
	// the catalog's; `[]` turns the source rule off.
	SourceOrder *[]string               `toml:"source_order"`
	Subagents   *subagentsDoc           `toml:"subagents"`
	Compaction  *compactionDoc          `toml:"compaction"`
	Models      map[string]modelOverlay `toml:"models"`
}

func (d *modelsOverlay) version() int { return d.Version }

type modelOverlay struct {
	Provider        *string   `toml:"provider"`
	WireModel       *string   `toml:"wire_model"`
	Name            *string   `toml:"name"`
	ContextWindow   *int      `toml:"context_window"`
	DefaultContext  *int      `toml:"default_context"`
	MaxOutputTokens *int      `toml:"max_output_tokens"`
	Efforts         *[]string `toml:"efforts"`
	DefaultEffort   *string   `toml:"default_effort"`
	Vision          *bool     `toml:"vision"`
	ToolProfile     *string   `toml:"tool_profile"`
	Source          *string   `toml:"source"`
	// Cost's rates are pointers already: each one the file writes overrides
	// the shipped rate on its own, and one it leaves out keeps the shipped
	// rate (an inherited rate cannot be cleared). long_context_above and the
	// long-context table's rates merge the same way, and
	// long_context_above = 0 turns a shipped tier off (overlayModel).
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
// FIREWORKS_API_KEY must never fund the user's own. An explicit empty list is
// "no variables". This decides funding only: the table still knows the
// shipped names hold keys (Table.CredentialEnvNames, C2r2), so an exported
// one is kept from the commands a session runs and redacted all the same.
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
	case p.Driver != base.Driver || !sameEndpoint(p.BaseURL, base.BaseURL):
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
// and otherwise clears it. Each cost rate overrides on its own, and so do
// long_context_above and each long-context rate (plan 047 D10); an entry that
// writes long_context_above = 0 and no long-context table of its own turns
// the shipped tier off, its table going with it. default_context merges as
// context_window does, and 0 turns a shipped one off (plan 050 C1). An entry
// that points the model at another provider or wire model makes it another
// model, so what craze learned of the one it was is forgotten
// (forgetLearned), and so is what the catalog says of the alias it was
// shipped as: deprecated, replaced_by and the maker belong to the shipped
// model, not to the alias the user has taken over (plan 050 C1, C5).
func overlayModel(base Model, o modelOverlay) Model {
	m := cloneModel(base)
	if o.Provider != nil {
		m.Provider = *o.Provider
	}
	if o.WireModel != nil {
		m.WireModel = *o.WireModel
	}
	if m.Provider != base.Provider || m.WireModel != base.WireModel {
		m.forgetLearned()
		m.Deprecated, m.ReplacedBy, m.Maker = false, "", ""
	}
	if o.Name != nil {
		m.Name = *o.Name
	}
	if o.ContextWindow != nil {
		m.ContextWindow = *o.ContextWindow
	}
	if o.DefaultContext != nil {
		m.DefaultContext = *o.DefaultContext
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
		m.Cost.LongContextAbove = cmp.Or(clonePtr(c.LongContextAbove), m.Cost.LongContextAbove)
		switch l := c.LongContext; {
		case l != nil:
			if m.Cost.LongContext == nil {
				m.Cost.LongContext = &LongContextCost{}
			}
			lc := m.Cost.LongContext
			lc.Input = cmp.Or(clonePtr(l.Input), lc.Input)
			lc.Output = cmp.Or(clonePtr(l.Output), lc.Output)
			lc.CacheRead = cmp.Or(clonePtr(l.CacheRead), lc.CacheRead)
			lc.CacheWrite = cmp.Or(clonePtr(l.CacheWrite), lc.CacheWrite)
		case c.LongContextAbove != nil && *c.LongContextAbove == 0:
			m.Cost.LongContext = nil // the tier turned off: its shipped rates go with it
		}
	}
	m.Source = sourceOrManual(deref(o.Source))
	return m
}

// LoadWith is Load over cat instead of the shipped catalog, for tests (plan
// 031 §3.2). A nil or empty cat is no catalog at all, as is a models.toml that
// says `catalog = false`: the two files are then the whole table under the
// rules that held before the catalog existed — both needed, default_model
// required, every entry complete. cat itself is never changed.
//
// Over a catalog, the ChatGPT plan's models from dir's account-bound list are
// added to the catalog's copy before the user's files are laid over it
// (withDiscovered, plan 033 §3.11), and the Anthropic account's list, bound
// to the provider's key, makes the copy's Anthropic models the account's
// (withAnthropicList, plan 044 §3.5). Either way, a table with a provider on
// the chatgpt driver keeps dir as the directory whose sign-in funds it.
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
		// The locked provider's entry loses the keys it may not set first,
		// each with a warning, so none of them can fail the load below (plan
		// 044 P22).
		warnings = append(warnings, lockAnthropic(cat, ppath, &pd)...)
		if err := checkOverlays(ppath, mpath, &pd, &md); err != nil {
			return nil, err
		}
		c := cat.Clone()
		disc, discWarnings := withDiscovered(c, dir)
		warnings = append(warnings, discWarnings...)
		warnings = append(warnings, withAnthropicList(c, dir, &pd)...)
		learned := learnedOf(c) // before the user's files touch an alias
		t = merge(c, ppath, mpath, &pd, &md)
		t.discover(disc)
		t.learned = learned
		t.noteAccountLists(c, disc)
		// Every entry of the user's that could not stand was dropped above,
		// and every shipped entry passed TestShippedCatalog: a failure here is
		// a bug in the catalog or the merge, reported rather than papered over.
		if err := validate(t, ppath, mpath, false); err != nil {
			return nil, err
		}
	}
	for _, p := range t.Providers {
		if p.Driver == DriverChatGPT {
			t.signIn = dir
			break
		}
	}
	t.Warnings = append(warnings, t.Warnings...)
	_, priceWarnings := pricedIdentities(t.Models)
	t.Warnings = append(t.Warnings, priceWarnings...)
	return t, nil
}

// discover records what withDiscovered added to the catalog t was merged
// over: each discovered model still in t, by its rank, as OriginDiscovered
// unless the user's files overrode it; and the account the list was bound
// to. A nil disc discovered nothing.
func (t *Table) discover(disc *discovered) {
	if disc == nil {
		return
	}
	t.discoveredFor = disc.account
	for alias, rank := range disc.rank {
		if !hasModel(t.Models, alias) {
			continue
		}
		if t.discovered == nil {
			t.discovered = make(map[string]int, len(disc.rank))
		}
		t.discovered[alias] = rank
		if t.modelOrigins[alias] == OriginShipped {
			t.modelOrigins[alias] = OriginDiscovered
		}
	}
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
		DefaultModel:  deref(md.DefaultModel),
		ProviderOrder: userOrder(md.ProviderOrder),
		SourceOrder:   userOrder(md.SourceOrder),
		Providers:     make(map[string]Provider, len(pd.Providers)),
		Models:        make(map[string]Model, len(md.Models)),
		Subagents:     subagentsFromDoc(md.Subagents),
		Compaction:    compactionFromDoc(md.Compaction),
	}
	for id, o := range pd.Providers {
		t.Providers[id] = o.complete()
	}
	for alias, o := range md.Models {
		t.Models[alias] = o.complete()
	}
	if err := validate(t, ppath, mpath, true); err != nil {
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
	if r := providerOrderProblem(deref(md.ProviderOrder)); r != "" {
		return &FileError{File: mpath, Key: "provider_order", Reason: r}
	}
	if r := providerOrderProblem(deref(md.SourceOrder)); r != "" {
		return &FileError{File: mpath, Key: "source_order", Reason: r}
	}
	if err := validateTierNames(mpath, subagentsFromDoc(md.Subagents)); err != nil {
		return err
	}
	return validateCompaction(mpath, compactionFromDoc(md.Compaction))
}

// userOrder is the user's provider_order as the table keeps it: nil when the
// file leaves it out, and otherwise a list of its own — non-nil even when
// empty, which is an explicit empty order (plan 038 §2.4).
func userOrder(written *[]string) []string {
	if written == nil {
		return nil
	}
	return append([]string{}, *written...)
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
	if r := aliasProblem(alias); r != "" {
		return at("", r)
	}
	switch {
	case o.Provider != nil && *o.Provider == "":
		return at("provider", "must not be empty: name a provider")
	case o.WireModel != nil && strings.TrimSpace(*o.WireModel) == "":
		return at("wire_model", "missing: the model id the provider's API expects")
	case o.ContextWindow != nil && *o.ContextWindow < 0:
		return at("context_window", "must not be negative")
	case o.DefaultContext != nil && *o.DefaultContext < 0:
		return at("default_context", "must not be negative")
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
			retired:         cat.Retired, // the merge's own copy
		},
	}
	for id, p := range cat.Providers {
		m.t.Providers[id] = p
		m.t.providerOrigins[id] = OriginShipped
	}
	for alias, mod := range cat.Models {
		// The maker is stamped on the catalog's own copy (plan 050 C5), the
		// models an account's list added to it included, so an override laid
		// over a shipped model below starts from it.
		mod.Maker = cmp.Or(mod.Maker, cat.ProviderMakers[mod.Provider])
		cat.Models[alias] = mod
		m.t.Models[alias] = mod
		m.t.modelOrigins[alias] = OriginShipped
	}
	for _, id := range slices.Sorted(maps.Keys(pd.Providers)) {
		m.provider(id, pd.Providers[id])
	}
	for _, alias := range slices.Sorted(maps.Keys(md.Models)) {
		m.model(alias, md.Models[alias])
	}
	m.makers()
	m.defaultModel(md.DefaultModel)
	m.t.ProviderOrder = userOrder(md.ProviderOrder)
	m.sourceOrder(md.SourceOrder)
	m.t.catalogOrder = slices.Clone(cat.ProviderOrder)
	m.t.starts = cat.startAliases()
	m.subagents(subagentsFromDoc(md.Subagents))
	m.t.credentialEnv = credentialNames(cat, pd)
	m.t.credentialKeys = credentialKeys(pd)
	m.t.catalogModels = catalogModels(cat, m.t.Models)
	return m.t
}

// catalogModels is every alias of models that is the catalog's own model
// (Resolved.CatalogModel): a shipped alias whose merged provider and wire
// model are still the shipped ones. nil for none.
func catalogModels(cat *Catalog, models map[string]Model) map[string]bool {
	var out map[string]bool
	for alias, mod := range models {
		if shipped, ok := cat.Models[alias]; ok && mod.Provider == shipped.Provider && mod.WireModel == shipped.WireModel {
			put(&out, alias, true)
		}
	}
	return out
}

// makers keeps the catalog's makers, and its env_key_by_name_only marks,
// where the user's providers.toml left the provider's endpoint as shipped
// (plan 050 C5): a provider moved to another driver or base_url is another
// source, so it loses its maker and its mark, and each of its models loses
// its maker — the same rule that keeps the shipped variables from funding it
// (overlayProvider). The endpoint is then the user's own, and so is what
// funds it.
func (m *merger) makers() {
	moved := func(id string) bool {
		shipped, ok := m.cat.Providers[id]
		p := m.t.Providers[id]
		return !ok || p.Driver != shipped.Driver || !sameEndpoint(p.BaseURL, shipped.BaseURL)
	}
	for id, maker := range m.cat.ProviderMakers {
		if !moved(id) {
			put(&m.t.providerMakers, id, maker)
		}
	}
	for id, on := range m.cat.EnvKeyByNameOnly {
		if on && !moved(id) {
			put(&m.t.envKeyByNameOnly, id, true)
		}
	}
	// A provider the catalog prices a model of is billed per token even when
	// the account's list has taken every priced one out of the table.
	for _, models := range []map[string]Model{m.cat.Models, m.cat.listHidden} {
		for _, mod := range models {
			if mod.Cost != nil && !moved(mod.Provider) {
				put(&m.t.billedProviders, mod.Provider, true)
			}
		}
	}
	for alias, mod := range m.t.Models {
		if mod.Maker != "" && moved(mod.Provider) {
			mod.Maker = ""
			m.t.Models[alias] = mod
		}
	}
}

// sourceOrder is the user's source_order over the catalog's (plan 050 C5):
// kept as written — checkOverlays refused a blank or repeated name — with
// one warning for each name that is no provider of the table, which the rule
// passes over.
func (m *merger) sourceOrder(written *[]string) {
	m.t.SourceOrder = userOrder(written)
	m.t.catalogSources = slices.Clone(m.cat.SourceOrder)
	for _, id := range m.t.SourceOrder {
		if !hasProvider(m.t.Providers, id) {
			m.warn(&FileError{File: m.mpath, Key: "source_order", Reason: fmt.Sprintf("%q is not a provider", id)}, "the name is passed over")
		}
	}
}

// deprecatedRef warns that the user's models.toml refers, at table and key,
// to alias, a deprecated model (plan 050 C1): one line per reference, naming
// the replacement. The reference still works; the retirement a later
// release makes drops it.
func (m *merger) deprecatedRef(table, key, alias string) {
	m.warn(&FileError{File: m.mpath, Table: table, Key: key,
		Reason: fmt.Sprintf("%q is deprecated; use %q", alias, m.t.Models[alias].ReplacedBy)},
		"it still runs, but a later craze release retires it and then drops this reference")
}

// userRef judges the reference the user's models.toml makes, at table and
// key, to alias — the pin, the [subagents] model or a tier target: true when
// alias is a merged model, which stands (with deprecatedRef's warning when it
// is deprecated); false when it is not, which the caller drops and which does
// instead — with retiredRef's word for a retired alias, a warning for
// anything else (plan 031 §3.2, §3.3; plan 050 C1).
func (m *merger) userRef(table, key, alias, instead string) bool {
	switch {
	case hasModel(m.t.Models, alias):
		if m.t.Models[alias].Deprecated {
			m.deprecatedRef(table, key, alias)
		}
		return true
	case m.retiredAlias[alias]:
		m.retiredRef(table, key, alias, instead)
	default:
		m.warn(&FileError{File: m.mpath, Table: table, Key: key, Reason: fmt.Sprintf("%q is not a model", alias)}, instead)
	}
	return false
}

// retiredRef warns that the user's models.toml refers, at table and key, to
// alias, a retired alias the catalog names a replacement for, and that the
// merge does instead (plan 050 C1). A reference to an alias retired with no
// replacement is dropped silently, as it always was (plan 031 §3.3).
func (m *merger) retiredRef(table, key, alias, instead string) {
	if r, _ := m.t.Retired(alias); r.ReplacedBy != "" {
		m.warn(&FileError{File: m.mpath, Table: table, Key: key,
			Reason: fmt.Sprintf("%q was retired; use %q", alias, r.ReplacedBy)}, instead)
	}
}

// startAliases is every provider's start as aliases of a merged table (plan
// 038 §2.2): each provider's Starts, the ChatGPT plan's [chatgpt_defaults]
// start as chatgpt/<slug>, and the Anthropic provider's models of
// [anthropic_defaults] start_family, newest first (anthropicStart, plan 044
// O1), each in order; nil for none.
func (c *Catalog) startAliases() map[string][]string {
	var out map[string][]string
	add := func(id string, aliases []string) {
		if len(aliases) == 0 {
			return
		}
		if out == nil {
			out = make(map[string][]string)
		}
		out[id] = aliases
	}
	for id, start := range c.Starts {
		add(id, slices.Clone(start))
	}
	var plan []string
	for _, s := range c.ChatGPT.Start {
		plan = append(plan, ChatGPTAliasPrefix+s)
	}
	add(ChatGPTProvider, plan)
	add(AnthropicProvider, c.anthropicStart())
	return out
}

// credentialNames is every variable name cat's providers and the user's
// providers.toml declare as holding a key, sorted and without repeats: what a
// merged table keeps apart from funding (Table.CredentialEnvNames, C2r2). The
// merge decides which of them a provider takes its key from — an entry that
// moves a shipped endpoint drops the shipped ones, an env_keys override
// replaces them, an entry that cannot stand is dropped whole — and none of
// that makes a name's value any less a credential. Every merged provider's
// names are among these, an old import's lent ones included, since each comes
// from the catalog or the file.
func credentialNames(cat *Catalog, pd *providersOverlay) []string {
	names := envKeyNames(cat.Providers)
	for _, o := range pd.Providers {
		names = append(names, deref(o.EnvKeys)...)
	}
	for _, o := range pd.withheld {
		names = append(names, deref(o.EnvKeys)...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// credentialKeys is every inline api_key the user's providers.toml writes,
// trimmed and without repeats: what a merged table redacts whether or not a
// merged provider still holds it (Table.Keys) — the key of an entry the merge
// dropped, or dropped from, is a credential all the same (C2r2's rule for
// variables, carried to keys). checkOverlays refused any the rules refuse.
func credentialKeys(pd *providersOverlay) []Secret {
	var keys []Secret
	entries := make([]providerOverlay, 0, len(pd.Providers)+len(pd.withheld))
	for _, id := range slices.Sorted(maps.Keys(pd.Providers)) {
		entries = append(entries, pd.Providers[id])
	}
	for _, o := range append(entries, pd.withheld...) {
		if k := o.APIKey; k != nil {
			v := strings.TrimSpace(*k)
			if v != "" && keyProblem(v) == nil && !slices.Contains(keys, Secret(v)) {
				keys = append(keys, Secret(v))
			}
		}
	}
	return keys
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
	switch shipped.Driver {
	case DriverChatGPT:
		// The ChatGPT plan's provider is funded by the sign-in and sends its
		// bearer to OpenAI's fixed endpoint alone (plan 033 §3.11, P37): an
		// entry may rename it and nothing else, so a key, a variable, an
		// endpoint or another driver written for it is dropped, each with a
		// warning, and craze's own is used — an old import's entry too.
		o = m.signInOverlay(id, o)
	case DriverAnthropic:
		// Anthropic's provider sends its key to Anthropic's fixed endpoint
		// alone (plan 044 P22): LoadWith locked the entry before anything
		// checked it (lockAnthropic), so it holds a name, variables and a key
		// at most, and nothing here can restore what it dropped — an old
		// import's source above all, whose path below would bring the shipped
		// variables back over an `env_keys = []` (P24).
	}
	if deref(o.Source) == legacySource {
		// An old import's copy of a provider craze now ships (§3.3): only
		// its key counts, and any variable names the catalog lacks, after
		// the catalog's own — a machine that relied on a gx-era variable
		// stays funded. Only when the entry is at the shipped endpoint,
		// though: a key an entry carries is used only at the endpoint the
		// entry is at after the merge, and one written for another endpoint
		// (a proxy's, say) never goes to craze's (plan 050 review).
		p := shipped
		p.EnvKeys = slices.Clone(shipped.EnvKeys)
		if movesEndpoint(shipped, o) {
			m.legacyKeyDropped(id, shipped, o)
			m.t.Providers[id] = p
			return
		}
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
		// the shipped half. The shipped endpoint is always valid. A key the
		// entry carries goes with a base_url of its own that is dropped: it
		// was written for that endpoint, not for craze's (plan 050 review).
		instead := "craze's shipped driver and base_url are used"
		if writesOtherEndpoint(o.BaseURL, shipped.BaseURL) && (o.APIKey != nil || o.EnvKeys != nil) {
			instead += ", and the entry's api_key and env_keys are dropped with its base_url, so its key is not sent to craze's endpoint"
			o.APIKey, o.EnvKeys = nil, nil
		}
		m.warn(err, instead)
		o.Driver, o.BaseURL = nil, nil
		p = overlayProvider(shipped, o)
	} else {
		if r := redundantProvider(shipped, o); r != "" {
			m.redundant(&FileError{File: m.ppath, Table: providerTable(id), Reason: r})
		}
		m.unfundedDriver(id, shipped, o, p)
	}
	m.t.Providers[id] = p
	m.t.providerOrigins[id] = OriginOverridden
}

// movesEndpoint reports whether o, an old import's entry (source = "gx") for
// a shipped provider, names another endpoint than shipped's: a base_url it
// writes that is not the shipped one (sameEndpoint). Its driver never did —
// the legacy rule has always ignored an old import's driver, so an entry that
// writes one and no base_url (openai-compat for Meta, say) still lends its
// key to the shipped endpoint, as it always has.
func movesEndpoint(shipped Provider, o providerOverlay) bool {
	return writesOtherEndpoint(o.BaseURL, shipped.BaseURL)
}

// writesOtherEndpoint reports whether written, a base_url an entry writes (nil
// for none), names another endpoint than shipped: one it writes, not empty —
// an empty base_url names no endpoint at all (plan 050 review) — and not the
// same endpoint once both are normalised (sameEndpoint).
func writesOtherEndpoint(written *string, shipped string) bool {
	return written != nil && *written != "" && !sameEndpoint(*written, shipped)
}

// sameEndpoint reports whether base URLs a and b address one endpoint: equal
// once each is normalised (normalEndpoint). It is the merge's one test of
// "the same endpoint": whether a key an entry carries may go there
// (writesOtherEndpoint), whether the shipped variables still fund an entry
// (overlayProvider), whether a provider keeps its maker (makers), and which
// env_keys an unfunded driver change is told to add (unfundedDriver).
func sameEndpoint(a, b string) bool { return normalEndpoint(a) == normalEndpoint(b) }

// normalEndpoint is base URL u with what does not change where a request goes
// taken out: the scheme and the host in lower case, the scheme's default port
// (443 for https, 80 for http) elided, and a trailing slash on the path
// dropped (the clients trim it themselves). The path's case, the user info, a
// query and a fragment stay as written. A value that does not parse is
// itself.
func normalEndpoint(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return u
	}
	p.Scheme = strings.ToLower(p.Scheme)
	host, port := strings.ToLower(p.Hostname()), p.Port()
	if (p.Scheme == "https" && port == "443") || (p.Scheme == "http" && port == "80") {
		port = ""
	}
	p.Host = host
	if port != "" {
		p.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		p.Host = "[" + host + "]"
	}
	p.Path = strings.TrimSuffix(p.Path, "/")
	p.RawPath = strings.TrimSuffix(p.RawPath, "/")
	return p.String()
}

// legacyKeyDropped warns that o, an old import's entry (source = "gx") for
// the shipped provider id that points it at another endpoint, lends it
// nothing: neither its api_key nor any variable name the catalog lacks, which
// were written for that endpoint (plan 050 review). One warning, naming the
// provider and the keys, never a value; none when the entry carries neither.
// The key and the names are still credentials (Table.Keys,
// CredentialEnvNames): redacted, and kept from the commands a session runs.
func (m *merger) legacyKeyDropped(id string, shipped Provider, o providerOverlay) {
	var keys []string
	if o.APIKey != nil {
		keys = append(keys, "api_key")
	}
	for _, name := range deref(o.EnvKeys) {
		if !slices.Contains(shipped.EnvKeys, name) {
			keys = append(keys, "env_keys")
			break
		}
	}
	if len(keys) == 0 {
		return
	}
	m.warn(&FileError{File: m.ppath, Table: providerTable(id), Key: strings.Join(keys, " and "),
		Reason: `dropped: this old import's entry (source = "gx") points the provider at another endpoint than craze's, so its key is not sent to craze's endpoint`},
		fmt.Sprintf("craze's shipped %s provider is used without it; remove source = \"gx\" from the entry to use it as written", id))
}

// unfundedDriver warns when o, an entry for a shipped provider, moves it to
// another driver and so — by design, the shipped variables belonging to the
// shipped driver's endpoint (overlayProvider) — takes none of its env_keys,
// while writing no env_keys and no api_key of its own: the load stands, but
// nothing funds the provider (plan 047 C7 review). That is the upgrade a
// user's own driver = "openai-compat" for Meta met when Meta's shipped driver
// became openai-responses: before, it inherited META_API_KEY. An explicit
// env_keys, even an empty one, is the user's own choice and is not warned
// about. The warning names the provider and the variables, never a value.
func (m *merger) unfundedDriver(id string, shipped Provider, o providerOverlay, p Provider) {
	if o.Driver == nil || p.Driver == shipped.Driver || len(shipped.EnvKeys) == 0 {
		return // not a driver change, or one that had nothing to inherit
	}
	if len(p.EnvKeys) != 0 || p.APIKey != "" || o.EnvKeys != nil {
		return // funded, or an env_keys of the user's own, even an empty one
	}
	add := "env_keys naming the variable that holds its key"
	if sameEndpoint(p.BaseURL, shipped.BaseURL) {
		names := make([]string, len(shipped.EnvKeys))
		for i, n := range shipped.EnvKeys {
			names[i] = strconv.Quote(n)
		}
		add = "env_keys = [" + strings.Join(names, ", ") + "]"
	}
	m.warn(&FileError{File: m.ppath, Table: providerTable(id), Key: "env_keys",
		Reason: fmt.Sprintf("missing: driver %q is not craze's shipped driver for this provider, so the entry takes none of its key variables, and it has no api_key", p.Driver)},
		fmt.Sprintf("nothing funds %s until you add %s, or store a key with \"craze auth login %s\"", id, add, id))
}

// signInOverlay is o, an entry of the user's for the ChatGPT plan's shipped
// provider, with every key but its name and source dropped — the driver when
// it is not the plan's, base_url, env_keys and api_key — each with a warning
// naming the key, never its value.
func (m *merger) signInOverlay(id string, o providerOverlay) providerOverlay {
	drop := func(key, reason string) {
		m.warn(&FileError{File: m.ppath, Table: providerTable(id), Key: key, Reason: reason}, "craze's own ChatGPT plan provider is used")
	}
	if o.Driver != nil && *o.Driver != DriverChatGPT {
		drop("driver", fmt.Sprintf("the ChatGPT plan's provider is always driver %q", DriverChatGPT))
		o.Driver = nil
	}
	if o.BaseURL != nil {
		drop("base_url", fixedEndpoint(DriverChatGPT))
		o.BaseURL = nil
	}
	if o.EnvKeys != nil {
		drop("env_keys", signInKeyReason)
		o.EnvKeys = nil
	}
	if o.APIKey != nil {
		drop("api_key", signInKeyReason)
		o.APIKey = nil
	}
	return o
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
		m.unreachableTier(alias, o, &mod)
	} else {
		mod = o.complete()
	}
	_, hidden := m.cat.listHidden[alias]
	if m.retiredID[identity{mod.Provider, mod.WireModel}] {
		// A dead wire id is dead whoever wrote it (§3.3). For a shipped alias
		// the shipped definition stays (r2-6); only the user's entry goes —
		// with a word when it is a retired alias's entry and the catalog
		// names the alias's replacement (plan 050 C1).
		if retiredAlias {
			m.retiredRef(modelTable(alias), "", alias, "the entry is ignored")
		}
		return
	}
	if !isShipped && (mod.Provider == "" || mod.WireModel == "") {
		// A retired alias's leftover override goes silently — unless the
		// catalog names its replacement (plan 050 C1) — as does one of an
		// Anthropic model the account's list hides (plan 044 §3.5): the entry
		// stands again when the model is offered again.
		if retiredAlias {
			m.retiredRef(modelTable(alias), "", alias, "the entry is ignored")
		} else if !hidden {
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
	if mod.Deprecated {
		// An override of a deprecated model that still is one: the entry
		// does not re-point the alias (plan 050 C1).
		m.deprecatedRef(modelTable(alias), "", alias)
	}
	if clean && sameAsShippedModel(shipped, o) {
		m.redundant(&FileError{File: m.mpath, Table: modelTable(alias),
			Reason: "repeats craze's shipped entry — delete it to follow craze's updates"})
	}
}

// unreachableTier turns off the long-context tier mod, a shipped model with
// the user's entry o laid over it, inherited, when the entry lowered its
// context_window below the tier's threshold without writing
// long_context_above (plan 050 C3): no prompt can reach the tier then, and
// left on it would fail the merged model's validation (validateTier), which
// drops the whole entry. The entry stands, its tier off as
// `long_context_above = 0` turns it off — which is how Save writes it — and
// one warning names the key.
func (m *merger) unreachableTier(alias string, o modelOverlay, mod *Model) {
	c := mod.Cost
	if c == nil || o.ContextWindow == nil || (o.Cost != nil && o.Cost.LongContextAbove != nil) {
		return
	}
	above := deref(c.LongContextAbove)
	if above <= 0 || mod.ContextWindow <= 0 || above <= int64(mod.ContextWindow) {
		return
	}
	c.LongContextAbove, c.LongContext = new(int64), nil
	m.warn(&FileError{File: m.mpath, Table: costTable(alias), Key: "long_context_above",
		Reason: fmt.Sprintf("craze's shipped %d is above the entry's context_window, %d: no prompt could reach it", above, mod.ContextWindow)},
		"the long-context tier is off; write long_context_above to keep one")
}

// sameAsShippedModel reports whether every field o writes (source aside)
// equals the shipped value (plan 031 §3.3): the cost's rates, its
// long_context_above and its long-context rates each on their own (plan 047
// D10).
func sameAsShippedModel(s Model, o modelOverlay) bool {
	if differs(o.Provider, s.Provider) || differs(o.WireModel, s.WireModel) ||
		differs(o.Name, s.Name) || differs(o.ContextWindow, s.ContextWindow) ||
		differs(o.DefaultContext, s.DefaultContext) ||
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
	if !writesShipped(o.Cost.Input, sc.Input) || !writesShipped(o.Cost.Output, sc.Output) ||
		!writesShipped(o.Cost.CacheRead, sc.CacheRead) || !writesShipped(o.Cost.CacheWrite, sc.CacheWrite) ||
		!writesShipped(o.Cost.LongContextAbove, sc.LongContextAbove) {
		return false
	}
	ol := o.Cost.LongContext
	if ol == nil {
		return true
	}
	var sl LongContextCost
	if sc.LongContext != nil {
		sl = *sc.LongContext
	}
	return writesShipped(ol.Input, sl.Input) && writesShipped(ol.Output, sl.Output) &&
		writesShipped(ol.CacheRead, sl.CacheRead) && writesShipped(ol.CacheWrite, sl.CacheWrite)
}

// writesShipped reports whether a key the user's entry may write, written,
// equals the shipped value: true when it is left out.
func writesShipped[T comparable](written, shipped *T) bool {
	return written == nil || (shipped != nil && *written == *shipped)
}

// defaultModel is the user's default_model — their pin (plan 038 §2.3) — when
// it names a merged model, and otherwise none: dropped silently when it names
// a retired alias, with a warning for anything else (plan 031 §3.2). A pin
// on a deprecated model stands with a warning, and one on a retired alias
// whose replacement the catalog names is dropped with one (plan 050 C1). The
// catalog has no default of its own; with no pin a new session starts by the
// provider order (StartModel).
func (m *merger) defaultModel(user *string) {
	if user != nil && m.userRef("", "default_model", *user, "a new session starts by craze's provider order") {
		m.t.DefaultModel = *user
	}
}

// subagents degrades [subagents] instead of failing the load (plan 031 §3.3):
// a model or tier target that is not a merged model is dropped — silently for
// a retired alias with no replacement named (plan 050 C1) — falling through
// to plan 026 §3.6's defaults, and an effort the model it is set with does
// not offer is dropped with a warning. A model or tier target that is a
// deprecated model stands, with a warning naming its replacement.
func (m *merger) subagents(s Subagents) {
	if s.Model != "" && !m.userRef("subagents", "model", s.Model, "a sub-agent defaults to its parent's model") {
		s.Model = ""
	}
	for _, tier := range slices.Sorted(maps.Keys(s.Tiers)) {
		if !m.userRef("subagents.tiers", tier, s.Tiers[tier], "the tier means the parent's model") {
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
// writes equals the shipped value, and a provider entry whose endpoint fields
// do. Each is legal but pins those
// values against later catalog updates, so `craze auth list` names it. One
// line each, located like a FileError, in file order; never a key.
func (t *Table) RedundantOverrides() []string {
	return slices.Clone(t.redundant)
}
