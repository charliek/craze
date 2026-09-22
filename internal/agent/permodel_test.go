package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// Plan 025 C1 over F1's `permodel` scripts: cursor as it answers today, where
// every model has an option catalog of its own and set_config_option(model, X)
// answers with X's (cmd/craze-fake-agent/permodel.go). A reply's catalog is
// installed by the read loop in arrival order (design 1), a model change is one
// call announced by one delta (design 2), and nothing here waits on a clock but
// a watchdog: the orders are fixed by the fake's wire, by the session's own
// setter barrier, and by holding the agent's pushes at the read loop.

// permodelFresh is the catalog the permodel fake answers for each model on a
// fresh session, as "id=value" pairs in cursor's order.
var permodelFresh = map[string]string{
	"grok-4.6":      "mode=agent model=grok-4.6 effort=high fast=true",
	"composer-2.5":  "mode=agent model=composer-2.5 fast=false",
	"claude-opus-5": "mode=agent model=claude-opus-5 thinking=true context=300k effort=high fast=false",
	"glm-5.2":       "mode=agent model=glm-5.2 reasoning=high",
}

// cfgString is a catalog as "id=value" pairs, in order: everything a control
// is drawn from, in one comparable string.
func cfgString(cfg []ConfigOption) string {
	out := make([]string, 0, len(cfg))
	for _, o := range cfg {
		out = append(out, o.ID+"="+o.Current)
	}
	return strings.Join(out, " ")
}

// wantOnModel asserts the snapshot's model and its whole catalog.
func wantOnModel(t *testing.T, snap Snapshot, model, cfg string) {
	t.Helper()
	if snap.CurrentModel != model {
		t.Fatalf("the session is on %q, want %q", snap.CurrentModel, model)
	}
	if got := cfgString(snap.Config); got != cfg {
		t.Fatalf("the catalog is\n  %s\nwant\n  %s", got, cfg)
	}
}

// foldedSnap is the snapshot a client folding evs holds, in the sections the
// status row's chips are drawn from.
func foldedSnap(evs []Event) Snapshot {
	f := foldDeltas(evs)
	return Snapshot{Provider: CursorProvider().Info(), CurrentModel: f.Model, Config: f.Config}
}

// chipText is the effort and fast chips of the status row over snap, from the
// two functions the TUI's modelLabel draws them with (internal/tui/status.go):
// the effort select's value, and "fast" when the toggle is on.
func chipText(snap Snapshot) string {
	var bits []string
	if opt := EffortOption(snap); opt != nil && opt.Current != "" {
		bits = append(bits, opt.Current)
	}
	if FastOn(snap) {
		bits = append(bits, "fast")
	}
	return strings.Join(bits, " · ")
}

// providerCatalog is the fake agent's own catalog, read by asking it: a
// set_config_option of the mode to the mode it is already in, whose reply is
// the agent's whole catalog as the agent holds it. It is a probe sent through
// the client directly, and its reply is installed like any other — so it is the
// last thing a case does, after everything it asserts about the session.
func providerCatalog(t *testing.T, s *session) string {
	t.Helper()
	cat, err := s.client.SetConfig(context.Background(), "mode", s.Snapshot().CurrentMode)
	if err != nil {
		t.Fatalf("the probe: %v", err)
	}
	if !cat.Present {
		t.Fatal("the probe's reply carried no catalog")
	}
	return cfgString(parseConfigOptions(cat.Options))
}

// ownDeltas are the deltas the command cause published, in order.
func ownDeltas(evs []Event, cause string) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Type == EventMeta && ev.State != nil && ev.Cause == cause {
			out = append(out, ev)
		}
	}
	return out
}

// flushAll commits everything enqueued and reads the primary dry.
func flushAll(s *session) []Event {
	_ = s.log.Flush(context.Background(), s.done)
	return drainBuffered(s)
}

// pushGate stands in the session's update handler, on the read loop, for the
// agent's config_option_update pushes alone: with hold set a push waits there,
// holding the read loop exactly where the wire has it, until the test lets it
// go; and every push that has been applied says so on applied. It is the read
// loop's own call into onUpdate, delayed — nothing else about the path changes.
type pushGate struct {
	hold    chan struct{}
	applied chan struct{}
}

