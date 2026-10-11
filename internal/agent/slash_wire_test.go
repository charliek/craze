package agent

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// The slash catalog on the wire (plan 043 C2): the codec's own corpora carry
// it (emitSiteEvents, pinnedWireShapes, the completeness filler and the leaf
// wrappers), and these hold the rest — the section absent when nil, values
// this build does not know kept as they came, and the two sessions craze runs
// saying they publish it.

// slashCodecCommands, slashCodecPlugins and slashCodecCatalog are a grok-like
// session's catalog inputs and the catalog BuildSlashCatalog makes of them:
// modes agent and plan advertised and ask not (so the ask row is unavailable),
// a plugin row whose bare name is free and one qualified because the agent
// took it, and a disk skill — the emit-site corpus's slash deltas.
func slashCodecCommands() []CommandInfo {
	return []CommandInfo{{Name: "review", Description: "the agent's\nown review"}}
}

func slashCodecPlugins() []PluginCommand {
	return []PluginCommand{
		{Plugin: "pr-tools", Bare: "ship", Display: "ship", Qualified: "pr-tools:ship", Description: "ship <it> & go", Kind: "command"},
		{Plugin: "pr-tools", Bare: "review", Display: "pr-tools:review", Qualified: "pr-tools:review", Description: "a plugin's review", Kind: "skill"},
	}
}

func slashCodecCatalog() SlashCatalog {
	return BuildSlashCatalog(SlashInputs{
		Provider:     "grok",
		Capabilities: Capabilities{Modes: true, Todos: true},
		Modes:        []string{"agent", "plan"},
		Commands:     slashCodecCommands(),
		Plugins:      slashCodecPlugins(),
		Skills:       []Skill{{Name: "deploy", Description: "deploy the build"}},
	})
}

