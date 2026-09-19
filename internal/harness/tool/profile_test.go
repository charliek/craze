package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func system(e SystemEnv) string { return "You work in " + e.Workspace + " on " + e.OS + "." }

// panicSpec is a tool whose Spec panics.
type panicSpec struct{}

func (panicSpec) Spec() Spec                          { panic("spec exploded") }
func (panicSpec) Prepare(Env, Call) (Prepared, error) { return nil, errors.New("unreachable") }

// TestNilAndPanickingToolsAreErrors: a nil pointer inside a Tool interface
// is not == nil, and calling its Spec would panic; neither it nor a Spec that
// panics may take down Register, NewDispatcher or ToolsJSON. The same tool
// set without the bad tool, the negative control, goes through all three.
func TestNilAndPanickingToolsAreErrors(t *testing.T) {
	var typedNil *fake
	for name, bad := range map[string]Tool{"a typed nil": typedNil, "a panicking Spec": panicSpec{}} {
		t.Run(name, func(t *testing.T) {
			want := "is a nil *tool.fake"
			if name == "a panicking Spec" {
				want = "Spec panicked: spec exploded"
			}
			tools := []Tool{newFake("echo", nil), bad}
			var r Registry
			if err := r.Register(Profile{Name: "p", Tools: tools, System: system}); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Register = %v, want an error containing %q", err, want)
			}
			if _, err := NewDispatcher(Options{Tools: tools, Env: testEnv(t)}); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("NewDispatcher = %v, want an error containing %q", err, want)
			}
			if _, err := (Profile{Name: "p", Tools: tools, System: system}).ToolsJSON(); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("ToolsJSON = %v, want an error containing %q", err, want)
			}

			good := tools[:1]
			if err := r.Register(Profile{Name: "p", Tools: good, System: system}); err != nil {
				t.Fatal(err)
			}
			if _, err := NewDispatcher(Options{Tools: good, Env: testEnv(t)}); err != nil {
				t.Fatal(err)
			}
			if _, err := (Profile{Name: "p", Tools: good, System: system}).ToolsJSON(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRegisterRefusesBrokenProfiles(t *testing.T) {
	good := func() *fake { return newFake("echo", nil) }
	with := func(mut func(*Spec)) Tool {
		f := good()
		mut(&f.spec)
		return f
	}
	cases := map[string]struct {
		p    Profile
		want string
	}{
		"no name":              {Profile{Name: "", Tools: []Tool{good()}, System: system}, "profile name"},
		"a name with a slash":  {Profile{Name: "open/code", Tools: []Tool{good()}, System: system}, "profile name"},
		"no System":            {Profile{Name: "p", Tools: []Tool{good()}}, "no System"},
		"a nil tool":           {Profile{Name: "p", Tools: []Tool{good(), nil}, System: system}, "is nil"},
		"a tool listed twice":  {Profile{Name: "p", Tools: []Tool{good(), good()}, System: system}, "listed twice"},
		"nil Required":         {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.Required = nil })}, System: system}, "required is nil"},
		"undeclared required":  {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.Required = []string{"nope"} })}, System: system}, `"nope" is not in Parameters`},
		"required twice":       {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.Required = []string{"text", "text"} })}, System: system}, "listed twice"},
		"an unknown kind":      {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.Kind = "write" })}, System: system}, "unknown kind"},
		"an unknown direction": {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.Truncate = 7 })}, System: system}, "direction"},
		"a bad function name":  {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.ID = "read file" })}, System: system}, "function-name"},
		"no description":       {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.Description = " \n" })}, System: system}, "no description"},
		"unmarshalable params": {Profile{Name: "p", Tools: []Tool{with(func(s *Spec) { s.Parameters["text"] = func() {} })}, System: system}, "do not marshal"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var r Registry
			err := r.Register(tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Register = %v, want an error containing %q", err, tc.want)
			}
			if _, err := r.ProfileFor(ModelRef{}); err == nil {
				t.Fatal("a refused profile became the default")
			}
		})
	}

	// The negative control, and a name taken twice.
	var r Registry
	if err := r.Register(Profile{Name: "opencode", Tools: []Tool{good()}, System: system}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Profile{Name: "opencode", Tools: []Tool{good()}, System: system}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("a second opencode = %v", err)
	}
	// No required parameters at all is fine, spelled as an empty slice.
	if err := r.Register(Profile{Name: "bare", Tools: []Tool{with(func(s *Spec) { s.Required = []string{} })}, System: system}); err != nil {
		t.Fatal(err)
	}
}

