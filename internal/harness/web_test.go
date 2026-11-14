package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// The web state (plan 051 §C.3, C2): what Options.Web offers a session and
// its children. The web is always fakeWebService: nothing here touches the
// network. web_identity_test.go holds the base the withheld state must keep.

// fakeWebService is a tool.Web that answers every search with one result and
// every fetch with one page, recording what it was asked.
type fakeWebService struct {
	mu      sync.Mutex
	queries []string
	urls    []string
}

func (w *fakeWebService) Search(_ context.Context, query string) ([]tool.WebResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queries = append(w.queries, query)
	return []tool.WebResult{{Title: "A result", URL: "https://example.com/r", Content: "result for " + query}}, nil
}

func (w *fakeWebService) Fetch(_ context.Context, url string) (tool.WebPage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.urls = append(w.urls, url)
	return tool.WebPage{URL: url, Text: "page at " + url}, nil
}

func (w *fakeWebService) asked() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append(slices.Clone(w.queries), w.urls...)
}

// The tool lists the web states give: a jobs-less session's and a child's,
// without and with the web tools, in the profile's order.
var (
	baseIDs     = []string{"bash", "read", "glob", "grep", "edit", "write", "agent", "agent_output", "todo_write", "ask_user_question", "exit_plan_mode"}
	withWebIDs  = []string{"bash", "read", "glob", "grep", "edit", "write", "agent", "agent_output", "webfetch", "todo_write", "websearch", "ask_user_question", "exit_plan_mode"}
	childIDs    = []string{"bash", "read", "glob", "grep", "edit", "write"}
	childWebIDs = []string{"bash", "read", "glob", "grep", "edit", "write", "webfetch", "websearch"}
)

// webModes are the states a test runs a session under, by name: the three
// modes, an unknown one (withheld), and on with no service.
func webModes(svc tool.Web) map[string]WebState {
	return map[string]WebState{
		"withheld":   {Service: svc},
		"on":         {Mode: WebOn, Service: svc},
		"asking":     {Mode: WebAsking, Service: svc},
		"unknown":    {Mode: WebMode(9), Service: svc},
		"on, no web": {Mode: WebOn},
	}
}

// offersWeb is whether the named state offers the web tools to a session,
// and to its children.
func offersWeb(mode string) (session, child bool) {
	switch mode {
	case "on", "on, no web":
		return true, true
	case "asking":
		return true, false
	}
	return false, false
}

// webCall is a step that calls websearch with query and fetch with url.
func webCall(t *testing.T, id, query, url string) step {
	t.Helper()
	return callStep(
		callParts(id+"s", tool.WebSearchTool, input(t, map[string]any{"query": query})),
		callParts(id+"f", tool.WebFetchTool, input(t, map[string]any{"url": url})))
}

// TestWebStateOffersTheTools: on and asking offer both tools, each at its
// place, on the wire and in the dispatcher, which runs them against the
// state's service (Env.Web); withheld — and a mode the harness does not know
// — offers neither, and a call to one is a call to a tool the session does
// not have. On with no service offers them, and each answers a tool_error.
func TestWebStateOffersTheTools(t *testing.T) {
	for mode, state := range webModes(nil) {
		t.Run(mode, func(t *testing.T) {
			svc := &fakeWebService{}
			if mode != "on, no web" {
				state.Service = svc
			}
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Web = state
			s := f.open(opts)
			offered, _ := offersWeb(mode)
			want := baseIDs
			if offered {
				want = withWebIDs
			}
			equal(t, "the tools", specIDs(s), want)

			a := f.models["test/a"]
			a.push(webCall(t, "c1", "golang release", "https://go.dev/"), answerWith("done"))
			run(t, s, "look it up")
			calls := a.requests()
			equal(t, "the tools on the wire", wireNames(calls[0]), want)
			history := strings.Join(promptOf(calls[1]), "\n")
			switch {
			case !offered:
				if !strings.Contains(history, "tool not found: websearch") || !strings.Contains(history, "tool not found: webfetch") || len(svc.asked()) != 0 {
					t.Fatalf("a withheld session ran a web tool (service asked %q):\n%s", svc.asked(), history)
				}
			case mode == "on, no web":
				if strings.Count(history, "Web search is not available in this session") != 2 {
					t.Fatalf("with no service the calls did not answer a tool_error:\n%s", history)
				}
			default:
				if !strings.Contains(history, "result for golang release") || !strings.Contains(history, "page at https://go.dev/") ||
					!slices.Equal(svc.asked(), []string{"golang release", "https://go.dev/"}) {
					t.Fatalf("the calls did not reach the state's service (asked %q):\n%s", svc.asked(), history)
				}
			}
		})
	}
}