func gatePushes(t *testing.T, s *session, held bool) *pushGate {
	t.Helper()
	g := &pushGate{applied: make(chan struct{}, 16)}
	if held {
		g.hold = make(chan struct{})
		// A push still held when the test ends would hold the read loop, and
		// with it the session's close.
		t.Cleanup(g.release)
	}
	s.client.SetUpdateHandler(func(n acp.SessionNotification) {
		var u struct {
			SessionUpdate string `json:"sessionUpdate"`
		}
		_ = json.Unmarshal(n.Update, &u)
		if u.SessionUpdate != acp.UpdateConfigOption {
			s.onUpdate(n)
			return
		}
		if g.hold != nil {
			<-g.hold
		}
		s.onUpdate(n)
		g.applied <- struct{}{}
	})
	return g
}

func (g *pushGate) release() {
	select {
	case <-g.hold:
	default:
		close(g.hold)
	}
}

func (g *pushGate) awaitApplied(t *testing.T) {
	t.Helper()
	select {
	case <-g.applied:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's push was never applied")
	}
}

// configPushOf is a config_option_update carrying cfg, in the wire's shape: what
// the agent sends when it moves an option on its own.
func configPushOf(cfg []ConfigOption) acp.SessionNotification {
	opts := make([]map[string]any, 0, len(cfg))
	for _, o := range cfg {
		values := make([]map[string]string, 0, len(o.SelectValues))
		for _, v := range o.SelectValues {
			values = append(values, map[string]string{"value": v.Value, "name": v.Name})
		}
		opts = append(opts, map[string]any{
			"id": o.ID, "name": o.Name, "category": o.Category, "type": o.Type,
			"currentValue": o.Current, "options": values,
		})
	}
	// The id is the other helpers' (settings_test.go): onUpdate, driven
	// directly as the read loop drives it, routes nothing by it.
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update:    mustJSON(map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": opts}),
	}
}

// withValue is cfg with option id at value, as a fresh slice.
func withValue(cfg []ConfigOption, id, value string) []ConfigOption {
	out := cloneConfig(cfg)
	for i := range out {
		if out[i].ID == id {
			out[i].Current = value
		}
	}
	return out
}

// TestTheCatalogFollowsTheModel is the defect plan 025 exists for, both ways
// round: cursor's catalog is per model, and a session that kept the one it
// started on offered effort on composer-2.5, which has none (setting it was
// refused), and would have hidden it on the way back. Now every switch installs
// the destination's catalog — composer-2.5 loses effort and keeps fast,
// grok-4.6 gets effort back, claude-opus-5 has effort, context, thinking and
// fast — and a client folding the stream ends exactly where Snapshot() does.
func TestTheCatalogFollowsTheModel(t *testing.T) {
	s := startScript(t, "permodel", false)
	awaitCatalog(t, s)
	wantOnModel(t, s.Snapshot(), "grok-4.6", permodelFresh["grok-4.6"])
	for i, step := range []struct {
		model        string
		effort, fast bool
	}{
		{"composer-2.5", false, true},
		{"grok-4.6", true, true},
		{"claude-opus-5", true, true},
		{"glm-5.2", true, false},
	} {
		out, err := s.SetModel(context.Background(), fmt.Sprintf("c-1/%d", i+1), step.model)
		if err != nil {
			t.Fatalf("SetModel(%q): %v", step.model, err)
		}
		if out.Value != step.model {
			t.Fatalf("SetModel(%q) confirmed %q", step.model, out.Value)
		}
		snap := s.Snapshot()
		wantOnModel(t, snap, step.model, permodelFresh[step.model])
		if got := EffortOption(snap) != nil; got != step.effort {
			t.Fatalf("on %s the effort control is offered: %v, want %v", step.model, got, step.effort)
		}
		if got := FastOption(snap) != nil; got != step.fast {
			t.Fatalf("on %s the fast toggle is offered: %v, want %v", step.model, got, step.fast)
		}
	}
	wantFoldMatchesSnapshot(t, flushAll(s), s.Snapshot())
}

