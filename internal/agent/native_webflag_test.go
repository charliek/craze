package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/websearch"
)

// The web search flags at the native Open (plan 051 F.1–F.3): the flag, where
// it came from and whether a person is opening the session, decided by the one
// function the CLI's early checks use too (DecideWeb, CheckWebFlag).

// TestTheNativeOpenAgreesWithDecideWeb: for every flag, origin, attendance,
// config.toml reading, remembered choice and override validity, a native
// session opens exactly as DecideWeb says — refused at Start with its words,
// or offered the web tools when the decision is on or asking and withheld
// otherwise — and what CheckWebFlag refuses ahead of a session (its refusal
// half) is exactly what the Open refuses. Opening never writes the
// remembered choice.
func TestTheNativeOpenAgreesWithDecideWeb(t *testing.T) {
	configs := map[string]WebConfig{
		"absent":  {},
		"off":     {Setting: WebSettingOff},
		"on":      {Setting: WebSettingOn, Provider: "exa"},
		"invalid": {Setting: WebSettingInvalid, Why: "config.toml could not be parsed, so web search is off (...)"},
	}
	states := map[string]struct {
		body string
		c    websearch.Choice
	}{"none": {}, "exa": {stateExa, websearch.Choice{Provider: "exa"}}, "disabled": {stateOff, websearch.Choice{Disabled: true}}}
	type row struct {
		flag            WebFlag
		origin          WebFlagOrigin
		attended        bool
		config, state   string
		overrideInvalid bool
	}
	var rows []row
	for _, flag := range []WebFlag{WebFlagAbsent, WebFlagOn, WebFlagOff} {
		for _, origin := range []WebFlagOrigin{WebFlagGiven, WebFlagRecorded} {
			if flag == WebFlagAbsent && origin == WebFlagRecorded {
				continue
			}
			for _, attended := range []bool{false, true} {
				for cn := range configs {
					for sn := range states {
						rows = append(rows, row{flag, origin, attended, cn, sn, false})
					}
				}
				rows = append(rows, row{flag, origin, attended, "absent", "exa", true})
			}
		}
	}
	for _, r := range rows {
		name := fmt.Sprintf("flag %q from %v, attended %t, config %s, remembered %s, override invalid %t",
			r.flag, r.origin, r.attended, r.config, r.state, r.overrideInvalid)
		t.Run(name, func(t *testing.T) {
			f, exa := webFixture(t)
			if r.overrideInvalid {
				f.env[websearch.TestEndpointEnv] = "http://example.com/mcp"
			}
			if b := states[r.state].body; b != "" {
				writeState(t, f, b)
			}
			want, wantErr := DecideWeb(WebInputs{
				Flag: r.flag, Origin: r.origin, Attended: r.attended, Native: true, Config: configs[r.config],
				OverrideInvalid: r.overrideInvalid, Remembered: states[r.state].c,
				// A default policy: an interactive session asks a person, a
				// headless one answers itself.
				CanAsk: r.attended,
			})
			getenv := func(k string) string { return f.env[k] }
			checked := CheckWebFlag(r.flag, r.origin, r.attended, true, func() WebConfig { return configs[r.config] }, getenv)
			if (checked == nil) != (wantErr == nil) || checked != nil && checked.Error() != wantErr.Error() {
				t.Fatalf("CheckWebFlag said %v; DecideWeb %v", checked, wantErr)
			}

			s := f.session(Options{Interactive: r.attended, WebSearch: configs[r.config], WebFlag: r.flag, WebFlagOrigin: r.origin})
			err := s.Start(context.Background())
			if wantErr != nil {
				var ce *nativeConfigError
				if err == nil || !errors.As(err, &ce) || err.Error() != "native: "+wantErr.Error() {
					t.Fatalf("Start = %v; want the refusal %q", err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			takeStartDelta(t, s.log)
			if got := offeredWeb(t, f, s); got != (want.Mode != harness.WebWithheld) {
				t.Fatalf("offered the web tools: %t; DecideWeb said %+v", got, want)
			}
			if got := stateFile(t, f); got != states[r.state].body || len(exa.calls()) != 0 {
				t.Fatalf("opening changed the remembered choice to %q, or sent %v", got, exa.calls())
			}
		})
	}
}

// TestWebSearchOverARememberedDisable (F.2): --web-search on a headless
// session that nobody can ask offers the tools over a remembered Disable, asks
// nothing, sends the call, and remembers nothing: the file is as it was, and
// with none there is none afterwards.
func TestWebSearchOverARememberedDisable(t *testing.T) {
	for _, body := range []string{stateOff, ""} {
		f, exa := webFixture(t)
		if body != "" {
			writeState(t, f, body)
		}
		s := f.started(Options{WebFlag: WebFlagOn, WebFlagOrigin: WebFlagGiven})
		w := newNativeWatcher(t, s)
		m := f.models["test/a"]
		m.push(nativeCallStep("c1", "websearch", searchArgs(t, "golang")), answer("done"))
		if _, err := s.Prompt(context.Background(), "look it up"); err != nil {
			t.Fatal(err)
		}
		w.waitTerminal()
		if n := w.count(EventQuestion); n != 0 || len(s.Asks().Resolved()) != 0 {
			t.Fatalf("remembered %q: %d cards, %d asks", body, n, len(s.Asks().Resolved()))
		}
		if got := exa.calls(); len(got) != 1 || got[0] != "web_search_exa" {
			t.Fatalf("remembered %q: the provider was sent %v", body, got)
		}
		if got := stateFile(t, f); got != body {
			t.Fatalf("remembered %q, and afterwards %q", body, got)
		}
	}
}

// TestNoWebSearchOverAConfigThatIsOn (F.2): --no-web-search beats config.toml's
// [websearch] and a remembered Allow, attended or not, given or recorded.
func TestNoWebSearchOverAConfigThatIsOn(t *testing.T) {
	for _, attended := range []bool{false, true} {
		for _, origin := range []WebFlagOrigin{WebFlagGiven, WebFlagRecorded} {
			f, _ := webFixture(t)
			writeState(t, f, stateExa)
			s := f.started(Options{Interactive: attended, WebFlag: WebFlagOff, WebFlagOrigin: origin,
				WebSearch: WebConfig{Setting: WebSettingOn, Provider: "exa"}})
			if offeredWeb(t, f, s) {
				t.Fatalf("attended %t, origin %v: offered the web tools", attended, origin)
			}
		}
	}
}

// TestARecordedWebSearchTheConfigTurnedOff (F.2, F.3): a session created with
// --web-search, reopened after config.toml turned web search off, fails to
// start when nobody attends it — a config error in DecideWeb's words — and,
// resumed by a person, opens without the web tools and says why.
func TestARecordedWebSearchTheConfigTurnedOff(t *testing.T) {
	off := WebConfig{Setting: WebSettingOff}
	f, _ := webFixture(t)
	s := f.session(Options{WebFlag: WebFlagOn, WebFlagOrigin: WebFlagRecorded, WebSearch: off})
	err := s.Start(context.Background())
	if info, ok := errkind.Of(err); !ok || info.Kind != errkind.Config ||
		err.Error() != "native: this session was created with --web-search, which cannot be honoured: config.toml turns web search off (websearch = false)" {
		t.Fatalf("an unattended reload: Start = %v", err)
	}

	g, _ := webFixture(t)
	diag := &lockedDiag{}
	resumed := g.started(Options{Interactive: true, WebFlag: WebFlagOn, WebFlagOrigin: WebFlagRecorded, WebSearch: off, Diag: diag})
	if offeredWeb(t, g, resumed) {
		t.Fatal("a person's resume was offered the web tools over a config that is off")
	}
	if out := diag.String(); !strings.Contains(out, "native: "+webRecordedNote) {
		t.Fatalf("diagnostics %q; want the line saying why", out)
	}
}
