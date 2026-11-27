// Package routing resolves review targets from local configuration and injected
// provider facts. Resolution never starts a provider or selects a credential.
package routing

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/charliek/craze/internal/errkind"
)

type Config struct {
	Routing Settings          `toml:"routing"`
	Targets map[string]Target `toml:"targets"`
	Roles   map[string]Role   `toml:"roles"`
}
type Settings struct {
	AllowPaidFallback bool `toml:"allow_paid_fallback"`
}
type Target struct {
	Maker   string  `toml:"maker"`
	Effort  string  `toml:"effort"`
	Cap     string  `toml:"cap"`
	Version string  `toml:"version"`
	Routes  []Route `toml:"routes"`
}
type Route struct {
	Provider        string `toml:"provider"`
	Model           string `toml:"model"`
	Billing         string `toml:"billing"`
	VersionTemplate string `toml:"version_template"`
}
type Role struct {
	Candidates []string `toml:"candidates"`
	Seats      []string `toml:"seats"`
}

const (
	Subscription = "subscription"
	APIKey       = "api_key"
	Metered      = "metered"
)

var (
	versionToken = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	nameToken    = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	efforts      = []string{"minimal", "low", "medium", "high", "xhigh", "max"}
)

type configError string

func (e configError) Error() string           { return string(e) }
func (e configError) ErrorInfo() errkind.Info { return errkind.New(errkind.Config) }

func KnownProvider(id string) bool {
	switch id {
	case "codex", "native", "claude", "antigravity", "cursor", "grok":
		return true
	}
	return false
}

// Load overlays only routing tables. TOML errors can contain the source line
// (including an unrelated credential). Never propagate the decoder's text.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, configError("cannot read routing configuration")
	}
	var user Config
	meta, err := toml.Decode(string(b), &user)
	if err != nil {
		return Config{}, configError("invalid TOML or field type in config.toml")
	}
	// The TOML decoder matches struct fields case-insensitively. In these
	// three tables only the documented spellings may affect policy.
	for _, key := range meta.Keys() {
		root := strings.ToLower(key[0])
		if root != "routing" && root != "targets" && root != "roles" {
			continue
		}
		for i, part := range key {
			if i == 1 && root != "routing" {
				continue
			}
			if part != strings.ToLower(part) {
				return Config{}, configError("unknown field in routing configuration")
			}
		}
	}
	for _, k := range meta.Undecoded() {
		if len(k) > 0 && slices.Contains([]string{"routing", "targets", "roles"}, k[0]) {
			return Config{}, configError("unknown field in routing configuration")
		}
	}
	cfg.Routing = user.Routing
	for name, t := range user.Targets {
		base, ok := cfg.Targets[name]
		if !ok {
			base = Target{Effort: "high", Cap: "20m"}
		}
		defined := func(key string) bool { return meta.IsDefined("targets", name, key) }
		if defined("maker") {
			base.Maker = t.Maker
		}
		if defined("effort") {
			base.Effort = t.Effort
		}
		if defined("cap") {
			base.Cap = t.Cap
		}
		if defined("version") {
			base.Version = t.Version
		}
		if defined("routes") {
			base.Routes = t.Routes
		}
		cfg.Targets[name] = base
	}
	for name, role := range user.Roles {
		if role.Candidates != nil || role.Seats != nil {
			cfg.Roles[name] = role
		} else if _, ok := cfg.Roles[name]; !ok {
			cfg.Roles[name] = role
		}
	}
	return cfg, cfg.validate()
}

func (cfg Config) validate() error {
	for name, t := range cfg.Targets {
		cap, err := time.ParseDuration(t.Cap)
		if !nameToken.MatchString(name) || !nameToken.MatchString(t.Maker) || !slices.Contains(efforts, t.Effort) || err != nil || cap <= 0 || t.Version != "" && !versionToken.MatchString(t.Version) {
			return configError("invalid target name, maker, effort, cap or version")
		}
		for _, r := range t.Routes {
			if !KnownProvider(r.Provider) || strings.TrimSpace(r.Model) == "" || strings.ContainsAny(r.Model, " \r\n\t") {
				return configError("invalid target route provider or model")
			}
			if !slices.Contains([]string{"", Subscription, APIKey, Metered}, r.Billing) {
				return configError("invalid route billing class")
			}
			tpl := strings.NewReplacer("{v}", "", "{v_dash}", "").Replace(r.VersionTemplate)
			if r.VersionTemplate != "" && (tpl == r.VersionTemplate || strings.ContainsAny(tpl, "{} \r\n\t")) {
				return configError("invalid version_template")
			}
		}
	}
	for name, r := range cfg.Roles {
		if !nameToken.MatchString(name) || (r.Candidates == nil) == (r.Seats == nil) {
			return configError("a role requires either candidates or seats")
		}
		for _, targets := range [][]string{r.Candidates, r.Seats} {
			for _, id := range targets {
				if _, ok := cfg.Targets[id]; !ok {
					return configError(fmt.Sprintf("role %s refers to an unknown target", name))
				}
			}
		}
	}
	return nil
}