// TestAModelSwitchIsOneDelta is §4's journal check at the seam: a cursor-shaped
// switch publishes exactly one event — an EventMeta carrying the Model and the
// Config section, and nothing else — because cursor answers the one call with
// the catalog and pushes nothing (X1.1). `craze prompt --json` prints nothing
// for it, and a later event's Seq moves by one, as it did when the delta
// carried the model alone.
func TestAModelSwitchIsOneDelta(t *testing.T) {
	s := startScript(t, "permodel", false)
	settle(t, s)
	if _, err := s.SetModel(context.Background(), "c-1/1", "composer-2.5"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	evs := flushAll(s)
	if len(evs) != 1 {
		t.Fatalf("a switch published %d events, want one:\n%s", len(evs), formatEvents(evs))
	}
	ev := evs[0]
	if ev.Type != EventMeta || ev.Cause != "c-1/1" || ev.State == nil || ev.Mode != "" || ev.Text != "" {
		t.Fatalf("the switch published %+v", ev)
	}
	st := ev.State
	if st.Model == nil || *st.Model != "composer-2.5" || st.Config == nil {
		t.Fatalf("the delta carries %+v, want the Model and the Config section", st)
	}
	if got := cfgString(st.Config.Options); got != permodelFresh["composer-2.5"] {
		t.Fatalf("the delta's catalog is %s", got)
	}
	if st.Title != nil || st.Mode != nil || st.Commands != nil || st.Plugins != nil || st.SendNow != nil ||
		st.Reason != "" || st.Detail != "" || st.IndexErr != "" {
		t.Fatalf("the delta carries more than the two sections: %+v", st)
	}
}

// TestAReplysCatalogIsOrderedWithPushesAtTheBoundary is panel astra 2: a
// settings reply and an agent push that both carry a catalog are applied in the
// order they reached the wire, never in the order two goroutines took s.mu.
// Before, the setter installed the reply from its own goroutine, so a push that
// arrived AFTER the reply but was applied before the setter's section was
// overwritten by the older reply — and the overwrite got the newer revision.
//
// Each order is forced, not sampled. The fake fixes the wire (a push written
// just ahead of the reply, or just behind it: permodel-pushbefore / -pushafter
// / -pushmodel-after), and the setter's section is put on either side of a push
// behind the reply — held at the read loop until the setter has returned, or
// waited for from inside the setter's barrier (beforeSetSection). The push
// moves an option the reply does not (fast), or the model itself; in every case
// the later of the two wins on the provider, the snapshot, the folded deltas
// and the chips, and SetOutcome.Value is the model the session ended on when
// the setter's section ran.
func TestAReplysCatalogIsOrderedWithPushesAtTheBoundary(t *testing.T) {
	const (
		wire         = "the wire's order"
		sectionFirst = "the setter's section, then the push"
		pushFirst    = "the push, then the setter's section"
	)
	claudeFast := strings.Replace(permodelFresh["claude-opus-5"], "fast=false", "fast=true", 1)
	for _, tc := range []struct {
		script, order string
		value         string // SetOutcome.Value
		model, cfg    string // where everything ends
		own           string // the catalog the setter's own delta carries
		chips         string
	}{
		{"permodel-pushbefore", wire, "claude-opus-5", "claude-opus-5", permodelFresh["claude-opus-5"], permodelFresh["claude-opus-5"], "high"},
		{"permodel-pushafter", sectionFirst, "claude-opus-5", "claude-opus-5", claudeFast, permodelFresh["claude-opus-5"], "high · fast"},
		{"permodel-pushafter", pushFirst, "claude-opus-5", "claude-opus-5", claudeFast, claudeFast, "high · fast"},
		{"permodel-pushmodel-after", sectionFirst, "claude-opus-5", "glm-5.2", permodelFresh["glm-5.2"], permodelFresh["claude-opus-5"], "high"},
		{"permodel-pushmodel-after", pushFirst, "glm-5.2", "glm-5.2", permodelFresh["glm-5.2"], permodelFresh["glm-5.2"], "high"},
	} {
		t.Run(tc.script+"/"+tc.order, func(t *testing.T) {
			s := startScript(t, tc.script, false)
			awaitCatalog(t, s)
			var g *pushGate
			switch tc.order {
			case sectionFirst:
				g = gatePushes(t, s, true)
			case pushFirst:
				g = gatePushes(t, s, false)
				s.beforeSetSection = func() { g.awaitApplied(t) }
			}
			out, err := s.SetModel(context.Background(), "c-1/1", "claude-opus-5")
			if err != nil {
				t.Fatalf("SetModel: %v", err)
			}
			if tc.order == sectionFirst {
				g.release()
				g.awaitApplied(t)
			}
			if out.Value != tc.value {
				t.Fatalf("SetModel confirmed %q, want %q", out.Value, tc.value)
			}
			snap := s.Snapshot()
			wantOnModel(t, snap, tc.model, tc.cfg)
			evs := flushAll(s)
			wantFoldMatchesSnapshot(t, evs, snap)
			own := ownDeltas(evs, "c-1/1")
			if len(own) != 1 || own[0].State.Model == nil || own[0].State.Config == nil {
				t.Fatalf("the setter published %d deltas, want one carrying both sections:\n%s", len(own), formatEvents(evs))
			}
			if got := cfgString(own[0].State.Config.Options); got != tc.own {
				t.Fatalf("the setter's delta carries\n  %s\nwant\n  %s", got, tc.own)
			}
			if got := chipText(foldedSnap(evs)); got != tc.chips {
				t.Fatalf("the chips over the folded state read %q, want %q", got, tc.chips)
			}
			if got := chipText(snap); got != tc.chips {
				t.Fatalf("the chips over the snapshot read %q, want %q", got, tc.chips)
			}
			if got := providerCatalog(t, s); got != tc.cfg {
				t.Fatalf("the agent holds\n  %s\nand the session ended on\n  %s", got, tc.cfg)
			}
		})
	}
}

// TestTheSetModelFallbackDropsThePreviousModelsOptions is panel astra 3: when
// the model moves by session/set_model — no model option advertised, or the
// agent refusing the config path as an unknown option — the reply carries no
// catalog, and what the session holds are the PREVIOUS model's options. They
// are dropped, mode and model kept, rather than offered for a model that may
// not have them, and the next catalog the agent sends brings the new model's
// back. A model the agent refuses is refused on either path: nothing changes
// and nothing is published.
func TestTheSetModelFallbackDropsThePreviousModelsOptions(t *testing.T) {
	t.Run("the config path refused", func(t *testing.T) {
		s := startScript(t, "permodel-refuse", false)
		settle(t, s)
		out, err := s.SetModel(context.Background(), "c-1/1", "composer-2.5")
		if err != nil || out.Value != "composer-2.5" {
			t.Fatalf("SetModel = (%+v, %v)", out, err)
		}
		// The model option survives, moved to the model set_model moved to, so
		// the Model and the Config section of the one delta agree.
		wantOnModel(t, s.Snapshot(), "composer-2.5", "mode=agent model=composer-2.5")
		evs := flushAll(s)
		if len(evs) != 1 || evs[0].State == nil || evs[0].State.Model == nil || evs[0].State.Config == nil {
			t.Fatalf("the fallback published:\n%s", formatEvents(evs))
		}
		if got := cfgString(evs[0].State.Config.Options); got != "mode=agent model=composer-2.5" {
			t.Fatalf("the fallback's delta carries %s", got)
		}
		// The next catalog the agent answers is composer-2.5's, fast and all.
		if _, err := s.SetConfig(context.Background(), "c-1/2", "fast", "true"); err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		wantOnModel(t, s.Snapshot(), "composer-2.5", "mode=agent model=composer-2.5 fast=true")
		if got := providerCatalog(t, s); got != "mode=agent model=composer-2.5 fast=true" {
			t.Fatalf("the agent holds %s", got)
		}
	})
	t.Run("no model option", func(t *testing.T) {
		s := startScript(t, "permodel-nomodel", false)
		settle(t, s)
		if ModelConfigOption(s.Snapshot()) != nil {
			t.Fatal("permodel-nomodel advertised a model option")
		}
		out, err := s.SetModel(context.Background(), "c-1/1", "composer-2.5")
		if err != nil || out.Value != "composer-2.5" {
			t.Fatalf("SetModel = (%+v, %v)", out, err)
		}
		wantOnModel(t, s.Snapshot(), "composer-2.5", "mode=agent")
		// No option to read the model from: the marker is what keeps a first
		// model option carrying grok-4.6 from rolling this back (r27 finding 2).
		s.mu.Lock()
		armed := s.modelBeforeSet["grok-4.6"]
		s.mu.Unlock()
		if !armed {
			t.Fatal("a set_model with no model option armed no marker")
		}
		if _, err := s.SetConfig(context.Background(), "c-1/2", "fast", "true"); err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		wantOnModel(t, s.Snapshot(), "composer-2.5", "mode=agent fast=true")
	})
	// A change to the model the session is already on brought no catalog
	// because it needed none: what the session holds is that model's, and
	// nothing is dropped — by set_model with no model option to go by, or by
	// the one call answered {}.
	for _, tc := range []struct{ script, model, cfg string }{
		{"effort", "default", "effort=medium fast=false"},
		{"permodel-noreply", "grok-4.6", permodelFresh["grok-4.6"]},
	} {
		t.Run("the model it is already on, over "+tc.script, func(t *testing.T) {
			s := startScript(t, tc.script, false)
			settle(t, s)
			if _, err := s.SetModel(context.Background(), "c-1/1", tc.model); err != nil {
				t.Fatalf("SetModel: %v", err)
			}
			wantOnModel(t, s.Snapshot(), tc.model, tc.cfg)
		})
	}
	for _, script := range []string{"permodel", "permodel-refuse"} {
		t.Run("a refused model on "+script, func(t *testing.T) {
			s := startScript(t, script, false)
			settle(t, s)
			before := s.Snapshot()
			_, err := s.SetModel(context.Background(), "c-1/1", "no-such-model")
			var rpcErr *acp.RPCError
			if !errors.As(err, &rpcErr) || !strings.HasPrefix(rpcErr.DataMessage(), "Invalid model value") {
				t.Fatalf("an unknown model answered %v, want the agent's refusal", err)
			}
			wantOnModel(t, s.Snapshot(), before.CurrentModel, cfgString(before.Config))
			if evs := flushAll(s); len(evs) != 0 {
				t.Fatalf("a refused model published:\n%s", formatEvents(evs))
			}
		})
	}
}

// TestConfigPathRefused is the fallback's decision alone: -32601, or -32602
// whose data says the option is unknown, sends the change on to set_model
// (X1.3); a model refused — cursor's "Invalid model value", grok's bare string
// "unknown model id" (X2) — and everything else is the caller's error.
func TestConfigPathRefused(t *testing.T) {
	data := func(s string) json.RawMessage { return json.RawMessage(s) }
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"-32601", &acp.RPCError{Code: acp.CodeMethodNotFound, Message: "Method not found"}, true},
		{"unknown option", &acp.RPCError{Code: acp.CodeInvalidParams, Message: "Invalid params",
			Data: data(`{"message":"Unknown model config option: model"}`)}, true},
		{"unknown option, wrapped", errors.Join(errors.New("ctx"), &acp.RPCError{Code: acp.CodeInvalidParams,
			Data: data(`{"message":"Unknown model config option: model"}`)}), true},
		{"invalid model", &acp.RPCError{Code: acp.CodeInvalidParams, Message: "Invalid params",
			Data: data(`{"message":"Invalid model value: nope"}`)}, false},
		{"grok's unknown model", &acp.RPCError{Code: acp.CodeInvalidParams, Message: "Invalid params",
			Data: data(`"unknown model id"`)}, false},
		{"-32602 with no data", &acp.RPCError{Code: acp.CodeInvalidParams, Message: "Invalid params"}, false},
		{"another code", &acp.RPCError{Code: -32000, Data: data(`{"message":"Unknown model config option: model"}`)}, false},
		{"not an RPC error", acp.ErrClosed, false},
		{"nil", nil, false},
	} {
		if got := configPathRefused(tc.err); got != tc.want {
			t.Errorf("%s: configPathRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// installMetas are the deltas carrying a Model section: on a new session, the
// install's alone when nothing else moved the model.
func modelMetas(evs []Event) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Type == EventMeta && ev.State != nil && ev.State.Model != nil {
			out = append(out, ev)
		}
	}
	return out
}

// TestAStartupModelOverrideRefreshesTheCatalog is panel astra 6 for a new
// session: --model takes SetModel's path, so the session starts on the model's
// own catalog — composer-2.5 without effort, claude-opus-5 with thinking and
// context and its own effort values — and the install delta carries it, with
// no delta of its own. The shipped install merge rules still hold: an update of
// the agent's own that lands between the override and the install is newer and
// is kept (r27 finding 1), and a push the agent wrote ahead of the override's
// reply is older than that reply and is not.
func TestAStartupModelOverrideRefreshesTheCatalog(t *testing.T) {
	for _, tc := range []struct {
		script, model, cfg string
	}{
		{"permodel", "composer-2.5", permodelFresh["composer-2.5"]},
		{"permodel", "claude-opus-5", permodelFresh["claude-opus-5"]},
		// The fallback at start-up: the previous model's options dropped, and
		// the agent's own catalog — which the probe below reads — is the one
		// the next reply brings back.
		{"permodel-refuse", "composer-2.5", "mode=agent model=composer-2.5"},
		// A push ahead of the reply is older than it: the reply's catalog wins.
		{"permodel-pushbefore", "claude-opus-5", permodelFresh["claude-opus-5"]},
	} {
		t.Run(tc.script+"/"+tc.model, func(t *testing.T) {
			s := startScriptOpts(t, tc.script, Options{Model: tc.model})
			awaitCatalog(t, s)
			snap := s.Snapshot()
			wantOnModel(t, snap, tc.model, tc.cfg)
			evs := flushAll(s)
			wantFoldMatchesSnapshot(t, evs, snap)
			// The install's delta says the model, and nothing else does except
			// the push that pushbefore writes, which is the agent's own.
			var install []Event
			for _, ev := range modelMetas(evs) {
				if ev.State.Commands != nil {
					install = append(install, ev)
				}
			}
			if len(install) != 1 {
				t.Fatalf("want exactly one install delta:\n%s", formatEvents(evs))
			}
			if tc.script == "permodel" && len(modelMetas(evs)) != 1 {
				t.Fatalf("the override published a delta of its own:\n%s", formatEvents(evs))
			}
			if got := providerCatalog(t, s); got != permodelFresh[tc.model] {
				t.Fatalf("the agent holds %s", got)
			}
		})
	}
	t.Run("claude-opus-5's effort values", func(t *testing.T) {
		s := startScriptOpts(t, "permodel", Options{Model: "claude-opus-5"})
		opt := EffortOption(s.Snapshot())
		if opt == nil || len(opt.SelectValues) != 5 || opt.SelectValues[4].Value != "max" {
			t.Fatalf("the effort select is %+v, want claude-opus-5's five levels", opt)
		}
	})
	t.Run("an update racing the install", func(t *testing.T) {
		s := newTestSession(t, Options{
			Binary:    fakeAgentPath(t),
			ExtraArgs: []string{"-script=permodel"},
			Workspace: t.TempDir(),
			Stderr:    io.Discard,
			Model:     "composer-2.5",
		})
		// After the override's reply has been installed, before the snapshot
		// session/new answered is: the agent turns fast on, on its own.
		s.beforeInstall = func() {
			s.mu.Lock()
			cfg := withValue(s.snap.Config, "fast", "true")
			s.mu.Unlock()
			s.onUpdate(configPushOf(cfg))
		}
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		awaitCatalog(t, s)
		want := strings.Replace(permodelFresh["composer-2.5"], "fast=false", "fast=true", 1)
		snap := s.Snapshot()
		wantOnModel(t, snap, "composer-2.5", want)
		wantFoldMatchesSnapshot(t, flushAll(s), snap)
	})
}

// TestAResumeModelOverrideRefreshesTheCatalog is the resume twin: on a load the
// override writes through s.mu after the restored snapshot is installed, and
// says so in ONE delta, as it always did — carrying the Config section beside
// the Model one now, so the event count is today's. cursor's load result
// carries the loaded model's catalog (X1.5), so a cursor resume takes the
// one-call path; grok's carries none, so a grok resume takes set_model and has
// nothing to drop.
func TestAResumeModelOverrideRefreshesTheCatalog(t *testing.T) {
	for _, tc := range []struct {
		script, model, cfg string
	}{
		{"permodel", "composer-2.5", permodelFresh["composer-2.5"]},
		{"permodel", "claude-opus-5", permodelFresh["claude-opus-5"]},
		{"grok-load", "grok-4.6", ""},
	} {
		t.Run(tc.script+"/"+tc.model, func(t *testing.T) {
			s := newLoadSession(t, tc.script, func(o *Options) { o.Model = tc.model })
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			snap := s.Snapshot()
			wantOnModel(t, snap, tc.model, tc.cfg)
			evs := flushAll(s)
			wantFoldMatchesSnapshot(t, evs, snap)
			var after []Event
			ended := false
			for _, ev := range evs {
				if ev.Type == EventReplay && ev.Replay != nil && ev.Replay.Phase == ReplayEnd {
					ended = true
					continue
				}
				if ended && ev.Type == EventMeta {
					after = append(after, ev)
				}
			}
			if len(after) != 1 {
				t.Fatalf("the resume override published %d deltas after the replay, want one:\n%s", len(after), formatEvents(evs))
			}
			st := after[0].State
			if st == nil || st.Model == nil || *st.Model != tc.model || st.Config == nil || after[0].Cause != "" {
				t.Fatalf("the override's delta is %+v", after[0])
			}
			if got := cfgString(st.Config.Options); got != tc.cfg {
				t.Fatalf("the override's delta carries %s", got)
			}
		})
	}
}

// TestSetOutcomeCarriesTheInstalledValue is panel astra 5: what a setter
// answers is what the session is now at, read in the section that announces it
// — the installed catalog's value, and the agent's later word where the agent
// has moved it since — never the value that was asked for. An option the
// answered catalog no longer lists is ErrOptionGone, and what was installed is
// still announced.
func TestSetOutcomeCarriesTheInstalledValue(t *testing.T) {
	t.Run("an option the agent moved behind its reply", func(t *testing.T) {
		s := startScript(t, "permodel", false)
		settle(t, s)
		// The reply sets effort to low; the agent then moves it to xhigh on its
		// own, and that push reaches the read loop before the setter's section
		// runs — driven here, from inside the setter's barrier, through the read
		// loop's own entry point.
		s.beforeSetSection = func() {
			s.mu.Lock()
			cfg := withValue(s.snap.Config, "effort", "xhigh")
			s.mu.Unlock()
			s.onUpdate(configPushOf(cfg))
		}
		out, err := s.SetConfig(context.Background(), "c-1/1", "effort", "low")
		if err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		if out.Value != "xhigh" {
			t.Fatalf("SetConfig confirmed %q, want the value the session is at", out.Value)
		}
		evs := flushAll(s)
		own := ownDeltas(evs, "c-1/1")
		if len(own) != 1 || configCurrent(Snapshot{Config: own[0].State.Config.Options}, "effort") != "xhigh" {
			t.Fatalf("the setter's delta is not what it confirmed:\n%s", formatEvents(evs))
		}
		if own[0].State.Model != nil {
			t.Fatalf("an effort change that moved no model carried the Model section: %+v", own[0].State)
		}
	})
	t.Run("a model the agent moved behind its reply", func(t *testing.T) {
		s := startScript(t, "permodel-pushmodel-after", false)
		settle(t, s)
		g := gatePushes(t, s, false)
		s.beforeSetSection = func() { g.awaitApplied(t) }
		out, err := s.SetModel(context.Background(), "c-1/1", "claude-opus-5")
		if err != nil {
			t.Fatalf("SetModel: %v", err)
		}
		if out.Value != "glm-5.2" {
			t.Fatalf("SetModel confirmed %q, want the model the agent moved on to", out.Value)
		}
	})
	t.Run("an option the answered catalog no longer has", func(t *testing.T) {
		s := startScript(t, "permodel-empty", false)
		settle(t, s)
		out, err := s.SetConfig(context.Background(), "c-1/1", "fast", "false")
		if !errors.Is(err, ErrOptionGone) {
			t.Fatalf("SetConfig answered %v, want ErrOptionGone", err)
		}
		if out.Ticket == nil {
			t.Fatal("ErrOptionGone came with no ticket: the install went unannounced")
		}
		if got := cfgString(s.Snapshot().Config); got != "" {
			t.Fatalf("an empty reply left the catalog %s", got)
		}
		evs := flushAll(s)
		own := ownDeltas(evs, "c-1/1")
		if len(own) != 1 || own[0].State.Config == nil || len(own[0].State.Config.Options) != 0 {
			t.Fatalf("the install was not announced:\n%s", formatEvents(evs))
		}
		if out.Ticket.Seq() != own[0].Seq {
			t.Fatalf("the ticket names seq %d, the delta is %d", out.Ticket.Seq(), own[0].Seq)
		}
	})
}

// TestAnEmptyCatalogClearsAndAMissingOneKeeps is panel astra 14 at the session:
// a reply carrying `[]` clears the catalog — mode and model options too — and a
// reply carrying none leaves the catalog standing, with the one option it set
// at its new value. (`{}` to a MODEL change is the fallback's case: the kept
// catalog is the previous model's, and its knobs go.) A malformed catalog is
// refused at the ACP layer (acp's TestASettingsReplyKeepsItsCatalogsPresence).
func TestAnEmptyCatalogClearsAndAMissingOneKeeps(t *testing.T) {
	t.Run("a catalog replaces", func(t *testing.T) {
		s := startScript(t, "permodel", false)
		settle(t, s)
		if _, err := s.SetConfig(context.Background(), "c-1/1", "fast", "false"); err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		wantOnModel(t, s.Snapshot(), "grok-4.6", "mode=agent model=grok-4.6 effort=high fast=false")
	})
	t.Run("an empty catalog clears", func(t *testing.T) {
		s := startScript(t, "permodel-empty", false)
		awaitCatalog(t, s)
		out, err := s.SetModel(context.Background(), "c-1/1", "composer-2.5")
		if err != nil || out.Value != "composer-2.5" {
			t.Fatalf("SetModel = (%+v, %v)", out, err)
		}
		// Nothing left to read the model from, so it is the one asked for, and
		// the marker guards the option's return.
		wantOnModel(t, s.Snapshot(), "composer-2.5", "")
		s.mu.Lock()
		armed := s.modelBeforeSet["grok-4.6"]
		s.mu.Unlock()
		if !armed {
			t.Fatal("a model change that left no model option armed no marker")
		}
		wantFoldMatchesSnapshot(t, flushAll(s), s.Snapshot())
	})
	t.Run("no catalog keeps", func(t *testing.T) {
		// The echo fake answers set_config_option with {} (after a push of its
		// own): the catalog stands, the option at its value.
		s := startScript(t, "effort", false)
		settle(t, s)
		if _, err := s.SetConfig(context.Background(), "c-1/1", "effort", "high"); err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		if got := cfgString(s.Snapshot().Config); got != "effort=high fast=false" {
			t.Fatalf("a reply with no catalog left %s", got)
		}
	})
	t.Run("no catalog to a model change", func(t *testing.T) {
		s := startScript(t, "permodel-noreply", false)
		settle(t, s)
		out, err := s.SetModel(context.Background(), "c-1/1", "composer-2.5")
		if err != nil || out.Value != "composer-2.5" {
			t.Fatalf("SetModel = (%+v, %v)", out, err)
		}
		wantOnModel(t, s.Snapshot(), "composer-2.5", "mode=agent model=composer-2.5")
		// The agent's own catalog is composer-2.5's; the next reply brings it.
		if got := providerCatalog(t, s); got != permodelFresh["composer-2.5"] {
			t.Fatalf("the agent holds %s", got)
		}
		wantOnModel(t, s.Snapshot(), "composer-2.5", permodelFresh["composer-2.5"])
	})
}
