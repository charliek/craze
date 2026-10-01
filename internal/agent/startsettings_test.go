package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// The start settings (plan 032 §3.11, P6): --effort and --fast/--no-fast set
// inside Start, after --model, on both providers. The live cases run the fake
// agent with its call record (CRAZE_FAKE_DUMP_CALLS) — the order craze wrote
// its calls in — so "before the first prompt" is read off the wire, not
// inferred.

// TestEffortValueMatchRules is §3.11's three rules, in order, each needing
// exactly one value: the exact id, then the id in any case, then the name in
// any case. A rule that matches two values is ambiguous and never falls
// through to a looser one; nothing matching is none; no option is unoffered.
// "MAX" is the order's negative control: by name it would be "fastest".
func TestEffortValueMatchRules(t *testing.T) {
	opt := &ConfigOption{ID: "effort", SelectValues: []SelectValue{
		{Value: "low", Name: "Low"},
		{Value: "high", Name: "High"},
		{Value: "xhigh", Name: "Extra High"},
		{Value: "max", Name: "Fastest"},
		{Value: "fastest", Name: "Max"},
		{Value: "Turbo", Name: "Turbo A"},
		{Value: "turbo", Name: "Turbo B"},
		{Value: "a1", Name: "Same"},
		{Value: "a2", Name: " same "},
	}}
	for _, tc := range []struct {
		want, value, why string
	}{
		{"high", "high", ""},
		{"HIGH", "high", ""},
		{"extra high", "xhigh", ""},
		{"MAX", "max", ""},
		{"Fastest", "fastest", ""},
		{"Turbo", "Turbo", ""},
		{"TURBO", "", effortAmbiguous},
		{"same", "", effortAmbiguous},
		{"ultra", "", effortNone},
	} {
		value, why := effortValue(opt, tc.want)
		if value != tc.value || why != tc.why {
			t.Errorf("effortValue(%q) = %q, %q; want %q, %q", tc.want, value, why, tc.value, tc.why)
		}
	}
	if value, why := effortValue(nil, "high"); value != "" || why != effortUnoffered {
		t.Errorf("no option: %q, %q; want unoffered", value, why)
	}
}

// TestFastValue is --fast and --no-fast through FastOnOff: cursor's toggle
// both ways; a toggle with no off value takes --fast and not --no-fast; no
// toggle takes neither.
func TestFastValue(t *testing.T) {
	cursor := &ConfigOption{ID: "fast", Category: fastCategory, SelectValues: []SelectValue{
		{Value: "true", Name: "Fast\u200b\u200b"}, {Value: "false", Name: "Off"},
	}}
	onOnly := &ConfigOption{ID: "fast", Category: fastCategory, SelectValues: []SelectValue{{Value: "turbo", Name: "Turbo"}}}
	for _, tc := range []struct {
		name  string
		opt   *ConfigOption
		on    bool
		value string
		ok    bool
	}{
		{"cursor on", cursor, true, "true", true},
		{"cursor off", cursor, false, "false", true},
		{"on only, on", onOnly, true, "turbo", true},
		{"on only, off", onOnly, false, "", false},
		{"none", nil, true, "", false},
	} {
		if value, ok := fastValue(tc.opt, tc.on); value != tc.value || ok != tc.ok {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, value, ok, tc.value, tc.ok)
		}
	}
}

