package agent

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kaptinlin/jsonschema"

	"github.com/charliek/craze/internal/protocol"
)

// The lossless event codec's twin of internal/protocol's
// TestSchemaCoversEveryWireField (plan 027 §3.11, CodeRabbit 19): the
// codec's wire structs are unexported, so the check that the published
// event.json describes them — every field, both ways — lives here, beside
// them. A field added to wireEvent or any struct it reaches fails the gate
// until event.json has it, and a property event.json gains fails it until the
// codec writes it.

// eventSchema is event.json's root, compiled with every $ref resolved from
// the embedded schema set.
func eventSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.RegisterLoader("https", protocol.SchemaLoader)
	c.RegisterLoader("http", protocol.SchemaLoader)
	s, err := c.Schema(protocol.SchemaURI(protocol.SchemaEvent))
	if err != nil {
		t.Fatalf("compile event.json: %v", err)
	}
	if u := s.UnresolvedReferenceURIs(); len(u) > 0 {
		t.Fatalf("event.json leaves $refs unresolved: %v", u)
	}
	return s
}

// validEvent fails the test unless body validates against event.json.
func validEvent(t *testing.T, s *jsonschema.Schema, what, body string) {
	t.Helper()
	if r := s.Validate([]byte(body)); !r.IsValid() {
		t.Errorf("%s does not validate against event.json: %v\nbody: %s", what, r.Errors, body)
	}
}

// TestEventSchemaCoversTheCodec: wireEvent and every wire struct it reaches
// against event.json, both ways, every $def of event.json describing
// something the codec writes; and every event of the codec's own corpora —
// the emit-site corpus, the pinned wire shapes, and the completeness
// filler's passes, which set every field there is — encoded with EncodeEvent,
// validates against it.
func TestEventSchemaCoversTheCodec(t *testing.T) {
	if EventCodecVersion != 1 {
		t.Fatalf("event.json describes event codec version 1, and the codec is version %d: publish its schema", EventCodecVersion)
	}
	c, err := protocol.NewCoverage(protocol.CoverageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(protocol.SchemaEvent+"#", reflect.TypeFor[wireEvent]())
	for _, p := range c.Problems() {
		t.Error(p)
	}
	for _, def := range c.Unvisited(protocol.SchemaEvent) {
		t.Errorf("%s describes nothing the codec writes", def)
	}

	s := eventSchema(t)
	for _, tc := range emitSiteEvents() {
		body, err := EncodeEvent(tc.ev)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		validEvent(t, s, tc.name, body)
	}
	for _, tc := range pinnedWireShapes() {
		validEvent(t, s, "the pinned "+string(tc.ev.Type)+" body", tc.want)
	}
	// The filler's passes set every field reachable from an Event, so every
	// property the codec can write is written at least once. The filler draws
	// every string, the type included, so the type is set to one there is.
	filled, bools := filledEvent(t, false, func(int) bool { return true })
	passes := map[string]Event{"the distinct pass": filled}
	for _, on := range bools {
		ev, _ := filledEvent(t, false, func(n int) bool { return n == on })
		passes[fmt.Sprintf("the one-hot pass for the bool drawn %d", on)] = ev
	}
	zero, _ := filledEvent(t, true, nil)
	passes["the zero pass"] = zero
	for what, ev := range passes {
		ev.Type = EventTool
		body, err := EncodeEvent(ev)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		validEvent(t, s, what, body)
	}
}

// constsOfType is every constant this package's non-test files declare with
// the named type, by value: what an enum on the wire must hold.
func constsOfType(t *testing.T, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != typeName {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("a %s constant that is not a string literal: %T", typeName, v)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// schemaEnum is the enum at pointer in one schema file, sorted.
func schemaEnum(t *testing.T, file string, path ...string) []string {
	t.Helper()
	b, err := protocol.SchemaFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, p := range path {
		v = v.(map[string]any)[p]
	}
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	sort.Strings(out)
	return out
}

// TestEveryEventTypeIsOnTheWire: event.json's type enum is exactly the
// EventType constants this package declares — a kind added to the codec is a
// kind the schema has.
func TestEveryEventTypeIsOnTheWire(t *testing.T) {
	got := schemaEnum(t, protocol.SchemaEvent, "properties", "type", "enum")
	if want := constsOfType(t, "EventType"); !slices.Equal(got, want) {
		t.Fatalf("event.json's type enum\n got %v\nwant %v", got, want)
	}
}

// TestEveryCursorReasonIsOnTheWire: the attach reply's reset enum
// (protocol.CursorReasons, and session.attach.json's cursorReason) is exactly
// the CursorReason constants the event log declares.
func TestEveryCursorReasonIsOnTheWire(t *testing.T) {
	want := constsOfType(t, "CursorReason")
	var got []string
	for _, r := range protocol.CursorReasons() {
		got = append(got, string(r))
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("protocol.CursorReasons\n got %v\nwant %v", got, want)
	}
	if s := schemaEnum(t, protocol.MethodSchema(protocol.MethodSessionAttach), "$defs", "cursorReason", "enum"); !slices.Equal(s, want) {
		t.Fatalf("session.attach.json's cursorReason\n got %v\nwant %v", s, want)
	}
}

// TestAnEventBodyCrossesTheWireVerbatim (§3.3): an event notification
// carries EncodeEvent's body byte for byte — HTML characters, a line
// separator, non-ASCII text — through the protocol's line writer.
func TestAnEventBodyCrossesTheWireVerbatim(t *testing.T) {
	body, err := EncodeEvent(Event{Type: EventText, Text: "a && b <c>   é 😀", At: codecTestTime})
	if err != nil {
		t.Fatal(err)
	}
	line, err := protocol.MarshalLine(protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: protocol.NotifyEvent,
		Params: wireParams(t, protocol.EventParams{Subscription: "s-1", Seq: 4, Event: json.RawMessage(body)})})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `"event":`+body+"}") {
		t.Fatalf("the body did not cross verbatim:\nbody %s\nline %s", body, line)
	}
	var back struct {
		Params struct {
			Event json.RawMessage `json:"event"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Params.Event) != body {
		t.Fatalf("read back %s, want %s", back.Params.Event, body)
	}
}

func wireParams(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := protocol.MarshalLine(v)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(strings.TrimSuffix(string(b), "\n"))
}
