package modeltable

import (
	_ "embed"
	"maps"
	"slices"
	"strconv"
	"sync"
)

// catalogTOML is catalog.toml, the model catalog craze ships (plan 031 §3.1),
// compiled into the binary: a release carries the catalog it was built with,
// and an upgrade is how a machine gets a new one. In development it is the
// checked-in file.
//
//go:embed catalog.toml
var catalogTOML []byte

// CatalogFile is the shipped catalog's name, in errors about it.
const CatalogFile = "catalog.toml"

// Catalog is a model table craze ships (plan 031 §3.1): providers without
// keys, models, the model a session starts on, and the aliases a release has
// retired. Load merges the user's two files over the one compiled into the
// binary; LoadWith takes another, so a test can pass its own. It holds no key
// and no source: its providers' APIKey and Source and its models' Source are
// always empty.
type Catalog struct {
	DefaultModel string
	Providers    map[string]Provider
	Models       map[string]Model
	Retired      []Retired
}

// Retired is one alias a release took out of the catalog, with the provider
// it was on and every wire model id it has pointed at (plan 031 §3.3). An
// entry an old import left for the alias is ignored, a [subagents] or
// default_model naming it falls back silently, and any user model whose
// (provider, wire model) is one of these identities is dropped silently.
type Retired struct {
	Alias      string
	Provider   string
	WireModels []string
}

// The catalog's own on-disk shape. It has no api_key, source, catalog,
// [subagents] or [compaction] key, so decodeStrict refuses any of them: the
// schema itself keeps a key, or a user-only setting, out of the file.
type catalogDoc struct {
	Version      int                             `toml:"version"`
	DefaultModel string                          `toml:"default_model"`
	Retired      []catalogRetiredEntry           `toml:"retired"`
	Providers    map[string]catalogProviderEntry `toml:"providers"`
	Models       map[string]catalogModelEntry    `toml:"models"`
}

func (d *catalogDoc) version() int { return d.Version }

type catalogRetiredEntry struct {
	Alias      string   `toml:"alias"`
	Provider   string   `toml:"provider"`
	WireModels []string `toml:"wire_models"`
}

type catalogProviderEntry struct {
	Name    string   `toml:"name"`
	Driver  string   `toml:"driver"`
	BaseURL string   `toml:"base_url"`
	EnvKeys []string `toml:"env_keys"`
}

type catalogModelEntry struct {
	Provider        string   `toml:"provider"`
	WireModel       string   `toml:"wire_model"`
	Name            string   `toml:"name"`
	ContextWindow   int      `toml:"context_window"`
	MaxOutputTokens int      `toml:"max_output_tokens"`
	Efforts         []string `toml:"efforts"`
	DefaultEffort   string   `toml:"default_effort"`
	Vision          bool     `toml:"vision"`
	ToolProfile     string   `toml:"tool_profile"`
	Cost            *Cost    `toml:"cost"`
}

// parseCatalog strict-decodes one catalog file. It does not validate the
// catalog's contents — (*Catalog).validate does, and TestShippedCatalog runs
// it on the shipped one — so a merge over a broken catalog fails where the
// merged table is validated, naming the entry.
func parseCatalog(name string, data []byte) (*Catalog, error) {
	var d catalogDoc
	if err := decodeStrict(name, data, &d); err != nil {
		return nil, err
	}
	c := &Catalog{
		DefaultModel: d.DefaultModel,
		Providers:    make(map[string]Provider, len(d.Providers)),
		Models:       make(map[string]Model, len(d.Models)),
	}
	for id, e := range d.Providers {
		c.Providers[id] = Provider{Name: e.Name, Driver: e.Driver, BaseURL: e.BaseURL, EnvKeys: nilIfEmpty(e.EnvKeys)}
	}
	for alias, e := range d.Models {
		c.Models[alias] = Model{
			Provider: e.Provider, WireModel: e.WireModel, Name: e.Name,
			ContextWindow: e.ContextWindow, MaxOutputTokens: e.MaxOutputTokens,
			Efforts: nilIfEmpty(e.Efforts), DefaultEffort: e.DefaultEffort,
			Vision: e.Vision, ToolProfile: e.ToolProfile, Cost: e.Cost,
		}
	}
	for _, r := range d.Retired {
		c.Retired = append(c.Retired, Retired{Alias: r.Alias, Provider: r.Provider, WireModels: nilIfEmpty(r.WireModels)})
	}
	return c, nil
}

// The shipped catalog, decoded once (plan 031 §3.1). Nothing hands out this
// copy: Load and ShippedCatalog each take a deep copy of it.
var shipped struct {
	once sync.Once
	cat  *Catalog
	err  error
}

func shippedCatalog() (*Catalog, error) {
	shipped.once.Do(func() {
		shipped.cat, shipped.err = parseCatalog(CatalogFile, catalogTOML)
	})
	return shipped.cat, shipped.err
}

// ShippedCatalog is a copy of the catalog compiled into this binary: the
// caller may change it freely. Its error is the embedded file's decode error,
// which TestShippedCatalog keeps from ever shipping.
func ShippedCatalog() (*Catalog, error) {
	c, err := shippedCatalog()
	if err != nil {
		return nil, err
	}
	return c.Clone(), nil
}