// dumpCalls points the fake agent's call record at a file of the test's and
// answers its reader: the lines so far.
func dumpCalls(t *testing.T) func() []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calls")
	t.Setenv("CRAZE_FAKE_DUMP_CALLS", path)
	return func() []string {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

// sets is calls' set_config_option lines, "<id>=<value>" each.
func sets(calls []string) []string {
	var out []string
	for _, c := range calls {
		if v, ok := strings.CutPrefix(c, "session/set_config_option "); ok {
			out = append(out, v)
		}
	}
	return out
}

// before reports whether calls has a line a ahead of every line b — and both.
func before(calls []string, a, b string) bool {
	ia, ib := slices.Index(calls, a), slices.Index(calls, b)
	return ia >= 0 && ib >= 0 && ia < ib
}

// optCurrent is the snapshot's current value of option id, "" with none.
func optCurrent(snap Snapshot, id string) string {
	for _, o := range snap.Config {
		if o.ID == id {
			return o.Current
		}
	}
	return ""
}

func boolPtr(v bool) *bool { return &v }

// startSettingsSession is script started with opts, journalled into a
// directory of the test's, its diag lane captured.
func startSettingsSession(t *testing.T, script string, opts Options) (*session, *bytes.Buffer) {
	t.Helper()
	var diag bytes.Buffer
	opts.Diag = &diag
	opts.JournalDir = filepath.Join(t.TempDir(), "journal")
	return startScriptOpts(t, script, opts), &diag
}

// TestStartSettingsAreSetBeforeStartReturns: a new session's --effort and
// --fast are sent after session/new and before Start returns — so the first
// prompt is written after both — and the install publishes them: the
// snapshot and a client folding the stream agree. The negative control is the
// same start without them: nothing is set, and the agent's defaults stand.
func TestStartSettingsAreSetBeforeStartReturns(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		calls := dumpCalls(t)
		s, diag := startSettingsSession(t, "effort", Options{Effort: "low", Fast: boolPtr(true)})
		got := calls()
		if !before(got, "session/new", "session/set_config_option effort=low") ||
			!before(got, "session/set_config_option effort=low", "session/set_config_option fast=true") ||
			slices.Contains(got, "session/prompt") {
			t.Fatalf("Start's calls: %q", got)
		}
		snap := s.Snapshot()
		if optCurrent(snap, "effort") != "low" || optCurrent(snap, "fast") != "true" {
			t.Fatalf("the session starts at %s", cfgString(snap.Config))
		}
		wantFoldMatchesSnapshot(t, flushAll(s), snap)
		if _, err := s.Prompt(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		if got := calls(); !before(got, "session/set_config_option fast=true", "session/prompt") {
			t.Fatalf("the calls: %q", got)
		}
		if diag.Len() != 0 {
			t.Fatalf("a start that applied both said %q", diag)
		}
	})
	t.Run("absent", func(t *testing.T) {
		calls := dumpCalls(t)
		s, _ := startSettingsSession(t, "effort", Options{})
		if got := sets(calls()); len(got) != 0 {
			t.Fatalf("a start with neither set %q", got)
		}
		if snap := s.Snapshot(); optCurrent(snap, "effort") != "medium" || optCurrent(snap, "fast") != "false" {
			t.Fatalf("the session starts at %s", cfgString(snap.Config))
		}
	})
}

// TestStartEffortMatchesTheSessionsValues is effortValue through a real start:
// the id in another case, the name, a value the session already holds (sent
// as nothing), and one that matches nothing — skipped, journalled
// effort_unmatched with the values offered, said on Diag, and the session
// starts at its own.
func TestStartEffortMatchesTheSessionsValues(t *testing.T) {
	for _, tc := range []struct {
		effort, want string
		sent         []string
	}{
		{"LOW", "low", []string{"effort=low"}},
		{"High", "high", []string{"effort=high"}},
		{"medium", "medium", nil},
		{"ultra", "medium", nil},
	} {
		t.Run(tc.effort, func(t *testing.T) {
			calls := dumpCalls(t)
			s, diag := startSettingsSession(t, "effort", Options{Effort: tc.effort})
			if got := sets(calls()); !slices.Equal(got, tc.sent) {
				t.Fatalf("sent %q, want %q", got, tc.sent)
			}
			if got := optCurrent(s.Snapshot(), "effort"); got != tc.want {
				t.Fatalf("the session starts at effort %q, want %q", got, tc.want)
			}
			w := journalOf(t, s.log)
			closeJournaled(t, s, w)
			notes := diags(fileLines(t, w), diagEffortUnmatched)
			if tc.effort != "ultra" {
				if len(notes) != 0 || diag.Len() != 0 {
					t.Fatalf("a matched effort noted %v, said %q", notes, diag)
				}
				return
			}
			if len(notes) != 1 || notes[0]["effort"] != "ultra" || notes[0]["reason"] != effortNone || notes[0]["option"] != "effort" {
				t.Fatalf("the effort_unmatched notes: %v", notes)
			}
			if !strings.Contains(diag.String(), `craze: --effort "ultra" matches none of this session's efforts (low, medium, high)`) {
				t.Fatalf("Diag: %q", diag.String())
			}
		})
	}
}

