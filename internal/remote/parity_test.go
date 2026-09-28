package remote_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/tui"
)

// Every command over the socket answers what the engine answers in process
// (plan 027 §3.14, A16): the same script, run against a Session attached to a
// host and against a twin host's engine called directly, gives the same
// results and the same errors — the text verbatim, the code and reason, and
// errors.Is over every sentinel a caller matches.

// commander is what a script drives: the Backend's commands and reads, and
// the client they go under.
type commander interface {
	ClientID() string
	Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error)
	Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error
	Unqueue(ctx context.Context, c engine.Command, id string) (agent.QueuedPrompt, error)
	EditQueued(ctx context.Context, c engine.Command, id, text string, expectedVersion *int) error
	ClearQueue(ctx context.Context, c engine.Command) ([]agent.QueuedPrompt, error)
	Disarm(ctx context.Context, c engine.Command) error
	Interject(ctx context.Context, c engine.Command, text string) error
	SetTitle(ctx context.Context, c engine.Command, title string) error
	Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error)
	Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error)
	CancelSubagent(ctx context.Context, c engine.Command, id string) error
	Ask(ctx context.Context, id string) (agent.AskRecord, bool, error)
	Settings(ctx context.Context) (backend.Settings, error)
}

// inProcess is the engine called directly: the calls the in-process backend
// forwards (tui's engineBackend), with nothing between.
type inProcess struct {
	e      *engine.Engine
	client string
}

func (p *inProcess) ClientID() string { return p.client }
func (p *inProcess) Submit(_ context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	return p.e.Submit(c, text, mode, fromRow)
}
func (p *inProcess) Answer(_ context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	return p.e.Answer(c, id, a)
}
func (p *inProcess) Unqueue(_ context.Context, c engine.Command, id string) (agent.QueuedPrompt, error) {
	return p.e.Unqueue(c, id)
}
func (p *inProcess) EditQueued(_ context.Context, c engine.Command, id, text string, v *int) error {
	return p.e.EditQueued(c, id, text, v)
}
func (p *inProcess) ClearQueue(_ context.Context, c engine.Command) ([]agent.QueuedPrompt, error) {
	return p.e.ClearQueue(c)
}
func (p *inProcess) Disarm(_ context.Context, c engine.Command) error { return p.e.Disarm(c) }
func (p *inProcess) Interject(ctx context.Context, c engine.Command, text string) error {
	return p.e.Interject(ctx, c, text)
}
func (p *inProcess) SetTitle(_ context.Context, c engine.Command, title string) error {
	return p.e.SetTitle(c, title)
}
func (p *inProcess) Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error) {
	return p.e.Set(ctx, c, s)
}
func (p *inProcess) Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error) {
	return p.e.Cancel(ctx, c, turn)
}
func (p *inProcess) CancelSubagent(_ context.Context, c engine.Command, id string) error {
	return p.e.CancelSubagent(c, id)
}
func (p *inProcess) Ask(_ context.Context, id string) (agent.AskRecord, bool, error) {
	rec, ok := p.e.Ask(id)
	return rec, ok, nil
}
func (p *inProcess) Settings(context.Context) (backend.Settings, error) {
	snap := p.e.State().Snapshot
	return backend.Settings{Model: snap.CurrentModel, Mode: snap.CurrentMode, Config: snap.Config}, nil
}

// side is one of a script's two runs: what it drives, and the Stub behind it.
type side struct {
	t    *testing.T
	cmd  commander
	stub *tui.Stub
	n    int
}

// next is the script's next command, under the side's client: both sides
// count alike.
func (sd *side) next() engine.Command {
	sd.n++
	return engine.Command{Client: sd.cmd.ClientID(), ID: strconv.Itoa(sd.n)}
}

func (sd *side) ctx() context.Context { return tctx(sd.t) }

// turn opens a turn that stays open, and waits for it.
func (sd *side) turn(text string) engine.SubmitResult {
	sd.t.Helper()
	open := sd.stub.HangNext()
	res, err := sd.cmd.Submit(sd.ctx(), sd.next(), text, engine.SubmitQueue, "")
	if err != nil {
		sd.t.Fatalf("the turn: %v", err)
	}
	await(sd.t, open, "the turn to open")
	return res
}

// outcome is one call's answer: its result and its error.
type outcome struct {
	what string
	res  any
	err  error
}

// parityScript is one script: host options, a Stub setup, and the calls.
type parityScript struct {
	name  string
	opts  []hostOpt
	setup func(*tui.Stub)
	run   func(sd *side) []outcome
}