// CatalogEnvNames is every environment variable name the shipped catalog's
// providers take a key from, sorted and without repeats. Tests unset them
// (plan 031 §3.13): the catalog brings these names into every table, so a key
// exported in the developer's shell would otherwise fund a test's session. It
// is nil only when the embedded catalog does not decode, which
// TestShippedCatalog fails on.
func CatalogEnvNames() []string {
	c, err := shippedCatalog()
	if err != nil {
		return nil
	}
	return envKeyNames(c.Providers)
}

// Clone is a deep copy of c: no map, slice or pointer of the copy is c's, so a
// table merged from it can be changed without changing c (plan 031 §3.1). A
// nil c clones to nil.
func (c *Catalog) Clone() *Catalog {
	if c == nil {
		return nil
	}
	out := &Catalog{
		DefaultModel: c.DefaultModel,
		Providers:    make(map[string]Provider, len(c.Providers)),
		Models:       make(map[string]Model, len(c.Models)),
	}
	for id, p := range c.Providers {
		p.EnvKeys = slices.Clone(p.EnvKeys)
		out.Providers[id] = p
	}
	for alias, m := range c.Models {
		out.Models[alias] = cloneModel(m)
	}
	for _, r := range c.Retired {
		r.WireModels = slices.Clone(r.WireModels)
		out.Retired = append(out.Retired, r)
	}
	return out
}

// cloneModel is m with slices and pointers of its own.
func cloneModel(m Model) Model {
	m.Efforts = slices.Clone(m.Efforts)
	m.Cost = m.Cost.Clone()
	m.ParallelToolCalls = clonePtr(m.ParallelToolCalls)
	return m
}

// empty reports whether c ships nothing: LoadWith treats a nil or empty
// catalog as none at all, today's behaviour.
func (c *Catalog) empty() bool {
	return c == nil || (len(c.Providers) == 0 && len(c.Models) == 0 && len(c.Retired) == 0)
}

// retiredAliases is the set of c's retired aliases.
func (c *Catalog) retiredAliases() map[string]bool {
	out := make(map[string]bool, len(c.Retired))
	for _, r := range c.Retired {
		out[r.Alias] = true
	}
	return out
}

// retiredIdentities is the set of every (provider, wire model) c retired.
func (c *Catalog) retiredIdentities() map[identity]bool {
	out := make(map[identity]bool)
	for _, r := range c.Retired {
		for _, w := range r.WireModels {
			out[identity{r.Provider, w}] = true
		}
	}
	return out
}

func hasModel(models map[string]Model, alias string) bool {
	_, ok := models[alias]
	return ok
}

// validate checks every rule the merge relies on (plan 031 §3.1), in a fixed
// order so the same catalog always reports the same problem: each provider
// has a name, a valid driver and base URL, and at least one env_keys name and
// no key; each model validates against the catalog's own providers, as a
// models.toml entry would; default_model is a model; no two aliases share an
// identity; the retired aliases are unique, none is a model, and no model's
// identity is a retired one. Errors are *FileErrors against name.
func (c *Catalog) validate(name string) error {
	at := func(table, key, reason string) error {
		return &FileError{File: name, Table: table, Key: key, Reason: reason}
	}
	for _, id := range slices.Sorted(maps.Keys(c.Providers)) {
		p := c.Providers[id]
		if err := validateProvider(name, id, p); err != nil {
			return err
		}
		table := providerTable(id)
		switch {
		case p.Name == "":
			return at(table, "name", "missing: every shipped provider has a display name")
		case len(p.EnvKeys) == 0:
			return at(table, "env_keys", "missing: every shipped provider names at least one variable a key comes from")
		case p.APIKey != "":
			return at(table, "api_key", "the catalog never holds a key")
		case p.Source != "":
			return at(table, "source", "the catalog has no source")
		}
	}
	byIdentity := make(map[identity]string, len(c.Models))
	for _, alias := range slices.Sorted(maps.Keys(c.Models)) {
		m := c.Models[alias]
		if err := validateModel(name, alias, m, c.Providers); err != nil {
			return err
		}
		if m.Source != "" {
			return at(modelTable(alias), "source", "the catalog has no source")
		}
		id := identity{m.Provider, m.WireModel}
		if other, ok := byIdentity[id]; ok {
			return at(modelTable(alias), "wire_model",
				"is "+other+"'s identity too: two shipped aliases of one (provider, wire model) would price and match as one")
		}
		byIdentity[id] = alias
	}
	if !hasModel(c.Models, c.DefaultModel) {
		return at("", "default_model", "not a model in the catalog")
	}
	seen := make(map[string]bool, len(c.Retired))
	for i, r := range c.Retired {
		table := "retired"
		switch {
		case r.Alias == "":
			return at(table, "alias", "entry "+strconv.Itoa(i+1)+" has no alias")
		case seen[r.Alias]:
			return at(table, "alias", r.Alias+" is retired twice")
		case hasModel(c.Models, r.Alias):
			return at(table, "alias", r.Alias+" is still a model: a retired alias is one the catalog no longer ships")
		case r.Provider == "":
			return at(table, "provider", r.Alias+" names no provider")
		}
		seen[r.Alias] = true
		for _, w := range r.WireModels {
			if alias, ok := byIdentity[identity{r.Provider, w}]; ok {
				return at(table, "wire_models", r.Alias+"'s "+w+" is shipped model "+alias+"'s identity")
			}
		}
	}
	return nil
}