// TestStartSettingsFollowTheModel: on cursor's per-model catalogs the settings
// are chosen after --model, against the catalog of the model the session
// starts on — claude-opus-5 has a max effort, grok-4.6 (the agent's own) does
// not, which is the negative control. Fast both ways and absent on grok-4.6,
// whose fast starts on: --no-fast sets it off, --fast sends nothing, neither
// leaves it. glm-5.2 has no fast toggle: fast_unmatched.
func TestStartSettingsFollowTheModel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   Options
		sent   []string
		effort string
		fast   string
		kind   string // the one diag kind noted, "" for none
	}{
		{"max after --model", Options{Model: "claude-opus-5", Effort: "max"}, []string{"model=claude-opus-5", "effort=max"}, "max", "false", ""},
		{"max without --model", Options{Effort: "max"}, nil, "high", "true", diagEffortUnmatched},
		{"--no-fast", Options{Fast: boolPtr(false)}, []string{"fast=false"}, "high", "false", ""},
		{"--fast, already on", Options{Fast: boolPtr(true)}, nil, "high", "true", ""},
		{"neither", Options{}, nil, "high", "true", ""},
		{"--fast on composer-2.5", Options{Model: "composer-2.5", Fast: boolPtr(true)}, []string{"model=composer-2.5", "fast=true"}, "", "true", ""},
		{"--fast on glm-5.2", Options{Model: "glm-5.2", Fast: boolPtr(true)}, []string{"model=glm-5.2"}, "", "", diagFastUnmatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := dumpCalls(t)
			s, _ := startSettingsSession(t, "permodel", tc.opts)
			if got := sets(calls()); !slices.Equal(got, tc.sent) {
				t.Fatalf("sent %q, want %q", got, tc.sent)
			}
			snap := s.Snapshot()
			if optCurrent(snap, "effort") != tc.effort || optCurrent(snap, "fast") != tc.fast {
				t.Fatalf("the session starts at %s", cfgString(snap.Config))
			}
			wantFoldMatchesSnapshot(t, flushAll(s), snap)
			w := journalOf(t, s.log)
			closeJournaled(t, s, w)
			lines := fileLines(t, w)
			for _, kind := range []string{diagEffortUnmatched, diagFastUnmatched, diagSettingRefused} {
				want := 0
				if kind == tc.kind {
					want = 1
				}
				if got := len(diags(lines, kind)); got != want {
					t.Fatalf("%d %s notes, want %d", got, kind, want)
				}
			}
		})
	}
}

// TestARefusedStartSettingIsNotedAndTheSessionStarts: the agent refusing
// --effort's set (-32602) is journalled start_setting_refused and said on
// Diag, the session starts at its own effort, and --fast after it is still
// applied. The negative control is a set that is never answered: that is no
// refusal, and the start fails (TestAnUnansweredStartSettingFailsTheStart).
func TestARefusedStartSettingIsNotedAndTheSessionStarts(t *testing.T) {
	t.Setenv("CRAZE_FAKE_SET_REFUSE", "effort")
	s, diag := startSettingsSession(t, "effort", Options{Effort: "low", Fast: boolPtr(true)})
	snap := s.Snapshot()
	if optCurrent(snap, "effort") != "medium" || optCurrent(snap, "fast") != "true" {
		t.Fatalf("the session starts at %s", cfgString(snap.Config))
	}
	if !strings.Contains(diag.String(), "craze: --effort low was refused, and the session starts without it:") {
		t.Fatalf("Diag: %q", diag.String())
	}
	w := journalOf(t, s.log)
	closeJournaled(t, s, w)
	notes := diags(fileLines(t, w), diagSettingRefused)
	if len(notes) != 1 || notes[0]["setting"] != "effort" || notes[0]["option"] != "effort" || notes[0]["value"] != "low" ||
		!strings.Contains(notes[0]["error"].(string), "Invalid params") {
		t.Fatalf("the start_setting_refused notes: %v", notes)
	}
}