// agentText is the description of s's agent tool as it is offered.
func agentText(t *testing.T, s *Session) string {
	t.Helper()
	sp, ok := s.tools.byID[tool.AgentTool]
	if !ok {
		t.Fatal("the session has no agent tool")
	}
	return sp.Description
}

// TestWebAgentTypesText: the agent tool's text lists the web tools among a
// type's tools only in a session that opened on — the one whose children are
// given them. An asking session's text is a withheld one's, byte for byte, as
// is the list its children's tools are drawn from (toolset.offered).
func TestWebAgentTypesText(t *testing.T) {
	texts, offered := map[string]string{}, map[string][]string{}
	for mode, state := range webModes(&fakeWebService{}) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		opts := f.options()
		opts.Web = state
		s := f.open(opts)
		texts[mode], offered[mode] = agentText(t, s), s.tools.offered
	}
	if texts["asking"] != texts["withheld"] || texts["unknown"] != texts["withheld"] {
		t.Fatalf("an asking session's agent text differs from a withheld one's:\n%s\n---\n%s", texts["asking"], texts["withheld"])
	}
	equal(t, "an asking session's child tools", offered["asking"], offered["withheld"])
	equal(t, "a withheld session's child tools", offered["withheld"], childIDs)
	equal(t, "an on session's child tools", offered["on"], childWebIDs)
	for _, mode := range []string{"withheld", "asking"} {
		if strings.Contains(texts[mode], "websearch") || strings.Contains(texts[mode], "webfetch") {
			t.Fatalf("a %s session's agent text names a web tool:\n%s", mode, texts[mode])
		}
	}
	const all = "(tools: bash, read, glob, grep, edit, write, webfetch, websearch)"
	if !strings.Contains(texts["on"], "- general-purpose: ") || !strings.Contains(texts["on"], all) {
		t.Fatalf("an on session's general-purpose type does not list the web tools:\n%s", texts["on"])
	}
	// Only the type that has every tool gains them: the others list the
	// tools they name, which no web tool is.
	if strings.Replace(texts["on"], all, "(tools: bash, read, glob, grep, edit, write)", 1) != texts["withheld"] {
		t.Fatalf("an on session's agent text differs from a withheld one's beyond general-purpose's tools:\n%s\n---\n%s", texts["on"], texts["withheld"])
	}
}

