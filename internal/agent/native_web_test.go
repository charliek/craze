package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/websearch"
)

// The web tools' decision and consent through a real native session (plan 051
// §C.3–§C.5): what the model is offered under each policy, config and
// remembered choice, what the consent card does, and what is remembered. The
// web is always a fake MCP server on loopback.

// ---- the decision function --------------------------------------------------

// TestDecideWeb is DecideWeb's whole table (plan 051 §C.3, F.2): with no flag,
// the config × remembered choice × canAsk product and the override; with each
// flag, from each origin, attended or not, the refusals and the list. The
// flags arrive with C6; the rows are pinned here so the function's shape is
// final.
func TestDecideWeb(t *testing.T) {
	const invalidWhy = "config.toml websearch is not one of its two forms, so web search is off (...)"
	const fileWhy = "websearch.json is not valid JSON, so no web search choice is remembered"
	cfgs := map[string]WebConfig{
		"absent":  {},
		"off":     {Setting: WebSettingOff},
		"on":      {Setting: WebSettingOn, Provider: "exa"},
		"invalid": {Setting: WebSettingInvalid, Why: invalidWhy},
	}
	choices := map[string]struct {
		c   websearch.Choice
		why string
	}{
		"none":     {},
		"exa":      {c: websearch.Choice{Provider: "exa"}},
		"disabled": {c: websearch.Choice{Disabled: true}},
		"corrupt":  {why: fileWhy},
	}
	type want struct {
		mode     harness.WebMode
		provider string
		notes    []string
	}
	on := want{mode: harness.WebOn, provider: "exa"}

	// No flag: the config × remembered choice × canAsk product.
	for cn, cfg := range cfgs {
		for rn, r := range choices {
			for _, canAsk := range []bool{false, true} {
				var w want
				switch {
				case cn == "off":
				case cn == "invalid":
					w.notes = []string{invalidWhy}
				case cn == "on", rn == "exa":
					w = on
				case rn == "disabled":
				default:
					if rn == "corrupt" {
						w.notes = []string{fileWhy}
					}
					if canAsk {
						w.mode, w.provider = harness.WebAsking, "exa"
					}
				}
				for _, attended := range []bool{false, true} {
					name := fmt.Sprintf("config %s, remembered %s, canAsk %t, attended %t", cn, rn, canAsk, attended)
					d, err := DecideWeb(WebInputs{Attended: attended, Native: true, Config: cfg, Remembered: r.c, RememberedWhy: r.why, CanAsk: canAsk})
					if err != nil || d.Mode != w.mode || d.Provider != w.provider || !slices.Equal(d.Notes, w.notes) {
						t.Errorf("%s: %+v, %v; want %+v", name, d, err, w)
					}
				}
			}
		}
	}

	// An invalid override is off with a line, after config.toml's word; a
	// config that is off already says nothing about it.
	for cn, w := range map[string]want{
		"absent":  {notes: []string{webOverrideNote}},
		"on":      {notes: []string{webOverrideNote}},
		"off":     {},
		"invalid": {notes: []string{invalidWhy}},
	} {
		d, err := DecideWeb(WebInputs{Native: true, Config: cfgs[cn], OverrideInvalid: true, Remembered: websearch.Choice{Provider: "exa"}, CanAsk: true})
		if err != nil || d.Mode != harness.WebWithheld || !slices.Equal(d.Notes, w.notes) {
			t.Errorf("an invalid override, config %s: %+v, %v; want withheld, %q", cn, d, err, w.notes)
		}
	}

	// Not native: withheld, with nothing said.
	if d, err := DecideWeb(WebInputs{Config: cfgs["invalid"], Remembered: websearch.Choice{Provider: "exa"}, CanAsk: true}); err != nil || d.Mode != harness.WebWithheld || d.Notes != nil {
		t.Errorf("not native: %+v, %v", d, err)
	}

	// --no-web-search: off, whatever else is said, never refused, and a
	// no-op off native.
	for cn, cfg := range cfgs {
		for _, native := range []bool{false, true} {
			for _, origin := range []WebFlagOrigin{WebFlagGiven, WebFlagRecorded} {
				d, err := DecideWeb(WebInputs{Flag: WebFlagOff, Origin: origin, Native: native, Config: cfg,
					OverrideInvalid: cn == "on", Remembered: websearch.Choice{Provider: "exa"}, CanAsk: true})
				if err != nil || d.Mode != harness.WebWithheld {
					t.Errorf("--no-web-search, config %s, native %t: %+v, %v", cn, native, d, err)
				}
			}
		}
	}

	// --web-search given now, or recorded and reloaded unattended: refused
	// for a non-native provider, a config that is off, invalid or unreadable,
	// and an invalid override; otherwise on, over a remembered disable and
	// with nobody to ask, on the config table's provider.
	for _, tc := range []struct {
		origin   WebFlagOrigin
		attended bool
		prefix   string
	}{
		{WebFlagGiven, false, "--web-search cannot be honoured: "},
		{WebFlagGiven, true, "--web-search cannot be honoured: "},
		{WebFlagRecorded, false, "this session was created with --web-search, which cannot be honoured: "},
	} {
		base := WebInputs{Flag: WebFlagOn, Origin: tc.origin, Attended: tc.attended, Native: true, Remembered: websearch.Choice{Disabled: true}}
		for name, in := range map[string]WebInputs{
			"not native":       with(base, func(in *WebInputs) { in.Native = false }),
			"config off":       with(base, func(in *WebInputs) { in.Config = cfgs["off"] }),
			"config invalid":   with(base, func(in *WebInputs) { in.Config = cfgs["invalid"] }),
			"override invalid": with(base, func(in *WebInputs) { in.OverrideInvalid = true }),
		} {
			_, err := DecideWeb(in)
			r, ok := err.(*WebRefusal)
			if !ok || !strings.HasPrefix(r.Msg, tc.prefix) {
				t.Errorf("--web-search (%v, attended %t), %s: %v; want a refusal starting %q", tc.origin, tc.attended, name, err, tc.prefix)
			}
			if name == "config invalid" && ok && !strings.HasSuffix(r.Msg, invalidWhy) {
				t.Errorf("the refusal %q does not say why the config is invalid", r.Msg)
			}
		}
		for cn, w := range map[string]want{"absent": on, "on": on} {
			in := base
			in.Config = cfgs[cn]
			d, err := DecideWeb(in)
			if err != nil || d.Mode != w.mode || d.Provider != w.provider || d.Notes != nil {
				t.Errorf("--web-search (%v, attended %t), config %s: %+v, %v; want on", tc.origin, tc.attended, cn, d, err)
			}
		}
	}

	// A recorded --web-search a person resumes attended is never refused: it
	// falls through to the list, and opens off, with a line, when the config
	// has turned web search off since.
	resumed := WebInputs{Flag: WebFlagOn, Origin: WebFlagRecorded, Attended: true, Native: true, CanAsk: true}
	for name, tc := range map[string]struct {
		in   WebInputs
		want want
	}{
		"config off":       {with(resumed, func(in *WebInputs) { in.Config = cfgs["off"] }), want{notes: []string{webRecordedNote}}},
		"config invalid":   {with(resumed, func(in *WebInputs) { in.Config = cfgs["invalid"] }), want{notes: []string{invalidWhy, webRecordedNote}}},
		"override invalid": {with(resumed, func(in *WebInputs) { in.OverrideInvalid = true }), want{notes: []string{webOverrideNote}}},
		"not native":       {with(resumed, func(in *WebInputs) { in.Native = false }), want{}},
		"config absent":    {resumed, on},
	} {
		d, err := DecideWeb(tc.in)
		if err != nil || d.Mode != tc.want.mode || d.Provider != tc.want.provider || !slices.Equal(d.Notes, tc.want.notes) {
			t.Errorf("a recorded --web-search resumed attended, %s: %+v, %v; want %+v", name, d, err, tc.want)
		}
	}
}

