package modeltable

import (
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/BurntSushi/toml"
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
// keys, models, the order a new session tries the providers in and the model
// each starts on (plan 038 §2), and the aliases a release has retired. Load
// merges the user's two files over the one compiled into the binary; LoadWith
// takes another, so a test can pass its own. It holds no key and no source:
// its providers' APIKey and Source and its models' Source are always empty.
// It has no default model: default_model is the user's own pin (models.toml).
type Catalog struct {
	// ProviderOrder is provider_order (plan 038 §2.1): the providers a new
	// session with nothing remembered and no funded pin tries, in order, each
	// on the first of its Starts that resolves (Table.StartModel). validate
	// requires it to name every provider exactly once and nothing else.
	ProviderOrder []string
	Providers     map[string]Provider
	Models        map[string]Model
	// Starts is each provider's [providers.<id>] start (plan 038 §2.2): the
	// provider's own catalog aliases, in order of preference, keyed by
	// provider id; a provider with none has no key here. The ChatGPT plan's
	// start is never here: its models are the account's, so it stays
	// [chatgpt_defaults] start, slugs (ChatGPT.Start).
	Starts  map[string][]string
	Retired []Retired
	// ChatGPT is [chatgpt_defaults] (plan 033 §3.11): craze's own settings for
	// the ChatGPT plan's models, which a load learns from the account's list
	// (withDiscovered) and never ships, so they are no [models] rows — no
	// history line, no [[retired]] row, nothing shown for an account that does
	// not list them. Its zero value is no section.
	ChatGPT ChatGPTDefaults
}

// ChatGPTDefaults is the catalog's [chatgpt_defaults] (plan 033 §3.11).
type ChatGPTDefaults struct {
	// Start is the ChatGPT plan's start (plan 038 §2.2): the slugs a new
	// session with nothing remembered and no funded pin starts on among the
	// plan's models when the plan's turn in provider_order comes (StartModel),
	// in order of preference: the first one the account lists and the sign-in
	// funds wins (plan 034 Q4). Nil is no preference; present, it is non-empty
	// and unique (validateChatGPT). It is catalog-only, so it is an array
	// outright.
	Start []string
	// startSet says the file wrote start, even as an empty array, so
	// validation can refuse `start = []` and still read an absent key as no
	// preference.
	startSet bool
	// ModelsClientVersion is the client_version every request for the plan's
	// model list carries (plan 034 §3.1, D-82): the server gates the list on
	// it, so craze claims one version it has tested. Required when the
	// catalog ships the chatgpt provider on its driver; dotted numeric, one
	// to three parts of one to six digits. It is the downgrade-safe reader's
	// pin too (withDiscovered).
	ModelsClientVersion string
	// Models are craze's settings for a slug, laid over what the account's
	// list says of it; nil for none.
	Models map[string]ChatGPTModelDefaults
}

// ChatGPTModelDefaults are craze's settings for one plan model's slug: each
// zero value leaves the list's own.
type ChatGPTModelDefaults struct {
	// Name is the name shown before " (ChatGPT plan)" in place of the list's
	// display name.
	Name string
	// Efforts are the efforts offered, in order, among those the list offers;
	// nil, or empty, offers the list's own.
	Efforts []string
	// DefaultEffort is the effort a session on the model starts at, when the
	// model offers it; else the list's default_reasoning_level.
	DefaultEffort string
	// ToolProfile is the model's tool profile ("" is the default).
	ToolProfile string
}

// Retired is one alias a release took out of the catalog, with the provider
// it was on and every wire model id it has pointed at (plan 031 §3.3). An
// entry an old import left for the alias is ignored, a [subagents] model or
// the user's default_model naming it is dropped silently, and any user model whose
// (provider, wire model) is one of these identities is dropped silently.
type Retired struct {
	Alias      string
	Provider   string
	WireModels []string
}

// The catalog's own on-disk shape. It has no api_key, source, catalog,
// default_model, [subagents] or [compaction] key, so decodeStrict refuses any
// of them: the schema itself keeps a key, or a user-only setting, out of the
// file (default_model is the user's pin since plan 038 §2.3).
type catalogDoc struct {
	Version       int                             `toml:"version"`
	ProviderOrder []string                        `toml:"provider_order"`
	Retired       []catalogRetiredEntry           `toml:"retired"`
	Providers     map[string]catalogProviderEntry `toml:"providers"`
	Models        map[string]catalogModelEntry    `toml:"models"`
	// ChatGPT is [chatgpt_defaults], decoded as strictly as the rest: a slug's
	// table takes name, efforts, default_effort and tool_profile, and nothing
	// else (plan 033 §3.11).
	ChatGPT *catalogChatGPTDoc `toml:"chatgpt_defaults"`
}

type catalogChatGPTDoc struct {
	Start               []string                          `toml:"start"`
	ModelsClientVersion string                            `toml:"models_client_version"`
	Models              map[string]catalogChatGPTModelDoc `toml:"models"`
}

type catalogChatGPTModelDoc struct {
	Name          string   `toml:"name"`
	Efforts       []string `toml:"efforts"`
	DefaultEffort string   `toml:"default_effort"`
	ToolProfile   string   `toml:"tool_profile"`
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
	// Start is catalog-only (plan 038 §2.2): no user file has the key.
	Start []string `toml:"start"`
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
		ProviderOrder: nilIfEmpty(d.ProviderOrder),
		Providers:     make(map[string]Provider, len(d.Providers)),
		Models:        make(map[string]Model, len(d.Models)),
	}
	for id, e := range d.Providers {
		c.Providers[id] = Provider{Name: e.Name, Driver: e.Driver, BaseURL: e.BaseURL, EnvKeys: nilIfEmpty(e.EnvKeys)}
		if e.Start != nil {
			// Written, even as `start = []`, which validate refuses.
			if c.Starts == nil {
				c.Starts = make(map[string][]string)
			}
			c.Starts[id] = slices.Clone(e.Start)
		}
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
	if g := d.ChatGPT; g != nil {
		c.ChatGPT.Start = g.Start
		c.ChatGPT.startSet = g.Start != nil
		c.ChatGPT.ModelsClientVersion = g.ModelsClientVersion
		for slug, e := range g.Models {
			if c.ChatGPT.Models == nil {
				c.ChatGPT.Models = make(map[string]ChatGPTModelDefaults, len(g.Models))
			}
			c.ChatGPT.Models[slug] = ChatGPTModelDefaults{Name: e.Name, Efforts: nilIfEmpty(e.Efforts),
				DefaultEffort: e.DefaultEffort, ToolProfile: e.ToolProfile}
		}
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

// ChatGPTModelsClientVersion is the client_version the shipped catalog pins
// for every request for the ChatGPT plan's model list (plan 034 §3.1, D-82):
// the harness may not import chatgptauth, and chatgptauth may not import this
// package, so a caller passes it from one to the other. It is "" only when
// the embedded catalog does not decode, which TestShippedCatalog fails on.
func ChatGPTModelsClientVersion() string {
	c, err := shippedCatalog()
	if err != nil {
		return ""
	}
	return c.ChatGPT.ModelsClientVersion
}

// Clone is a deep copy of c: no map, slice or pointer of the copy is c's, so a
// table merged from it can be changed without changing c (plan 031 §3.1). A
// nil c clones to nil.
func (c *Catalog) Clone() *Catalog {
	if c == nil {
		return nil
	}
	out := &Catalog{
		ProviderOrder: slices.Clone(c.ProviderOrder),
		Providers:     make(map[string]Provider, len(c.Providers)),
		Models:        make(map[string]Model, len(c.Models)),
	}
	for id, p := range c.Providers {
		p.EnvKeys = slices.Clone(p.EnvKeys)
		out.Providers[id] = p
	}
	for id, start := range c.Starts {
		if out.Starts == nil {
			out.Starts = make(map[string][]string, len(c.Starts))
		}
		out.Starts[id] = slices.Clone(start)
	}
	for alias, m := range c.Models {
		out.Models[alias] = cloneModel(m)
	}
	for _, r := range c.Retired {
		r.WireModels = slices.Clone(r.WireModels)
		out.Retired = append(out.Retired, r)
	}
	out.ChatGPT.Start = slices.Clone(c.ChatGPT.Start)
	out.ChatGPT.startSet = c.ChatGPT.startSet
	out.ChatGPT.ModelsClientVersion = c.ChatGPT.ModelsClientVersion
	for slug, d := range c.ChatGPT.Models {
		if out.ChatGPT.Models == nil {
			out.ChatGPT.Models = make(map[string]ChatGPTModelDefaults, len(c.ChatGPT.Models))
		}
		d.Efforts = slices.Clone(d.Efforts)
		out.ChatGPT.Models[slug] = d
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
// no key — but the ChatGPT plan's, which signs in and names no variable (plan
// 033 §3.11); each model validates against the catalog's own providers, as a
// models.toml entry would; no two aliases share an identity; provider_order
// and each provider's start are valid (validateStarts, plan 038 §2); the
// retired aliases are unique, none is a model, and no model's identity is a
// retired one; and [chatgpt_defaults] is valid (validateChatGPT). Errors are
// *FileErrors against name.
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
		case len(p.EnvKeys) == 0 && p.Driver != DriverChatGPT:
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
	if err := c.validateStarts(name); err != nil {
		return err
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
	return c.validateChatGPT(name)
}

// validateStarts checks where a new session with nothing remembered starts
// (plan 038 §2): provider_order is there and names every provider exactly
// once and nothing else; and each provider's start — never the ChatGPT
// plan's, which is [chatgpt_defaults] start — is a non-empty list of models
// of that provider, each listed once. The order is checked entry by entry,
// then the providers it misses, sorted; the starts by provider, sorted.
func (c *Catalog) validateStarts(name string) error {
	at := func(table, key, reason string) error {
		return &FileError{File: name, Table: table, Key: key, Reason: reason}
	}
	if len(c.ProviderOrder) == 0 {
		return at("", "provider_order", "missing: list every provider, in the order a new session with nothing remembered tries them")
	}
	for i, id := range c.ProviderOrder {
		switch _, ok := c.Providers[id]; {
		case !ok:
			return at("", "provider_order", fmt.Sprintf("entry %d, %q, is not a provider in the catalog", i+1, id))
		case slices.Contains(c.ProviderOrder[:i], id):
			return at("", "provider_order", fmt.Sprintf("lists %q twice", id))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(c.Providers)) {
		if !slices.Contains(c.ProviderOrder, id) {
			return at("", "provider_order", fmt.Sprintf("does not list provider %q: name every provider once", id))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(c.Starts)) {
		start, table := c.Starts[id], providerTable(id)
		switch p, ok := c.Providers[id]; {
		case !ok:
			// Unreachable from a file, whose start sits in its provider's
			// own table; a Catalog built in memory can still say it.
			return at(table, "start", "is for a provider the catalog does not have")
		case p.Driver == DriverChatGPT:
			return at(table, "start", "the ChatGPT plan's start is [chatgpt_defaults] start, its account's model slugs")
		case len(start) == 0:
			return at(table, "start", "is empty: list at least one of the provider's models, or leave start out")
		}
		for i, alias := range start {
			m, ok := c.Models[alias]
			switch {
			case !ok:
				return at(table, "start", fmt.Sprintf("entry %d, %q, is not a model in the catalog", i+1, alias))
			case m.Provider != id:
				return at(table, "start", fmt.Sprintf("entry %d, %q, is a model of provider %q, not of %q", i+1, alias, m.Provider, id))
			case slices.Contains(start[:i], alias):
				return at(table, "start", fmt.Sprintf("lists %q twice", alias))
			}
		}
	}
	return nil
}

// validateChatGPT checks [chatgpt_defaults] (plan 033 §3.11): it is only for a
// catalog that ships the chatgpt provider on its driver; start, and every
// slug it has settings for, is a slug craze can name; a name is one line of
// text; each effort is one a ChatGPT plan request can carry, listed once; a
// default effort is one of them (and of efforts, when the slug lists them);
// and a tool profile is one craze has. Slugs are checked in sorted order.
func (c *Catalog) validateChatGPT(name string) error {
	d := c.ChatGPT
	at := func(table, key, reason string) error {
		return &FileError{File: name, Table: table, Key: key, Reason: reason}
	}
	p, shipsChatGPT := c.Providers[ChatGPTProvider]
	shipsChatGPT = shipsChatGPT && p.Driver == DriverChatGPT
	if shipsChatGPT && d.ModelsClientVersion == "" {
		return at("chatgpt_defaults", "models_client_version", "missing: the catalog ships the "+ChatGPTProvider+" provider, whose model list the server gates on a client version (plan 034 §3.1)")
	}
	if !d.startSet && len(d.Start) == 0 && d.ModelsClientVersion == "" && len(d.Models) == 0 {
		return nil
	}
	if !shipsChatGPT {
		return at("chatgpt_defaults", "", fmt.Sprintf("no provider %q on driver %q ships for these settings to apply to", ChatGPTProvider, DriverChatGPT))
	}
	if v := d.ModelsClientVersion; !validPinVersion(v) {
		return at("chatgpt_defaults", "models_client_version", fmt.Sprintf("%q is not a dotted version: one to three numeric parts of one to six digits, as 0.160.0", v))
	}
	if d.startSet && len(d.Start) == 0 {
		return at("chatgpt_defaults", "start", "is empty: list at least one model slug, or leave start out for no preference")
	}
	for i, s := range d.Start {
		switch {
		case !chatgptSlug.MatchString(s):
			return at("chatgpt_defaults", "start", fmt.Sprintf("entry %d is not a model slug", i+1))
		case slices.Contains(d.Start[:i], s):
			return at("chatgpt_defaults", "start", fmt.Sprintf("lists %s twice", s))
		}
	}
	for _, slug := range slices.Sorted(maps.Keys(d.Models)) {
		m := d.Models[slug]
		table := toml.Key{"chatgpt_defaults", "models", slug}.String()
		switch {
		case !chatgptSlug.MatchString(slug):
			return at(table, "", "not a model slug")
		case m.Name != "" && !shownText(m.Name, 128):
			return at(table, "name", "not one line of text")
		}
		if r := effortsProblem(m.Efforts); r != "" {
			return at(table, "efforts", r)
		}
		for _, e := range m.Efforts {
			if !slices.Contains(chatgptEfforts, e) {
				return at(table, "efforts", fmt.Sprintf("%q is not an effort a ChatGPT plan request can carry", e))
			}
		}
		switch {
		case m.DefaultEffort != "" && !slices.Contains(chatgptEfforts, m.DefaultEffort):
			return at(table, "default_effort", fmt.Sprintf("%q is not an effort a ChatGPT plan request can carry", m.DefaultEffort))
		case m.Efforts != nil && defaultEffortProblem(m.Efforts, m.DefaultEffort) != "":
			return at(table, "default_effort", defaultEffortProblem(m.Efforts, m.DefaultEffort))
		}
		if r := toolProfileProblem(m.ToolProfile); r != "" {
			return at(table, "tool_profile", r)
		}
	}
	return nil
}