// TestWebChildren (§C.3, "Children"): a sub-agent the runner opens is offered
// the web tools only when its parent opened on, and then reaches the web
// through the parent's own service; a child of an asking parent (it has
// nobody to ask) or of a withheld one has neither tool, on the wire or in
// its dispatcher.
func TestWebChildren(t *testing.T) {
	for mode, state := range webModes(nil) {
		if mode == "on, no web" {
			continue
		}
		t.Run(mode, func(t *testing.T) {
			svc := &fakeWebService{}
			state.Service = svc
			f := newRouted(t)
			opts := f.options()
			opts.Web = state
			s := f.open(opts)
			k := watchKids(s)
			a := f.routers["test/a"]
			a.route("fan out", callStep(agentPart(t, "a1", task("look", "child one", "subagent_type", "general-purpose"))), answerWith("done"))
			a.route("child one", webCall(t, "c1", "child query", "https://example.com/child"), answerWith("child done"))
			run(t, s, "fan out")
			if n := len(k.all()); n != 1 {
				t.Fatalf("%d children, want 1", n)
			}
			_, toChild := offersWeb(mode)
			want := childIDs
			if toChild {
				want = childWebIDs
			}
			child := k.all()[0]
			equal(t, "the child's tools", specIDs(child), want)
			calls := a.requests("child one")
			equal(t, "the child's tools on the wire", wireNames(calls[0]), want)
			history := strings.Join(promptOf(calls[1]), "\n")
			if toChild {
				if !slices.Equal(svc.asked(), []string{"child query", "https://example.com/child"}) || !strings.Contains(history, "result for child query") {
					t.Fatalf("the child's calls did not reach the parent's service (asked %q):\n%s", svc.asked(), history)
				}
				return
			}
			if len(svc.asked()) != 0 || !strings.Contains(history, "tool not found: websearch") {
				t.Fatalf("a child of a %s parent ran a web tool (service asked %q):\n%s", mode, svc.asked(), history)
			}
		})
	}
}

// TestWebChildOptions: what childOpenOptions hands a child — the parent's
// state, sharing its service, when the parent opened on, and the zero state
// otherwise — and a child opened with any other state is held to the same
// rule by Open.
func TestWebChildOptions(t *testing.T) {
	svc := &fakeWebService{}
	for mode, state := range webModes(svc) {
		s := &Session{base: childBase{web: state}}
		got := s.childOpenOptions(tableView{}, "", "", &ChildOptions{}).Web
		_, toChild := offersWeb(mode)
		switch {
		case toChild && (got.Mode != WebOn || got.Service != state.Service):
			t.Errorf("%s: a child's Options.Web = %+v; want the parent's %+v", mode, got, state)
		case !toChild && got != (WebState{}):
			t.Errorf("%s: a child's Options.Web = %+v; want withheld", mode, got)
		}
	}

	f := newFixture(t, "http://127.0.0.1:1/v1")
	parent := f.open(f.options())
	for mode, state := range webModes(svc) {
		opts := childOf(f, parent, ChildOptions{ID: "child-" + strings.ReplaceAll(strings.ReplaceAll(mode, " ", ""), ",", ""),
			AllTools: true, Mode: "agent"})
		opts.Web = state
		child := f.open(opts)
		_, toChild := offersWeb(mode)
		want := childIDs
		if toChild {
			want = childWebIDs
		}
		equal(t, mode+": a child opened by hand", specIDs(child), want)
	}
}

// TestWebModelSwitch: a switch to another model of the profile keeps the
// session's tools as it opened with them — the web tools withheld, or
// offered — on the wire after the switch, and check judges the switch
// against the profile as filtered (profileFor), whose registry has the web
// tools either way.
func TestWebModelSwitch(t *testing.T) {
	for _, mode := range []string{"withheld", "on", "asking"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Web = webModes(&fakeWebService{})[mode]
			s := f.open(opts)
			offered, _ := offersWeb(mode)
			want := baseIDs
			if offered {
				want = withWebIDs
			}
			if err := s.SetModel("test/b"); err != nil {
				t.Fatalf("SetModel: %v", err)
			}
			f.models["test/b"].push(answerWith("hi"))
			run(t, s, "hello")
			equal(t, "the tools on the wire after the switch", wireNames(f.models["test/b"].requests()[0]), want)

			p, err := s.tools.profileFor(profileRef(""))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, tl := range p.Tools {
				ids = append(ids, tl.Spec().ID)
			}
			if got := slices.ContainsFunc(ids, tool.IsWebTool); got != offered {
				t.Fatalf("the profile a switch is judged against has the web tools: %v; want %v (%v)", got, offered, ids)
			}
			all, err := s.tools.registry.ProfileFor(profileRef(""))
			if err != nil {
				t.Fatal(err)
			}
			if len(all.Tools) != 15 {
				t.Fatalf("control: the registry's profile has %d tools; want the fifteen", len(all.Tools))
			}
		})
	}
}