// gateSets holds every set_config_option the fake agent is sent unanswered
// (CRAZE_FAKE_SET_GATE): nothing ever writes the FIFO.
func gateSets(t *testing.T) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SET_GATE", fifo)
}

// TestAnUnansweredStartSettingFailsTheStart (plan 032 X47): a set the agent
// never answers fails the start once startSettingWait has passed, naming the
// flag — on a new session and on a load, for --effort and for --fast — under
// a caller's context that never ends, context.Background(), as the TUI's
// start and a detached host's have it. The caller's own cancellation is still
// its own error, not reworded as the bound's.
func TestAnUnansweredStartSettingFailsTheStart(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		load         bool
		set          func(*Options)
		sent, flag   string
	}{
		{"new, --effort", "effort", false, func(o *Options) { o.Effort = "low" }, "effort=low", "--effort low"},
		{"new, --fast", "effort", false, func(o *Options) { o.Fast = boolPtr(true) }, "fast=true", "--fast"},
		{"load, --effort", "permodel", true, func(o *Options) { o.Effort = "low" }, "effort=low", "--effort low"},
		{"load, --no-fast", "permodel", true, func(o *Options) { o.Fast = boolPtr(false) }, "fast=false", "--no-fast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := startSettingWait
			startSettingWait = 200 * time.Millisecond
			t.Cleanup(func() { startSettingWait = prev })
			gateSets(t)
			calls := dumpCalls(t)
			var s *session
			if tc.load {
				s = newLoadSession(t, tc.script, tc.set)
			} else {
				opts := Options{
					Binary: fakeAgentPath(t), ExtraArgs: []string{"-script=" + tc.script},
					Workspace: t.TempDir(), Stderr: &bytes.Buffer{},
				}
				tc.set(&opts)
				s = newTestSession(t, opts)
			}
			done := make(chan error, 1)
			go func() { done <- s.Start(context.Background()) }()
			select {
			case err := <-done:
				want := "agent: " + tc.flag + ": the agent did not answer its set within 200ms"
				if err == nil || err.Error() != want {
					t.Fatalf("Start with its set unanswered: %v, want %q", err, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Start did not fail with its set unanswered")
			}
			if got := calls(); !slices.Contains(got, "session/set_config_option "+tc.sent) {
				t.Fatalf("the calls: %q", got)
			}
		})
	}
	t.Run("the caller's cancellation", func(t *testing.T) {
		gateSets(t)
		calls := dumpCalls(t)
		s := newTestSession(t, Options{
			Binary: fakeAgentPath(t), ExtraArgs: []string{"-script=effort"},
			Workspace: t.TempDir(), Stderr: &bytes.Buffer{}, Effort: "low",
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- s.Start(ctx) }()
		waitFor(t, "the set on the wire", func() bool { return slices.Contains(calls(), "session/set_config_option effort=low") })
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Start with its set unanswered: %v, want its context's end", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Start did not return once its context ended")
		}
	})
}

// TestStartSettingsApplyOnALoad: --effort and --fast apply to a loaded session
// too, after its replay — the session the load restored is the one they are
// for — each announced by SetConfig's own delta, since a load's snapshot is
// published before them; the fold agrees with the snapshot.
func TestStartSettingsApplyOnALoad(t *testing.T) {
	calls := dumpCalls(t)
	s := newLoadSession(t, "permodel", func(o *Options) { o.Effort = "low"; o.Fast = boolPtr(false) })
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if !before(got, "session/load", "session/set_config_option effort=low") ||
		!before(got, "session/set_config_option effort=low", "session/set_config_option fast=false") {
		t.Fatalf("the load's calls: %q", got)
	}
	snap := s.Snapshot()
	if optCurrent(snap, "effort") != "low" || optCurrent(snap, "fast") != "false" {
		t.Fatalf("the loaded session is at %s", cfgString(snap.Config))
	}
	evs := flushAll(s)
	wantFoldMatchesSnapshot(t, evs, snap)
	var after []Event
	ended := false
	for _, ev := range evs {
		if ev.Type == EventReplay && ev.Replay != nil && ev.Replay.Phase == ReplayEnd {
			ended = true
			continue
		}
		if ended && ev.Type == EventMeta && ev.State != nil && ev.State.Config != nil {
			after = append(after, ev)
		}
	}
	if len(after) != 2 {
		t.Fatalf("%d config deltas after the replay, want one per setting:\n%s", len(after), formatEvents(evs))
	}
}