func parityScripts() []parityScript {
	bad := failIndex{errors.New("craze: not saving the session: permission denied")}
	grok := func(s *tui.Stub) { s.SetProvider(agent.GrokProvider()) }
	permission := agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
		Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}}
	return []parityScript{
		{name: "a submit starts a turn", run: func(sd *side) []outcome {
			res, err := sd.cmd.Submit(sd.ctx(), sd.next(), "one", engine.SubmitQueue, "")
			return []outcome{{"submit", res, err}}
		}},
		{name: "a submit queues behind a turn, and a queued row cannot start", run: func(sd *side) []outcome {
			sd.turn("first")
			res, err := sd.cmd.Submit(sd.ctx(), sd.next(), "second", engine.SubmitQueue, "")
			out := []outcome{{"queue", res, err}}
			if res.Queued != nil {
				again, err := sd.cmd.Submit(sd.ctx(), sd.next(), "", engine.SubmitQueue, res.Queued.ID)
				out = append(out, outcome{"from its row", again, err})
			}
			return out
		}},
		// A send-now arms, and its own cancel ends the Stub's turn at once, so
		// the arm fires a round trip later: a second arm, or a disarm, races
		// the fire over the wire and does not in process. Those refusals'
		// reconstruction is TestEveryReasonReconstructsItsSentinel's
		// (already_submitted, not_accepting); here, the arm itself and a
		// disarm with nothing armed.
		{name: "a send-now arms", run: func(sd *side) []outcome {
			sd.turn("first")
			armed, err := sd.cmd.Submit(sd.ctx(), sd.next(), "now", engine.SubmitSendNow, "")
			return []outcome{{"arm", armed, err}}
		}},
		{name: "a disarm with nothing armed", run: func(sd *side) []outcome {
			return []outcome{{"disarm", nil, sd.cmd.Disarm(sd.ctx(), sd.next())}}
		}},
		{name: "an interjection with no turn", setup: grok, run: func(sd *side) []outcome {
			return []outcome{{"interject", nil, sd.cmd.Interject(sd.ctx(), sd.next(), "more")}}
		}},
		{name: "an interjection into a turn", setup: grok, run: func(sd *side) []outcome {
			sd.turn("first")
			return []outcome{{"interject", nil, sd.cmd.Interject(sd.ctx(), sd.next(), "more")}}
		}},
		{name: "an interjection the provider cannot make", run: func(sd *side) []outcome {
			sd.turn("first")
			return []outcome{{"interject", nil, sd.cmd.Interject(sd.ctx(), sd.next(), "more")}}
		}},
		{name: "the queue verbs", run: func(sd *side) []outcome {
			sd.turn("first")
			a, _ := sd.cmd.Submit(sd.ctx(), sd.next(), "a", engine.SubmitQueue, "")
			b, _ := sd.cmd.Submit(sd.ctx(), sd.next(), "b", engine.SubmitQueue, "")
			var out []outcome
			if a.Queued == nil || b.Queued == nil {
				return []outcome{{"premise", [2]engine.SubmitResult{a, b}, nil}}
			}
			v := a.Queued.Version
			out = append(out, outcome{"edit", nil, sd.cmd.EditQueued(sd.ctx(), sd.next(), a.Queued.ID, "a, edited", &v)})
			out = append(out, outcome{"edit a stale version", nil, sd.cmd.EditQueued(sd.ctx(), sd.next(), a.Queued.ID, "a, again", &v)})
			out = append(out, outcome{"edit with no version", nil, sd.cmd.EditQueued(sd.ctx(), sd.next(), a.Queued.ID, "a, at last", nil)})
			row, err := sd.cmd.Unqueue(sd.ctx(), sd.next(), b.Queued.ID)
			out = append(out, outcome{"unqueue", row, err})
			row, err = sd.cmd.Unqueue(sd.ctx(), sd.next(), b.Queued.ID)
			out = append(out, outcome{"unqueue it again", row, err})
			_, _ = sd.cmd.Submit(sd.ctx(), sd.next(), "c", engine.SubmitQueue, "")
			rows, err := sd.cmd.ClearQueue(sd.ctx(), sd.next())
			out = append(out, outcome{"clear", rows, err})
			rows, err = sd.cmd.ClearQueue(sd.ctx(), sd.next())
			out = append(out, outcome{"clear it empty", rows, err})
			return out
		}},
		{name: "a rename", run: func(sd *side) []outcome {
			return []outcome{{"rename", nil, sd.cmd.SetTitle(sd.ctx(), sd.next(), "a better name")}}
		}},
		{name: "a rename whose index write fails", opts: []hostOpt{withIndex(engine.IndexOptions{Store: bad, CWD: "/w", Provider: "cursor"})},
			run: func(sd *side) []outcome {
				return []outcome{{"rename", nil, sd.cmd.SetTitle(sd.ctx(), sd.next(), "a better name")}}
			}},
		{name: "the settings", run: func(sd *side) []outcome {
			var out []outcome
			for _, s := range []engine.Setting{
				{Kind: engine.SettingModel, Value: "fast"},
				{Kind: engine.SettingMode, Value: "plan"},
				{Kind: engine.SettingConfig, ID: "effort", Value: "high"},
				{Kind: engine.SettingConfig, ID: "effort", Value: "low", ForModel: "grok"},
				{Kind: engine.SettingConfig, ID: "nope", Value: "x"},
			} {
				res, err := sd.cmd.Set(sd.ctx(), sd.next(), s)
				out = append(out, outcome{"set " + string(s.Kind) + " " + s.ID + "=" + s.Value, res, err})
			}
			set, err := sd.cmd.Settings(sd.ctx())
			return append(out, outcome{"the settings read", set, err})
		}},
		{name: "a setting the agent refuses", setup: func(s *tui.Stub) { s.FailNextSetMode() }, run: func(sd *side) []outcome {
			res, err := sd.cmd.Set(sd.ctx(), sd.next(), engine.Setting{Kind: engine.SettingMode, Value: "plan"})
			return []outcome{{"set", res, err}}
		}},
		{name: "a setting whose option goes", setup: func(s *tui.Stub) { s.DropOptionOnSet("effort") }, run: func(sd *side) []outcome {
			res, err := sd.cmd.Set(sd.ctx(), sd.next(), engine.Setting{Kind: engine.SettingConfig, ID: "effort", Value: "low"})
			return []outcome{{"set", res, err}}
		}},
		{name: "the cancels", run: func(sd *side) []outcome {
			res, err := sd.cmd.Cancel(sd.ctx(), sd.next(), "")
			out := []outcome{{"cancel with nothing running", res, err}}
			turn := sd.turn("first").Turn
			res, err = sd.cmd.Cancel(sd.ctx(), sd.next(), "turn-99")
			out = append(out, outcome{"cancel a stale turn", res, err})
			res, err = sd.cmd.Cancel(sd.ctx(), sd.next(), turn)
			out = append(out, outcome{"cancel the turn", res, err})
			return append(out, outcome{"stop a sub-agent", nil, sd.cmd.CancelSubagent(sd.ctx(), sd.next(), "a-1")})
		}},
		{name: "the asks", run: func(sd *side) []outcome {
			sd.stub.Emit(permission)
			rec, ok, err := sd.cmd.Ask(sd.ctx(), "perm-1")
			out := []outcome{{"read it open", askSeen{rec, ok}, err}}
			out = append(out, outcome{"answer none", nil, sd.cmd.Answer(sd.ctx(), sd.next(), "nope", agent.AskAnswer{OptionID: "allow"})})
			out = append(out, outcome{"a bad answer", nil, sd.cmd.Answer(sd.ctx(), sd.next(), "perm-1", agent.AskAnswer{OptionID: "bogus"})})
			out = append(out, outcome{"answer", nil, sd.cmd.Answer(sd.ctx(), sd.next(), "perm-1", agent.AskAnswer{OptionID: "allow"})})
			out = append(out, outcome{"answer again", nil, sd.cmd.Answer(sd.ctx(), sd.next(), "perm-1", agent.AskAnswer{OptionID: "allow"})})
			rec, ok, err = sd.cmd.Ask(sd.ctx(), "perm-1")
			out = append(out, outcome{"read it resolved", askSeen{rec, ok}, err})
			rec, ok, err = sd.cmd.Ask(sd.ctx(), "nope")
			return append(out, outcome{"read none", askSeen{rec, ok}, err})
		}},
	}
}