// TestWebResume: a resumed session's tools are rebuilt from the web state it
// reopens with, as a new session's are: the zero state withholds them, on
// offers them.
func TestWebResume(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	id := storedTurn(t, f, f.options(), nil)
	for mode, want := range map[string][]string{"withheld": baseIDs, "on": withWebIDs} {
		opts := resumeOptions(f.options(), id)
		opts.Web = webModes(&fakeWebService{})[mode]
		s := f.open(opts)
		equal(t, mode+": a resumed session's tools", specIDs(s), want)
		_ = s.Close()
	}
}

// TestWebOnIsNotTheBase is TestWithheldWebIsTheBase's control: the same
// three sessions opened on are not the base, so the identity check can see
// the web tools when they are there.
func TestWebOnIsNotTheBase(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	sessions := openBaseSessions(t, f, func(o *Options) { o.Web = WebState{Mode: WebOn} })
	for _, b := range baseTools {
		want, err := os.ReadFile(filepath.Join("testdata", "base_tools", b.name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := hostNeutral(t, sessions[b.name].tools.wire)
		if bytes.Equal(got, want) {
			t.Fatalf("%s: a session offered the web tools offers the base's tools", b.name)
		}
	}
}

// toolsText is a tools array as the harness's tools goldens hold it: head,
// then each tool's name, description and parameters, indented, with bash's
// description host-neutral (hostNeutral).
func toolsText(t *testing.T, head string, wire []byte) []byte {
	t.Helper()
	neutral, _ := hostNeutral(t, wire)
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(neutral, &tools); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.WriteString(head)
	for _, tl := range tools {
		var params bytes.Buffer
		if err := json.Indent(&params, tl.Parameters, "", "  "); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "\n=== tool: %s\n--- description\n%s\n--- parameters\n%s\n", tl.Name, tl.Description, params.String())
	}
	return b.Bytes()
}

// checkToolsGolden compares got with testdata/name, which -update rewrites.
func checkToolsGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/harness -run %s -update)", err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the tools differ from %s (regenerate with -update if the change is deliberate)\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// TestToolsWithWebGolden pins what a session offered the web tools sends —
// a top-level session that runs background jobs, and a child of one that
// opened on — with bash's machine written as placeholders, and each array's
// tools_sha256 on the pinned host. Regenerate with:
//
//	go test ./internal/harness -run TestToolsWithWebGolden -update
func TestToolsWithWebGolden(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	on := WebState{Mode: WebOn, Service: &fakeWebService{}}
	opts := f.options()
	opts.Background = true
	opts.Web = on
	top := f.open(opts)
	parent := f.open(func() Options { o := f.options(); o.Web = on; return o }())
	copts := childOf(f, parent, ChildOptions{ID: "child-0001", ParentCall: "t1.1.1", Type: "general-purpose", Role: "r", AllTools: true, Mode: "agent"})
	copts.Web = parent.base.web
	child := f.open(copts)

	for name, s := range map[string]*Session{"tools_with_web.golden": top, "tools_with_web_child.golden": child} {
		neutral, _ := hostNeutral(t, s.tools.wire)
		head := fmt.Sprintf("# The tools a session offered the web tools sends (toolset.wire), bash's machine as placeholders.\n# tools_sha256 on linux, bash, /tmp/craze: %s\n", hexSHA(onHost(neutral, pinnedHost)))
		checkToolsGolden(t, name, toolsText(t, head, s.tools.wire))
	}
	equal(t, "the top-level session's tools", specIDs(top), []string{"bash", "bash_output", "bash_stop", "read", "glob", "grep", "edit", "write",
		"agent", "agent_output", "webfetch", "todo_write", "websearch", "ask_user_question", "exit_plan_mode"})
	equal(t, "the child's tools", specIDs(child), childWebIDs)
}