// TestNativeStartSettings: native's one option, its effort select, set on the
// harness before the install — the snapshot says it and the first turn asks
// for it — matched by the same rules; an effort the model does not offer, or
// a model with none, is effort_unmatched; native has no fast toggle, so --fast
// is always fast_unmatched. Nothing is remembered: --effort is the start's,
// as --model is, where the session's own SetConfig is a switch the memory
// keeps (the negative control).
func TestNativeStartSettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   Options
		effort string
		kinds  []string
	}{
		{"low", Options{Effort: "low"}, "low", nil},
		{"LOW", Options{Effort: "LOW"}, "low", nil},
		{"none matches", Options{Effort: "medium"}, "high", []string{diagEffortUnmatched}},
		{"a model with no efforts", Options{Model: "other/c", Effort: "low"}, "", []string{diagEffortUnmatched}},
		{"after --model", Options{Model: "test/b", Effort: "medium"}, "medium", nil},
		{"--fast", Options{Fast: boolPtr(true)}, "high", []string{diagFastUnmatched}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			var diag bytes.Buffer
			tc.opts.Diag = &diag
			tc.opts.JournalDir = filepath.Join(t.TempDir(), "journal")
			s := f.session(tc.opts)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			snap := s.Snapshot()
			if got := optCurrent(snap, nativeEffortID); got != tc.effort {
				t.Fatalf("the session starts at effort %q, want %q", got, tc.effort)
			}
			if _, effort := s.hs.Current(); effort != tc.effort {
				t.Fatalf("the harness is at effort %q, want %q", effort, tc.effort)
			}
			model := f.models[snap.CurrentModel]
			model.push(answer("ok"))
			if _, err := s.Prompt(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			if got := callEffort(t, model.requests()[0], model.provider); got != tc.effort {
				t.Fatalf("the first turn asked for effort %q, want %q", got, tc.effort)
			}
			if _, err := os.Stat(filepath.Join(f.dir, modeltable.RecentFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the start wrote the model memory: %v", err)
			}
			w := journalOf(t, s.log)
			closeJournaled(t, s, w)
			lines := fileLines(t, w)
			var noted []string
			for _, kind := range []string{diagEffortUnmatched, diagFastUnmatched, diagSettingRefused} {
				for range diags(lines, kind) {
					noted = append(noted, kind)
				}
			}
			if !slices.Equal(noted, tc.kinds) {
				t.Fatalf("noted %q, want %q", noted, tc.kinds)
			}
			if (len(tc.kinds) == 0) != (diag.Len() == 0) || (diag.Len() > 0 && !strings.HasPrefix(diag.String(), "native: --")) {
				t.Fatalf("Diag: %q", diag.String())
			}
		})
	}
	t.Run("a switch in the session is remembered", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.session(Options{})
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "low", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(f.dir, modeltable.RecentFile)); err != nil {
			t.Fatalf("a switch in the session left no model memory: %v", err)
		}
	})
	t.Run("a load", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.session(Options{LoadSessionID: "0b8f3c1e-5d2a-4c7e-9f10-2a3b4c5d6e7f", Effort: "low"})
		if _, err := startLoad(t, s); err != nil {
			t.Fatal(err)
		}
		if got := optCurrent(s.Snapshot(), nativeEffortID); got != "low" {
			t.Fatalf("the loaded session is at effort %q, want low", got)
		}
	})
}

// callEffort is the reasoning effort a scripted native call carried, "" for
// none.
func callEffort(t *testing.T, call fantasy.Call, provider string) string {
	t.Helper()
	v, ok := call.ProviderOptions[provider]
	if !ok {
		return ""
	}
	opts, ok := v.(*openaicompat.ProviderOptions)
	if !ok || opts.ReasoningEffort == nil {
		t.Fatalf("provider options for %q = %#v, want an openaicompat reasoning effort", provider, v)
	}
	return string(*opts.ReasoningEffort)
}
