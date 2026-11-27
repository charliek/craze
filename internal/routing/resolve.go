package routing

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

type Reason string

const (
	PaidBlocked         Reason = "paid_blocked"
	HeldEnvKey          Reason = "held_env_key"
	OverageHold         Reason = "overage_hold"
	NotSignedIn         Reason = "not_signed_in"
	BinaryMissing       Reason = "binary_missing"
	Unfunded            Reason = "unfunded"
	ModelUnavailable    Reason = "model_unavailable"
	ChatGPTListUncached Reason = "chatgpt_list_uncached"
	NoVersionTemplate   Reason = "no_version_template"
	SameMaker           Reason = "same_maker"
	SelfDelegation      Reason = "self_delegation"
)

// Facts are read offline for the expanded route. Billing, when present, is
// the class of the actual credential source (including native alias overrides).
type Facts struct {
	Billing     string
	Reason      Reason
	Provisional bool
	Note        string
	Efforts     []string
}
type Probe func(provider, model string) Facts

type Caller struct {
	Providers []string `json:"providers"`
	Immediate string   `json:"immediate,omitempty"`
}

// DetectCaller preserves the immediate provider for exact-provider refusal.
// The union independently excludes maker families from semantic role requests.
func DetectCaller(getenv func(string) string, immediate, override string) (Caller, error) {
	c := Caller{Immediate: immediate, Providers: []string{}}
	add := func(p string) {
		if KnownProvider(p) && !slices.Contains(c.Providers, p) {
			c.Providers = append(c.Providers, p)
		}
	}
	if override != "" {
		if !KnownProvider(override) {
			return Caller{}, configError("unknown caller provider")
		}
		c.Providers = []string{override}
		// A caller override disambiguates the caller but cannot mask a known immediate provider.
		if c.Immediate == "" {
			c.Immediate = override
		}
		return c, nil
	}
	add(strings.TrimSpace(getenv("CRAZE_SESSION_PROVIDER")))
	if getenv("CLAUDECODE") == "1" {
		add("claude")
	}
	for _, m := range []struct{ key, provider string }{{"CURSOR_AGENT", "cursor"}, {"ANTIGRAVITY_AGENT", "antigravity"}, {"CODEX_THREAD_ID", "codex"}} {
		if getenv(m.key) != "" {
			add(m.provider)
		}
	}
	return c, nil
}

func callerMaker(p string) string {
	switch p {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "antigravity":
		return "google"
	default:
		return p
	}
}

type Request struct {
	Target, Version, Role, Effort, Provider string
	AllowPaid, AllowSame                    bool
	Caller                                  Caller
}
type Choice struct {
	Target      string  `json:"target"`
	Version     *string `json:"version"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	Billing     string  `json:"billing"`
	Effort      string  `json:"effort"`
	Provisional bool    `json:"provisional"`
	Note        string  `json:"note,omitempty"`
}
type Skip struct {
	Target   string `json:"target"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Reason   Reason `json:"reason"`
}
type Selection struct {
	Target     string   `json:"target"`
	Effort     string   `json:"effort"`
	CapSeconds float64  `json:"capSeconds"`
	Mode       string   `json:"mode,omitempty"`
	Status     string   `json:"status"`
	Routes     []Choice `json:"routes"`
	Skipped    []Skip   `json:"skipped"`
}
type Result struct {
	Schema     int         `json:"schema"`
	Caller     Caller      `json:"caller"`
	Kind       string      `json:"kind"`
	Effort     string      `json:"effort,omitempty"`
	CapSeconds float64     `json:"capSeconds,omitempty"`
	Targets    []Selection `json:"targets,omitzero"`
	Seats      []Selection `json:"seats,omitzero"`
	Routes     []Choice    `json:"routes,omitzero"`
	Skipped    []Skip      `json:"skipped"`
	Warnings   []string    `json:"warnings,omitempty"`
}