// TestSlashIsAbsentOnTheWireWhenNil (plan 043 P6): a delta that did not touch
// the catalog carries no "slash" key at all — never null and never {} — which
// is every delta a session that does not publish one sends, and every delta
// of one that does but a change of the catalog's own inputs, so no fixture
// recorded before the section existed moves; the leaf wrappers agree. A
// present and empty section is {}, the catalog cleared, and decodes as a
// non-nil empty one. Negative control: wireState.Slash without omitempty
// writes "slash":null into the first body.
func TestSlashIsAbsentOnTheWireWhenNil(t *testing.T) {
	body, err := EncodeEvent(Event{Type: EventMeta, State: &StateDelta{
		Model: ptr("grok"), Commands: &CommandsState{Commands: []CommandInfo{{Name: "research"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"slash"`) {
		t.Fatalf("a delta with no slash section writes one: %s", body)
	}
	if raw, err := EncodeSlashCatalog(nil); raw != nil || err != nil {
		t.Fatalf("EncodeSlashCatalog(nil) = %q, %v; want no bytes at all", raw, err)
	}
	cleared, err := EncodeEvent(Event{Type: EventMeta, State: &StateDelta{Slash: &SlashCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	if cleared != `{"type":"meta","state":{"slash":{}}}` {
		t.Fatalf("a cleared catalog is %s", cleared)
	}
	back, err := DecodeEvent(cleared)
	if err != nil || back.State == nil || back.State.Slash == nil || back.State.Slash.Items != nil {
		t.Fatalf("a cleared catalog decodes as %+v, %v; want a present section with no rows", back.State, err)
	}
}

// TestASlashSectionKeepsWhatThisBuildDoesNotKnow (plan 043 P17): the values of
// a row's source, requires and every run key are open — a later host may add
// one, and a client hides a row with a value it does not know — so the decoder
// keeps each as it came and never refuses the section for one: a row with an
// unknown run kind, one with an unknown source, method, requires, local
// action, picker and arg, decode verbatim beside the rows this build knows,
// and the body re-encodes byte for byte. event.json says the same: the body
// validates, its values being plain strings, not a closed list. A key a later
// build adds to a row is ignored, as every key a decoder does not know is.
// Negative controls: a decoder that dropped a row whose kind it did not know
// fails the row count; an enum on slashRun.kind in event.json fails the
// validation.
func TestASlashSectionKeepsWhatThisBuildDoesNotKnow(t *testing.T) {
	known := slashCodecCatalog().Items
	unknown := []SlashItem{
		{Name: "teleport", Description: "a run kind of a later build", Source: SlashSourceBuiltin,
			Run: SlashRun{Kind: "teleport", Arg: SlashArgOptional}},
		{Name: "fork", Description: "every other open value", Source: "workflow", Requires: "fork",
			Run: SlashRun{Kind: SlashRunMethod, Method: "session.fork", Param: "name", Arg: "multiline", Picker: "branch", Local: "split"}},
	}
	items := append(append(append([]SlashItem(nil), known[:2]...), unknown...), known[2:]...)
	body, err := EncodeEvent(Event{Type: EventMeta, State: &StateDelta{Slash: &SlashCatalog{Items: items}}})
	if err != nil {
		t.Fatal(err)
	}
	validEvent(t, eventSchema(t), "a slash section with values this build does not know", body)
	ev, err := DecodeEvent(body)
	if err != nil {
		t.Fatalf("the section was refused: %v", err)
	}
	if ev.State == nil || ev.State.Slash == nil || !reflect.DeepEqual(ev.State.Slash.Items, items) {
		t.Fatalf("decoded %+v\nwant %+v", ev.State, items)
	}
	again, err := EncodeEvent(ev)
	if err != nil || again != body {
		t.Fatalf("re-encoded:\n got %s\nwant %s (%v)", again, body, err)
	}

	// A key of a later build, on a row and on its run, is skipped and the row
	// kept.
	later := strings.Replace(body, `"name":"teleport",`, `"name":"teleport","icon":"rocket",`, 1)
	later = strings.Replace(later, `"run":{"kind":"teleport",`, `"run":{"kind":"teleport","where":"mars",`, 1)
	if later == body {
		t.Fatal("fixture: the body has no teleport row to add keys to")
	}
	ev, err = DecodeEvent(later)
	if err != nil || ev.State == nil || ev.State.Slash == nil || !reflect.DeepEqual(ev.State.Slash.Items, items) {
		t.Fatalf("keys of a later build: %+v, %v", ev.State, err)
	}
	var raw struct {
		State struct {
			Slash struct {
				Items []json.RawMessage `json:"items"`
			} `json:"slash"`
		} `json:"state"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil || len(raw.State.Slash.Items) != len(items) {
		t.Fatalf("the body carries %d rows, want %d (%v)", len(raw.State.Slash.Items), len(items), err)
	}
}

// TestLiveAndNativeSessionsPublishTheSlashCatalog (plan 043 P2): the session
// capability slashCatalog is the session's own fact, and both sessions craze
// runs say it — a live ACP session of every provider, and a native one —
// before Start as after, since each installs a catalog at Start. (The engine
// asks through agent.SlashCataloger, engine.PublishesSlashCatalog, and the
// server puts it in the info document; internal/control and internal/fakehost
// hold those, the Stub saying no unless opted in.) Negative control: the
// native session's PublishesSlashCatalog answering false fails the native
// case.
func TestLiveAndNativeSessionsPublishTheSlashCatalog(t *testing.T) {
	for _, p := range Providers() {
		if p.InProcess() {
			continue
		}
		s := newTestSession(t, Options{Binary: fakeAgentPath(t), Provider: &p, Workspace: t.TempDir(), Stderr: io.Discard})
		if c, ok := Session(s).(SlashCataloger); !ok || !c.PublishesSlashCatalog() {
			t.Fatalf("a %s live session does not say it publishes the slash catalog", p.Name())
		}
	}
	f := newNativeFixture(t)
	if c, ok := Session(f.session(Options{})).(SlashCataloger); !ok || !c.PublishesSlashCatalog() {
		t.Fatal("a native session does not say it publishes the slash catalog")
	}
}

// TestAMalformedSlashRowIsDroppedAlone (plan 043 P17, review W9): one row a
// decoder cannot read never costs the section, the event or a client's
// stream. A section of good rows around bad ones — a name that is a number,
// an empty or blank name or one with a space in it, a row that is a string
// or null, a key of the wrong type, a run that is not an object — decodes as
// the good rows alone, in order, and the rest of the delta (its commands,
// its cause) as it came; a slash or items value that is not an object or an
// array is a section with no rows, present, so it still replaces; null is no
// section at all. The leaf wrapper the snapshot uses reads the same way, and
// still refuses bytes that are not JSON. Negative control: a wireSlashIn
// without its UnmarshalJSON (the typed list) fails the first body's decode.
func TestAMalformedSlashRowIsDroppedAlone(t *testing.T) {
	good := slashCodecCatalog().Items[:3]
	enc := func(it SlashItem) string {
		b, err := json.Marshal(toWireSlash(&SlashCatalog{Items: []SlashItem{it}}).Items[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	section := func(rows ...string) string { return `{"items":[` + strings.Join(rows, ",") + `]}` }
	body := func(slash string) string {
		return `{"type":"meta","state":{"commands":{"commands":[{"name":"review"}]},"slash":` + slash + `},"cause":"c-1/2"}`
	}
	for _, tc := range []struct {
		name  string
		slash string
		want  []SlashItem // nil: present with no rows
	}{
		{"a name that is a number", section(enc(good[0]), `{"name":42}`, enc(good[1])), good[:2]},
		{"every kind of bad row", section(`"a string"`, enc(good[0]), `null`, `{"name":""}`, `{"name":"   "}`,
			`{"name":"two words"}`, `{"name":"ok","whileWorking":"yes"}`, enc(good[1]), `{"name":"ok","run":5}`,
			`{"name":"ok","run":{"kind":7}}`, `[]`, enc(good[2])), good},
		{"a section that is a number", `5`, nil},
		{"a section that is a list", `[` + enc(good[0]) + `]`, nil},
		{"items that are an object", `{"items":{"name":"help"}}`, nil},
		{"items that are null", `{"items":null}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := DecodeEvent(body(tc.slash))
			if err != nil {
				t.Fatalf("the event was refused: %v", err)
			}
			st := ev.State
			if ev.Cause != "c-1/2" || st == nil || st.Commands == nil || len(st.Commands.Commands) != 1 {
				t.Fatalf("the rest of the event did not survive: %+v", ev)
			}
			if st.Slash == nil || !reflect.DeepEqual(st.Slash.Items, tc.want) {
				t.Fatalf("the section decoded as %+v, want present with %+v", st.Slash, tc.want)
			}
			leaf, err := DecodeSlashCatalog(json.RawMessage(tc.slash))
			if err != nil || leaf == nil || !reflect.DeepEqual(leaf.Items, tc.want) {
				t.Fatalf("the leaf wrapper read %+v, %v; want %+v", leaf, err, tc.want)
			}
		})
	}
	ev, err := DecodeEvent(body(`null`))
	if err != nil || ev.State == nil || ev.State.Slash != nil || ev.State.Commands == nil {
		t.Fatalf("a null section: %+v, %v; want no section and the commands", ev.State, err)
	}
	if _, err := DecodeSlashCatalog(json.RawMessage(`{"items":[`)); err == nil {
		t.Fatal("the leaf wrapper read bytes that are not JSON")
	}
}