func with(in WebInputs, f func(*WebInputs)) WebInputs {
	f(&in)
	return in
}

// ---- a fake MCP server ------------------------------------------------------

// fakeExa is an MCP endpoint on loopback answering every search with one
// result and every fetch with one page, counting what it was sent.
type fakeExa struct {
	srv   *httptest.Server
	mu    sync.Mutex
	tools []string
}

func newFakeExa(t *testing.T) *fakeExa {
	t.Helper()
	f := &fakeExa{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID     int64 `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &call)
		f.mu.Lock()
		f.tools = append(f.tools, call.Params.Name)
		f.mu.Unlock()
		text := "Title: Fake result\nURL: https://fake.example/\nHighlights:\nfound it"
		if call.Params.Name == "web_fetch_exa" {
			text = "the fetched page"
		}
		res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", res)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeExa) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tools)
}

// webFixture is a native fixture whose web search goes to a fake server.
func webFixture(t *testing.T) (*nativeFixture, *fakeExa) {
	t.Helper()
	f := newNativeFixture(t)
	exa := newFakeExa(t)
	f.env[websearch.TestEndpointEnv] = exa.srv.URL + "/mcp"
	return f, exa
}

// offeredWeb runs one turn on s and reports whether the model was offered
// websearch and webfetch (both or neither: anything else fails).
func offeredWeb(t *testing.T, f *nativeFixture, s *nativeSession) bool {
	t.Helper()
	m := f.models["test/a"]
	before := len(m.requests())
	m.push(answer("ok"))
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	names := toolNamesOf(m.requests()[before])
	search, fetch := slices.Contains(names, "websearch"), slices.Contains(names, "webfetch")
	if search != fetch {
		t.Fatalf("offered %v: one web tool without the other", names)
	}
	return search
}

func searchArgs(t *testing.T, q string) string { return nativeArgs(t, map[string]any{"query": q}) }

// stateFile is the remembered choice's contents, "" when there is none.
func stateFile(t *testing.T, f *nativeFixture) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, websearch.StateFile))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeState(t *testing.T, f *nativeFixture, body string) {
	t.Helper()
	writeNativeFile(t, filepath.Join(f.dir, websearch.StateFile), body)
}

const (
	stateExa = `{"version":1,"provider":"exa"}` + "\n"
	stateOff = `{"version":1,"provider":false}` + "\n"
)

// ---- policy, config and remembered choice ------------------------------------

// TestNativeWebPolicy (plan 051 §C.3): with nothing decided, the web tools
// are offered only where the consent question would reach a person — the ask
// policy, wherever it comes from — and withheld where questions are answered
// automatically, however interactive the session.
func TestNativeWebPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		opts Options
		want bool
	}{
		"interactive, first-option policy": {Options{Interactive: true, Approval: &ApprovalPolicy{Question: ApprovalFirstOption}}, false},
		"non-interactive, ask policy":      {Options{Approval: &ApprovalPolicy{Question: ApprovalAsk, Plan: ApprovalAccept}}, true},
		"default interactive":              {Options{Interactive: true}, true},
		"default headless":                 {Options{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, exa := webFixture(t)
			s := f.started(tc.opts)
			if got := offeredWeb(t, f, s); got != tc.want {
				t.Fatalf("offered the web tools: %t, want %t", got, tc.want)
			}
			if stateFile(t, f) != "" || len(exa.calls()) != 0 {
				t.Fatalf("opening wrote %q, sent %v", stateFile(t, f), exa.calls())
			}
		})
	}
}

// TestNativeWebConfigAndState is the config × remembered-choice matrix (plan
// 051 §C.1–§C.3) for a session that can ask: config.toml off, invalid or
// unreadable is off whatever is remembered, with a line for the invalid; a
// valid table is on; absent defers to the file, which is on, off, or —
// absent, corrupt, of an unknown version — undecided (offered, asking), with
// a line for the file it could not use.
func TestNativeWebConfigAndState(t *testing.T) {
	const invalidWhy = "config.toml could not be parsed, so web search is off (...)"
	cfgs := map[string]WebConfig{
		"absent":  {},
		"false":   {Setting: WebSettingOff},
		"table":   {Setting: WebSettingOn, Provider: "exa"},
		"invalid": {Setting: WebSettingInvalid, Why: invalidWhy},
	}
	states := map[string]string{
		"absent":          "",
		"exa":             stateExa,
		"false":           stateOff,
		"corrupt":         "{not json",
		"unknown version": `{"version":2,"provider":"exa"}`,
	}
	for cn, cfg := range cfgs {
		for sn, body := range states {
			t.Run(cn+" × "+sn, func(t *testing.T) {
				f, _ := webFixture(t)
				if body != "" {
					writeState(t, f, body)
				}
				diag := &lockedDiag{}
				s := f.started(Options{Interactive: true, WebSearch: cfg, Diag: diag})
				var want harness.WebMode
				var said string
				switch {
				case cn == "false":
				case cn == "invalid":
					said = invalidWhy
				case cn == "table", sn == "exa":
					want = harness.WebOn
				case sn == "false":
				default:
					want = harness.WebAsking
					if sn == "corrupt" || sn == "unknown version" {
						said = websearch.StateFile
					}
				}
				if got := offeredWeb(t, f, s); got != (want != harness.WebWithheld) {
					t.Fatalf("offered the web tools: %t, want mode %v", got, want)
				}
				out := diag.String()
				if said != "" && !strings.Contains(out, "native: "+said) {
					t.Fatalf("diagnostics %q; want a line about %q", out, said)
				}
				if said == "" && strings.Contains(out, "web search") {
					t.Fatalf("diagnostics %q; want nothing about web search", out)
				}
				if got := stateFile(t, f); got != body {
					t.Fatalf("the remembered choice changed at open: %q", got)
				}
			})
		}
	}
}

// TestNativeWebOverride: a valid CRAZE_TEST_EXA_MCP is where the calls go
// (the fake server); an invalid one is off with a line naming the variable,
// never a failed Start, and the line does not echo the value.
func TestNativeWebOverride(t *testing.T) {
	f, exa := webFixture(t)
	s := f.started(Options{Interactive: true, WebSearch: WebConfig{Setting: WebSettingOn, Provider: "exa"}})
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "websearch", searchArgs(t, "golang")), answer("done"))
	if _, err := s.Prompt(context.Background(), "look it up"); err != nil {
		t.Fatal(err)
	}
	if got := exa.calls(); !slices.Equal(got, []string{"web_search_exa"}) {
		t.Fatalf("the fake server was sent %v", got)
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; !strings.Contains(text, "## [Fake result](https://fake.example/)") {
		t.Fatalf("the model was told %q", text)
	}

	g, _ := webFixture(t)
	g.env[websearch.TestEndpointEnv] = "http://example.com/mcp-secret-path"
	diag := &lockedDiag{}
	s = g.started(Options{Interactive: true, WebSearch: WebConfig{Setting: WebSettingOn, Provider: "exa"}, Diag: diag})
	if offeredWeb(t, g, s) {
		t.Fatal("an invalid override offered the web tools")
	}
	if out := diag.String(); !strings.Contains(out, "native: CRAZE_TEST_EXA_MCP is not a loopback http URL, so web search is off") || strings.Contains(out, "secret") {
		t.Fatalf("diagnostics %q", out)
	}
}

// ---- consent through the card ------------------------------------------------

// TestNativeWebConsentAllow: an undecided interactive session asks on the
// first web call with an ordinary question card; Allow sends the call to the
// provider, remembers "exa", and the next session searches without asking.
func TestNativeWebConsentAllow(t *testing.T) {
	f, exa := webFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "websearch", searchArgs(t, "golang")), answer("done"))
	out := startPrompt(s, "look it up")
	q := w.waitType(EventQuestion).Question
	if q == nil || len(q.Questions) != 1 || q.Title != "Web search" || q.Auto {
		t.Fatalf("the card: %+v", q)
	}
	opts := q.Questions[0].Options
	if q.Questions[0].Prompt != websearch.ConsentQuestion || len(opts) != 2 || opts[0].Label != "Allow via Exa" ||
		opts[0].Description != "Queries and URLs are sent to Exa (exa.ai)." || opts[1].Label != "Disable web search" {
		t.Fatalf("the card's question: %+v", q.Questions[0])
	}
	if len(exa.calls()) != 0 {
		t.Fatal("the call reached the provider before the person answered")
	}
	answerQuestion(t, s, q, 0)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatal(got.err)
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; !strings.Contains(text, "found it") {
		t.Fatalf("the model was told %q", text)
	}
	if got := stateFile(t, f); got != stateExa {
		t.Fatalf("remembered %q", got)
	}

	// The next session: on, never asks.
	next := f.started(Options{Interactive: true})
	nw := newNativeWatcher(t, next)
	m.push(nativeCallStep("c2", "webfetch", nativeArgs(t, map[string]any{"url": "https://go.dev/"})), answer("done"))
	if got := await(t, startPrompt(next, "fetch it"), "the prompt"); got.err != nil {
		t.Fatal(got.err)
	}
	nw.waitTerminal()
	if nw.count(EventQuestion) != 0 {
		t.Fatal("a session with web search allowed asked again")
	}
	if got := exa.calls(); !slices.Equal(got, []string{"web_search_exa", "web_fetch_exa"}) {
		t.Fatalf("the fake server was sent %v", got)
	}
}

// TestNativeWebConsentDisable: Disable answers this call (and every later
// one) that web search is disabled, sends nothing, remembers false, and the
// next session is offered no web tools.
func TestNativeWebConsentDisable(t *testing.T) {
	f, exa := webFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "websearch", searchArgs(t, "golang")),
		nativeCallStep("c2", "websearch", searchArgs(t, "again")), answer("done"))
	out := startPrompt(s, "look it up")
	answerQuestion(t, s, w.waitType(EventQuestion).Question, 1)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatal(got.err)
	}
	w.waitTerminal()
	reqs := m.requests()
	if a, b := toolResultsOf(reqs[1])["c1"], toolResultsOf(reqs[2])["c2"]; a != websearch.DisabledText || b != websearch.DisabledText {
		t.Fatalf("the model was told %q, then %q", a, b)
	}
	if w.count(EventQuestion) != 1 || len(exa.calls()) != 0 {
		t.Fatalf("%d cards, provider sent %v", w.count(EventQuestion), exa.calls())
	}
	if got := stateFile(t, f); got != stateOff {
		t.Fatalf("remembered %q", got)
	}
	if offeredWeb(t, f, f.started(Options{Interactive: true})) {
		t.Fatal("the next session was offered the web tools")
	}
}

// TestNativeWebConsentSkip: a skipped card declines for this session only:
// the call is told web search was not approved, a second call is told the
// same without a second card, nothing is remembered and nothing sent, and the
// next session asks again.
func TestNativeWebConsentSkip(t *testing.T) {
	f, exa := webFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "websearch", searchArgs(t, "golang")),
		nativeCallStep("c2", "webfetch", nativeArgs(t, map[string]any{"url": "https://go.dev/"})), answer("done"))
	out := startPrompt(s, "look it up")
	q := w.waitType(EventQuestion).Question
	if _, err := s.Asks().Answer("test/skip", q.ID, AskAnswer{Skip: true}); err != nil {
		t.Fatal(err)
	}
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatal(got.err)
	}
	w.waitTerminal()
	reqs := m.requests()
	if a, b := toolResultsOf(reqs[1])["c1"], toolResultsOf(reqs[2])["c2"]; a != websearch.DeclinedText || b != websearch.DeclinedText {
		t.Fatalf("the model was told %q, then %q", a, b)
	}
	if w.count(EventQuestion) != 1 || len(exa.calls()) != 0 || stateFile(t, f) != "" {
		t.Fatalf("%d cards, provider sent %v, remembered %q", w.count(EventQuestion), exa.calls(), stateFile(t, f))
	}
	if !offeredWeb(t, f, f.started(Options{Interactive: true})) {
		t.Fatal("the next session was not offered the web tools: a skip was remembered")
	}
}

// TestNativeWebFirstOptionNeverConsents (plan 051 §C.4, acceptance C): a
// headless session whose web state is forced undecided — offered and asking,
// past the decision that would have withheld it — still never asks: its
// asker would answer the card's first option, Allow, on nobody's behalf. The
// call is declined, no card or automatic answer is recorded, nothing is
// remembered and nothing reaches the provider.
func TestNativeWebFirstOptionNeverConsents(t *testing.T) {
	f, exa := webFixture(t)
	f.edit = func(o *harness.Options) { o.Web = harness.WebState{Mode: harness.WebAsking} }
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "websearch", searchArgs(t, "golang")), answer("done"))
	if got := await(t, startPrompt(s, "look it up"), "the prompt"); got.err != nil {
		t.Fatal(got.err)
	}
	w.waitTerminal()
	if !slices.Contains(toolNamesOf(m.requests()[0]), "websearch") {
		t.Fatal("the forced state did not offer the tools: the test proves nothing")
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; text != websearch.DeclinedText {
		t.Fatalf("the model was told %q", text)
	}
	if n := w.count(EventQuestion); n != 0 || len(s.Asks().Resolved()) != 0 {
		t.Fatalf("%d cards, %d asks recorded: the consent question was put to the policy", n, len(s.Asks().Resolved()))
	}
	if stateFile(t, f) != "" || len(exa.calls()) != 0 {
		t.Fatalf("remembered %q, provider sent %v", stateFile(t, f), exa.calls())
	}
}

// ---- resume --------------------------------------------------------------

// TestNativeWebResumeIntoWithheld (plan 051 §C.5): a transcript holding web
// calls resumes into a session that withholds the tools — the calls are
// history — and the resumed session goes on without them.
func TestNativeWebResumeIntoWithheld(t *testing.T) {
	f, exa := webFixture(t)
	ws := t.TempDir()
	s := f.started(Options{Workspace: ws, WebSearch: WebConfig{Setting: WebSettingOn, Provider: "exa"}})
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "websearch", searchArgs(t, "golang")),
		nativeCallStep("c2", "webfetch", nativeArgs(t, map[string]any{"url": "https://go.dev/"})), answer("done"))
	if _, err := s.Prompt(context.Background(), "look it up"); err != nil {
		t.Fatal(err)
	}
	if len(exa.calls()) != 2 {
		t.Fatalf("the provider was sent %v", exa.calls())
	}
	id := s.Snapshot().SessionID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	resumed := f.session(Options{Workspace: ws, LoadSessionID: id, WebSearch: WebConfig{Setting: WebSettingOff}})
	if _, err := startLoad(t, resumed); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.push(answer("carried on"))
	if _, err := resumed.Prompt(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	reqs := m.requests()
	last := reqs[len(reqs)-1]
	if slices.Contains(toolNamesOf(last), "websearch") || slices.Contains(toolNamesOf(last), "webfetch") {
		t.Fatalf("the resumed session offered %v", toolNamesOf(last))
	}
	if results := toolResultsOf(last); !strings.Contains(results["c1"], "found it") || results["c2"] != "the fetched page" {
		t.Fatalf("the resumed history lost the web calls' results: %q", results)
	}
}