// Resolve retains every eligible route in order. A single-target role flattens
// eligible routes across its remaining candidates (taking effort and cap from
// the first resolved candidate), a panel resolves every seat, and an overview
// succeeds if at least one target can run.
func (cfg Config) Resolve(req Request, probe Probe) (Result, bool, error) {
	res := Result{Schema: 1, Caller: req.Caller, Skipped: []Skip{}}
	if req.Target != "" && req.Role != "" || req.Target == "" && req.Version != "" {
		return res, false, configError("--version requires --target; --role and --target are exclusive")
	}
	if req.Effort != "" && !slices.Contains(efforts, req.Effort) {
		return res, false, configError("unknown effort")
	}
	var names []string
	switch {
	case req.Target != "":
		id, v, pin := strings.Cut(req.Target, "@")
		if pin {
			if req.Version != "" || v == "" {
				return res, false, configError("supply a version once")
			}
			req.Version = v
		}
		if req.Version != "" && !versionToken.MatchString(req.Version) {
			return res, false, configError("invalid version")
		}
		if _, ok := cfg.Targets[id]; !ok {
			return res, false, configError("unknown target")
		}
		res.Kind, names = "target", []string{id}
	case req.Role != "":
		role, ok := cfg.Roles[req.Role]
		if !ok {
			return res, false, configError("unknown role")
		}
		res.Kind, names = "role", role.Candidates
		if role.Seats != nil {
			res.Kind, names = "panel", role.Seats
		}
	default:
		res.Kind, names = "overview", slices.Sorted(maps.Keys(cfg.Targets))
	}
	// Preserve the empty array appropriate to this kind; irrelevant
	// collections remain nil and are omitted by the encoder.
	switch res.Kind {
	case "role", "target":
		res.Routes = []Choice{}
	case "panel":
		res.Seats = []Selection{}
	default:
		res.Targets = []Selection{}
	}
	ready := 0
	for _, id := range names {
		t := cfg.Targets[id]
		sameMaker := slices.ContainsFunc(req.Caller.Providers, func(p string) bool { return callerMaker(p) == t.Maker })
		if res.Kind == "role" && sameMaker {
			res.Skipped = append(res.Skipped, Skip{Target: id, Reason: SameMaker})
			continue
		}
		d, _ := time.ParseDuration(t.Cap)
		seat := Selection{Target: id, CapSeconds: d.Seconds(), Effort: cmp.Or(req.Effort, t.Effort), Status: "unresolvable", Routes: []Choice{}, Skipped: []Skip{}}
		subagent := res.Kind == "panel" && slices.ContainsFunc(req.Caller.Providers, func(p string) bool {
			return (p == "claude" || p == "antigravity" || p == "codex") && callerMaker(p) == t.Maker
		})
		if subagent {
			seat.Mode, seat.Status = "native_subagent", "resolved"
		} else {
			if res.Kind == "panel" {
				seat.Mode = "craze_review"
			}
			for _, r := range t.Routes {
				if req.Provider != "" && r.Provider != req.Provider {
					continue
				}
				c, reason := choose(id, t, r, req, cfg.Routing.AllowPaidFallback, probe)
				if reason != "" {
					seat.Skipped = append(seat.Skipped, Skip{id, c.Provider, c.Model, reason})
					continue
				}
				seat.Routes = append(seat.Routes, c)
				seat.Status = "resolved"
				if c.Effort != seat.Effort {
					msg := fmt.Sprintf("%s/%s: effort %s adjusted to %s", c.Provider, c.Model, seat.Effort, c.Effort)
					if !slices.Contains(res.Warnings, msg) {
						res.Warnings = append(res.Warnings, msg)
					}
				}
			}
		}
		if seat.Status == "resolved" {
			ready++
		}
		switch res.Kind {
		case "panel":
			res.Seats = append(res.Seats, seat)
		case "overview":
			res.Targets = append(res.Targets, seat)
		default:
			res.Skipped = append(res.Skipped, seat.Skipped...)
			if len(seat.Routes) > 0 {
				if ready == 1 {
					res.Effort, res.CapSeconds = seat.Effort, seat.CapSeconds
				}
				res.Routes = append(res.Routes, seat.Routes...)
			} else if ready == 0 {
				res.Effort, res.CapSeconds = seat.Effort, seat.CapSeconds
			}
		}
	}
	return res, ready > 0 && (res.Kind != "panel" || ready == len(names)), nil
}

func choose(id string, t Target, r Route, req Request, configPaid bool, probe Probe) (Choice, Reason) {
	c := Choice{Target: id, Provider: r.Provider, Model: r.Model, Billing: cmp.Or(r.Billing, Metered), Effort: cmp.Or(req.Effort, t.Effort)}
	movingAlias := r.Provider == "claude" && (r.Model == "opus" || r.Model == "fable")
	switch {
	case req.Version != "":
		if r.VersionTemplate == "" {
			return c, NoVersionTemplate
		}
		c.Model = strings.NewReplacer("{v}", req.Version, "{v_dash}", strings.ReplaceAll(req.Version, ".", "-")).Replace(r.VersionTemplate)
		c.Version = &req.Version
	case t.Version != "" && r.VersionTemplate != "" && !movingAlias:
		c.Version = &t.Version
	}
	if !req.AllowSame && req.Caller.Immediate == r.Provider {
		return c, SelfDelegation
	}
	f := probe(c.Provider, c.Model)
	// A route declaration can tighten the policy, never relabel a paid
	// credential as a subscription.
	if f.Billing != "" && (c.Billing == Subscription || f.Billing != Subscription) {
		c.Billing = f.Billing
	}
	if f.Reason == HeldEnvKey {
		return c, f.Reason
	}
	if c.Billing != Subscription && !req.AllowPaid && !configPaid {
		return c, PaidBlocked
	}
	if f.Reason != "" {
		return c, f.Reason
	}
	c.Provisional, c.Note = f.Provisional, f.Note
	c.Effort = ClampEffort(c.Effort, f.Efforts)
	return c, ""
}

// ClampEffort chooses the closest supported effort at or below the request.
// A nil catalog leaves a preference provisional until the provider starts.
func ClampEffort(want string, levels []string) string {
	if want == "" || levels == nil || slices.Contains(levels, want) {
		return want
	}
	for i := slices.Index(efforts, want); i >= 0; i-- {
		if slices.Contains(levels, efforts[i]) {
			return efforts[i]
		}
	}
	if len(levels) > 0 {
		return levels[0]
	}
	return ""
}