// askSeen is an Ask read's answer.
type askSeen struct {
	rec agent.AskRecord
	ok  bool
}

// TestEveryCommandOverTheWireIsTheEngines (§3.14, A16): each script, run over
// the socket (a Session, attached, driving a host) and in process (a twin
// host's engine called directly), answers the same: every result equal (times,
// which two engines stamp apart, compared as present), and every error with
// the same text, code, reason and errors.Is over every sentinel a caller
// matches.
func TestEveryCommandOverTheWireIsTheEngines(t *testing.T) {
	for _, sc := range parityScripts() {
		t.Run(sc.name, func(t *testing.T) {
			wire := newHost(t, sc.opts...)
			local := newHost(t, sc.opts...)
			for _, h := range []*host{wire, local} {
				if sc.setup != nil {
					sc.setup(h.stub)
				}
			}
			s, _ := started(t, wire, newTap(t), remote.SessionOptions{})
			over := sc.run(&side{t: t, cmd: s, stub: wire.stub})
			in := sc.run(&side{t: t, cmd: &inProcess{e: local.eng, client: local.eng.NewClientID()}, stub: local.stub})
			if len(over) != len(in) {
				t.Fatalf("%d outcomes over the wire, %d in process", len(over), len(in))
			}
			set := matchSet()
			for i := range in {
				o, p := over[i], in[i]
				if !sameResult(o.res, p.res) {
					t.Errorf("%s: over the wire %+v, in process %+v", p.what, o.res, p.res)
				}
				switch {
				case (o.err == nil) != (p.err == nil):
					t.Errorf("%s: over the wire %v, in process %v", p.what, o.err, p.err)
					continue
				case p.err == nil:
					continue
				}
				if o.err.Error() != p.err.Error() {
					t.Errorf("%s: the text over the wire %q, in process %q", p.what, o.err, p.err)
				}
				var e *remote.Error
				if !errors.As(o.err, &e) || string(e.Code) != engine.Code(p.err) || string(e.Reason) != engine.Reason(p.err) {
					t.Errorf("%s: over the wire %#v, in process %s/%s", p.what, o.err, engine.Code(p.err), engine.Reason(p.err))
				}
				for name, sentinel := range set {
					if errors.Is(o.err, sentinel) != errors.Is(p.err, sentinel) {
						t.Errorf("%s: errors.Is(_, %s) is %v over the wire, %v in process (%v)",
							p.what, name, errors.Is(o.err, sentinel), errors.Is(p.err, sentinel), p.err)
					}
				}
				if errors.Is(p.err, engine.ErrIndexWrite) && errors.Unwrap(o.err).Error() != errors.Unwrap(p.err).Error() {
					t.Errorf("%s: the cause over the wire %q, in process %q", p.what, errors.Unwrap(o.err), errors.Unwrap(p.err))
				}
			}
		})
	}
}