// TestSecondProfileSelectedByOverride is Seam 2 end to end: a second
// profile with another tool set is chosen by a model's tool_profile, the
// default by any other model, and a dispatcher built on either knows only
// its own tools.
func TestSecondProfileSelectedByOverride(t *testing.T) {
	var r Registry
	if err := r.Register(Profile{Name: "opencode", Tools: []Tool{newFake("read", nil), newFake("bash", nil)}, System: system}); err != nil {
		t.Fatal(err)
	}
	patch := newFake("apply_patch", nil)
	if err := r.Register(Profile{Name: "gpt", Tools: []Tool{patch}, System: func(SystemEnv) string { return "patch prompt" }}); err != nil {
		t.Fatal(err)
	}

	def, err := r.ProfileFor(ModelRef{Provider: "fireworks", Alias: "kimi", WireModel: "kimi-k3"})
	if err != nil || def.Name != "opencode" || len(def.Tools) != 2 {
		t.Fatalf("default = %+v, %v", def, err)
	}
	gpt, err := r.ProfileFor(ModelRef{Alias: "gpt-5", Profile: "gpt"})
	if err != nil || gpt.Name != "gpt" || len(gpt.Tools) != 1 || gpt.System(SystemEnv{}) != "patch prompt" {
		t.Fatalf("override = %+v, %v", gpt, err)
	}
	if _, err := r.ProfileFor(ModelRef{Alias: "x", Profile: "claude"}); !errors.Is(err, ErrUnknownProfile) || !strings.Contains(err.Error(), `"claude"`) {
		t.Fatalf("unknown override = %v, want ErrUnknownProfile naming it", err)
	}
	// A caller cannot change the registry's tool list through a returned
	// profile.
	gpt.Tools[0] = nil
	if gpt, _ = r.ProfileFor(ModelRef{Profile: "gpt"}); gpt.Tools[0] == nil {
		t.Fatal("ProfileFor returned the registry's own slice")
	}

	env := testEnv(t)
	d := newDispatcher(t, env, nil, gpt.Tools...)
	if _, _, ok := d.Prepare(Call{ID: "t1.1.1", Tool: "apply_patch", Input: input(t, fakeInput{Text: "p"})}); !ok {
		t.Fatal("the gpt profile's own tool did not prepare")
	}
	if res := d.Run(context.Background(), "t1.1.1", nil); res.IsError || patch.runs.Load() != 1 {
		t.Fatalf("Run = %+v", res)
	}
	_, res, ok := d.Prepare(Call{ID: "t1.1.2", Tool: "bash", Input: input(t, fakeInput{Text: "ls"})})
	if ok || res.Text != "tool not found: bash. Available tools: apply_patch" {
		t.Fatalf("the other profile's tool = ok %v, %+v", ok, res)
	}

	var empty Registry
	if _, err := empty.ProfileFor(ModelRef{}); err == nil {
		t.Fatal("an empty registry chose a profile")
	}
}

func TestToolsJSONIsDeterministic(t *testing.T) {
	noParams := newFake("ping", nil)
	noParams.spec.Parameters, noParams.spec.Required = nil, []string{}
	noParams.spec.Description = "Checks <path> & more."
	p := Profile{Name: "p", Tools: []Tool{newFake("echo", nil), noParams}, System: system}

	a, err := p.ToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"echo","description":"Echoes text; a test double.","parameters":{"type":"object",` +
		`"properties":{"path":{"description":"A path the call names","type":"string"},"text":{"description":"What to return","type":"string"}},` +
		`"required":["text"]}},` +
		`{"name":"ping","description":"Checks <path> & more.","parameters":{"type":"object","properties":{},"required":[]}}]`
	if string(a) != want {
		t.Fatalf("ToolsJSON =\n%s\nwant\n%s", a, want)
	}
	// The negative control for "never null": a nil slice would say so.
	if nilJSON, _ := json.Marshal(struct{ R []string }{}); string(nilJSON) != `{"R":null}` {
		t.Fatalf("a nil slice marshals as %s; the check above proves nothing", nilJSON)
	}

	// The same tools, their parameter maps built in another order, give the
	// same bytes; so does a second call.
	rebuilt := newFake("echo", nil)
	rebuilt.spec.Parameters = map[string]any{}
	rebuilt.spec.Parameters["text"] = map[string]any{"type": "string", "description": "What to return"}
	rebuilt.spec.Parameters["path"] = map[string]any{"description": "A path the call names", "type": "string"}
	b, err := Profile{Name: "p", Tools: []Tool{rebuilt, noParams}, System: system}.ToolsJSON()
	if err != nil || string(b) != string(a) {
		t.Fatalf("a rebuilt profile serialized differently:\n%s\n%s", a, b)
	}

	sum, err := p.ToolsSHA256()
	h := sha256.Sum256(a)
	if err != nil || sum != hex.EncodeToString(h[:]) {
		t.Fatalf("ToolsSHA256 = %s, %v", sum, err)
	}
	// Any change to what the model reads changes the hash.
	noParams.spec.Description = "Checks <path>."
	if other, _ := p.ToolsSHA256(); other == sum {
		t.Fatal("a changed description kept the hash")
	}
}
