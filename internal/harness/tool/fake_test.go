package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
)

// keyA is the only credential any test here holds. It is obviously not a
// secret and clears modeltable's 8-byte floor.
const keyA = "sk-canary-alpha-0001"

// fakeInput is every fake tool's argument object.
type fakeInput struct {
	Text string `json:"text"`
	Path string `json:"path"`
}

// fake is a tool whose Run a test supplies. Its Prepare parses fakeInput and
// refuses an empty text, the way a real tool validates before anything runs;
// req, when set, replaces the Request it builds.
type fake struct {
	spec Spec
	run  func(ctx context.Context, env Env, in fakeInput) Result
	req  func(in fakeInput, env Env) Request
	// prepPanic and reqPanic make Prepare or Request panic.
	prepPanic, reqPanic bool

	runs     atomic.Int32
	lastText atomic.Value // the text the last Run saw, unredacted
}

func fakeSpec(id string, trunc Direction) Spec {
	return Spec{
		ID:          id,
		Description: "Echoes text; a test double.",
		Parameters: map[string]any{
			"text": map[string]any{"type": "string", "description": "What to return"},
			"path": map[string]any{"type": "string", "description": "A path the call names"},
		},
		Required: []string{"text"},
		Kind:     KindRead,
		ReadOnly: true,
		Parallel: true,
		Truncate: trunc,
	}
}

func newFake(id string, run func(context.Context, Env, fakeInput) Result) *fake {
	return &fake{spec: fakeSpec(id, Head), run: run}
}

func (f *fake) Spec() Spec { return f.spec }

func (f *fake) Prepare(env Env, c Call) (Prepared, error) {
	if f.prepPanic {
		panic("prepare exploded")
	}
	var in fakeInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return nil, fmt.Errorf("input is not an object with string fields: %v", err)
	}
	if in.Text == "" {
		return nil, errors.New("text must not be empty")
	}
	return &fakePrepared{f: f, in: in, env: env}, nil
}

type fakePrepared struct {
	f   *fake
	in  fakeInput
	env Env
}

func (p *fakePrepared) Request() Request {
	if p.f.reqPanic {
		panic("request exploded")
	}
	if p.f.req != nil {
		return p.f.req(p.in, p.env)
	}
	r := Request{Title: p.in.Text}
	if p.in.Path != "" {
		r.Paths = []string{p.env.Resolve(p.in.Path)}
	}
	return r
}

func (p *fakePrepared) Run(ctx context.Context, env Env) Result {
	p.f.runs.Add(1)
	p.f.lastText.Store(p.in.Text)
	if p.f.run != nil {
		return p.f.run(ctx, env, p.in)
	}
	return Result{Text: p.in.Text}
}

// testEnv is an Env on two fresh directories, redacting keys.
func testEnv(t *testing.T, keys ...string) Env {
	t.Helper()
	return Env{Workspace: t.TempDir(), Home: t.TempDir(), Redactor: redact.New(keys...)}
}

func newDispatcher(t *testing.T, env Env, gate Gate, tools ...Tool) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(Options{Tools: tools, Gate: gate, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// input marshals v as a call's raw arguments.
func input(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