// sameResult says two results are the same answer: equal once each time in
// them — which two engines stamp at different instants — is known present on
// both sides and cleared, and once a cancel's outcome — requested or
// settled, by whether the turn's ending beat the call's return — is known to
// be one of the two on both.
func sameResult(a, b any) bool {
	return reflect.DeepEqual(normalized(a), normalized(b))
}

func normalized(v any) any {
	row := func(q agent.QueuedPrompt) agent.QueuedPrompt {
		if !q.QueuedAt.IsZero() {
			q.QueuedAt = time.Time{}
		} else {
			q.Text += " (no time)"
		}
		return q
	}
	switch v := v.(type) {
	case engine.SubmitResult:
		if v.Queued != nil {
			q := row(*v.Queued)
			v.Queued = &q
		}
		return v
	case agent.QueuedPrompt:
		if v == (agent.QueuedPrompt{}) {
			return v
		}
		return row(v)
	case []agent.QueuedPrompt:
		if v == nil {
			return "no rows"
		}
		out := []agent.QueuedPrompt{}
		for _, q := range v {
			out = append(out, row(q))
		}
		return out
	case engine.CancelResult:
		if v.Outcome == engine.CancelSettled {
			v.Outcome = engine.CancelRequested
		}
		return v
	case askSeen:
		// What the wire carries of a record (X6: the host's turn token,
		// delivery fields and incarnation stay home), times as present.
		return struct {
			rec any
			ok  bool
		}{askFields(v.rec), v.ok}
	case backend.Settings:
		cfg, _ := agent.EncodeConfigState(&agent.ConfigState{Options: v.Config})
		return [3]string{v.Model, v.Mode, string(cfg)}
	}
	return v
}

// askFields is what an Ask read compares: every field the wire carries, times
// as present or not.
func askFields(r agent.AskRecord) any {
	var body [3]string
	if r.Body.Permission != nil {
		b, _ := agent.EncodePermissionEvent(r.Body.Permission)
		body[0] = string(b)
	}
	if r.Body.Question != nil {
		b, _ := agent.EncodeQuestionEvent(r.Body.Question)
		body[1] = string(b)
	}
	if r.Body.Plan != nil {
		b, _ := agent.EncodePlanEvent(r.Body.Plan)
		body[2] = string(b)
	}
	answers := r.Answer.Answers
	if len(answers) == 0 {
		answers = nil
	}
	r.Answer.Answers = answers
	return struct {
		ID, Kind, Status, Outcome, By string
		Answer                        agent.AskAnswer
		Body                          [3]string
		Opened, Resolved              bool
	}{r.ID, string(r.Kind), string(r.Status), string(r.Outcome), r.By, r.Answer, body, !r.OpenedAt.IsZero(), !r.ResolvedAt.IsZero()}
}
