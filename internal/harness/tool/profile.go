package tool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Profile is one tool contract: a tool set and the system prompt written for
// it (Seam 2, plan 019 §3.1). H2 registers one, opencode's. Models differ
// most in how they use tools — opencode varies its prompt by model family and
// swaps edit and write for apply_patch on GPT-5 — so a second profile is a
// registration, not a change to the runner.
//
// A profile is session-scoped: the session chooses it once, at Open, from
// its starting model, and records its name and ToolsSHA256 in the
// transcript's header. The system prompt is frozen per session (D-30), so a
// switch to a model with another profile is refused, not honoured midway.
type Profile struct {
	Name string
	// Tools are offered to the model in this order, which is part of every
	// request's cache prefix.
	Tools []Tool
	// System builds the session's system prompt. It is called once per
	// session and must be deterministic.
	System func(SystemEnv) string
}

// SystemEnv is what a profile's system prompt may depend on. It holds
// nothing that changes between requests — no clock, no git state — so every
// request in a session starts with the same bytes (plan 018 §3.7).
type SystemEnv struct {
	Workspace string // the session's working directory, absolute and cleaned
	OS        string // runtime.GOOS
}

// ModelRef is how a profile is chosen: the resolved model, and the optional
// tool_profile override models.toml gives it. This package does not import
// modeltable; the harness fills a ModelRef from a resolved model.
type ModelRef struct {
	Provider  string // the provider id
	Alias     string
	WireModel string
	// Profile is the model's tool_profile; "" takes the registry's default.
	Profile string
}

// ErrUnknownProfile is ProfileFor's error for an override that names no
// registered profile.
var ErrUnknownProfile = errors.New("tool: unknown tool profile")

// Registry holds the profiles a build knows. The first one registered is the
// default. It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	profiles map[string]Profile
	def      string
}

// Register adds p. It refuses a profile with no name or a name taken, no
// System func, a nil tool, or a tool whose Spec breaks a rule (see
// validateTools) — each a bug in the build, caught before any session
// offers the profile to a model.
func (r *Registry) Register(p Profile) error {
	if !validID(p.Name) {
		return fmt.Errorf("tool: profile name %q: want 1-64 letters, digits, dots, dashes, underscores", p.Name)
	}
	if p.System == nil {
		return fmt.Errorf("tool: profile %q has no System func", p.Name)
	}
	if _, err := validateTools(p.Tools); err != nil {
		return fmt.Errorf("tool: profile %q: %w", p.Name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.profiles[p.Name]; dup {
		return fmt.Errorf("tool: profile %q is already registered", p.Name)
	}
	if r.profiles == nil {
		r.profiles = make(map[string]Profile)
		r.def = p.Name
	}
	p.Tools = slices.Clone(p.Tools)
	r.profiles[p.Name] = p
	return nil
}

// ProfileFor returns the profile for ref: the one its override names, else
// the default. It is the single place a profile is chosen.
func (r *Registry) ProfileFor(ref ModelRef) (Profile, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name := ref.Profile
	if name == "" {
		name = r.def
	}
	p, ok := r.profiles[name]
	switch {
	case ok:
		p.Tools = slices.Clone(p.Tools)
		return p, nil
	case len(r.profiles) == 0:
		return Profile{}, errors.New("tool: no tool profile is registered")
	default:
		return Profile{}, fmt.Errorf("%w %q (model %q)", ErrUnknownProfile, name, ref.Alias)
	}
}

// validateTools checks a tool set and returns its specs, in the tools'
// order. Every tool must be non-nil with a unique id that a provider accepts
// as a function name, a description, a known Kind and Direction, a non-nil
// Required naming only declared parameters, each once, and parameters that
// marshal to JSON.
func validateTools(tools []Tool) ([]Spec, error) {
	specs := make([]Spec, 0, len(tools))
	for i, t := range tools {
		s, err := specOf(t)
		if err != nil {
			return nil, fmt.Errorf("tool %d %w", i, err)
		}
		if err := validateSpec(s); err != nil {
			return nil, fmt.Errorf("tool %q: %w", s.ID, err)
		}
		if slices.ContainsFunc(specs, func(o Spec) bool { return o.ID == s.ID }) {
			return nil, fmt.Errorf("tool %q is listed twice", s.ID)
		}
		specs = append(specs, s)
	}
	return specs, nil
}

// specOf returns t's Spec, or an error when t is nil — an untyped nil, or a
// nil pointer (or map, func, ...) inside the interface, which t == nil does
// not see — or when Spec panics.
func specOf(t Tool) (s Spec, err error) {
	if t == nil {
		return Spec{}, errors.New("is nil")
	}
	switch v := reflect.ValueOf(t); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice, reflect.Interface:
		if v.IsNil() {
			return Spec{}, fmt.Errorf("is a nil %T", t)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			s, err = Spec{}, fmt.Errorf("(%T): Spec panicked: %v", t, r)
		}
	}()
	return t.Spec(), nil
}

func validateSpec(s Spec) error {
	if !functionName(s.ID) {
		return errors.New("id: want 1-64 letters, digits, dashes, underscores (a provider's function-name rule)")
	}
	if strings.TrimSpace(s.Description) == "" {
		return errors.New("no description")
	}
	if !s.Kind.valid() {
		return fmt.Errorf("unknown kind %q", s.Kind)
	}
	if !s.Truncate.valid() {
		return fmt.Errorf("unknown truncation direction %d", s.Truncate)
	}
	// A nil Required marshals as null, which strict providers reject; the
	// rule is enforced here rather than patched over, so every Spec says
	// what it sends.
	if s.Required == nil {
		return errors.New("required is nil: use []string{} for no required parameters")
	}
	for i, name := range s.Required {
		if _, ok := s.Parameters[name]; !ok {
			return fmt.Errorf("required parameter %q is not in Parameters", name)
		}
		if slices.Contains(s.Required[:i], name) {
			return fmt.Errorf("required parameter %q is listed twice", name)
		}
	}
	if _, err := json.Marshal(s.Parameters); err != nil {
		return fmt.Errorf("parameters do not marshal: %w", err)
	}
	return nil
}

// functionName reports whether s is a valid function name for OpenAI's
// Chat Completions API, the strictest rule among the providers craze drives:
// ^[a-zA-Z0-9_-]{1,64}$.
func functionName(s string) bool {
	return len(s) <= 64 && allBytes(s, func(c byte) bool { return wordByte(c) || c == '-' })
}

// wireTool is a tool as a model sees it: the fields toolbridge.go gives
// Fantasy, in the shape Fantasy sends them (agent.go:1131-1136).
type wireTool struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parameters  wireSchema `json:"parameters"`
}

type wireSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required"`
}

// ToolsJSON serializes the profile's tools as the model sees them — name,
// description, and the JSON Schema object {type, properties, required} — in
// the profile's order (SpecsJSON). ToolsSHA256 hashes them.
//
// A caller that changes a spec before it offers it — a session redacts the
// machine's own facts out of a description — hashes what it offers with
// SpecsJSON instead, so the hash and the wire cannot disagree.
func (p Profile) ToolsJSON() ([]byte, error) {
	specs := make([]Spec, 0, len(p.Tools))
	for i, t := range p.Tools {
		s, err := specOf(t)
		if err != nil {
			return nil, fmt.Errorf("tool: profile %q: tool %d %w", p.Name, i, err)
		}
		specs = append(specs, s)
	}
	b, err := SpecsJSON(specs)
	if err != nil {
		return nil, fmt.Errorf("tool: profile %q: %w", p.Name, err)
	}
	return b, nil
}

// SpecsJSON serializes specs as the model sees them — name, description, and
// the JSON Schema object {type, properties, required} — in their order. The
// bytes are deterministic: struct fields marshal in order, map keys sorted,
// HTML left unescaped, no trailing newline; required is [] and properties {}
// when empty, never null.
func SpecsJSON(specs []Spec) ([]byte, error) {
	tools := make([]wireTool, 0, len(specs))
	for _, s := range specs {
		props := s.Parameters
		if props == nil {
			props = map[string]any{}
		}
		req := s.Required
		if req == nil {
			req = []string{}
		}
		tools = append(tools, wireTool{
			Name:        s.ID,
			Description: s.Description,
			Parameters:  wireSchema{Type: "object", Properties: props, Required: req},
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tools); err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ToolsSHA256 is the hex SHA-256 of ToolsJSON: the fingerprint a transcript's
// header records, so a later reader can tell which tool contract the
// conversation was held under (plan 019 §3.1).
func (p Profile) ToolsSHA256() (string, error) {
	b, err := p.ToolsJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
