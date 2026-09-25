package protocol_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kaptinlin/jsonschema"

	"github.com/charliek/craze/internal/protocol"
)

// oneOf is a result that is one of several documents, each its own Go type,
// in the order of its schema's oneOf branches (hello's, plan 027 X5).
type oneOf []any

// methodTypes is every method's params and result Go type: what the schema's
// $defs params and result describe. A nil result is a method whose result
// protocol 1 does not specify (sessions.subscribe, the hub's); session.create,
// reserved, has no types at all.
var methodTypes = map[string]struct{ params, result any }{
	protocol.MethodHello:             {protocol.HelloParams{}, oneOf{protocol.HelloResult{}, protocol.HubHelloResult{}}},
	protocol.MethodSessionsList:      {protocol.SessionsListParams{}, protocol.SessionsListResult{}},
	protocol.MethodSessionsSubscribe: {protocol.SessionsSubscribeParams{}, nil},
	protocol.MethodSessionConnect:    {protocol.ConnectParams{}, protocol.Empty{}},
	protocol.MethodSessionAttach:     {protocol.AttachParams{}, protocol.AttachResult{}},
	protocol.MethodSessionDetach:     {protocol.DetachParams{}, protocol.Empty{}},
	protocol.MethodSessionState:      {protocol.StateParams{}, protocol.StateResult{}},
	protocol.MethodSessionSnapshot:   {protocol.SnapshotParams{}, protocol.SnapshotResult{}},
	protocol.MethodSessionSync:       {protocol.SyncParams{}, protocol.SyncResult{}},
	protocol.MethodSessionPrompt:     {protocol.PromptParams{}, protocol.PromptResult{}},
	protocol.MethodSessionCancel:     {protocol.CancelParams{}, protocol.CancelResult{}},
	protocol.MethodSessionDisarm:     {protocol.DisarmParams{}, protocol.Empty{}},
	protocol.MethodQueueAdd:          {protocol.QueueAddParams{}, protocol.QueueAddResult{}},
	protocol.MethodQueueEdit:         {protocol.QueueEditParams{}, protocol.Empty{}},
	protocol.MethodQueueRemove:       {protocol.QueueRemoveParams{}, protocol.QueueRemoveResult{}},
	protocol.MethodQueueClear:        {protocol.QueueClearParams{}, protocol.QueueClearResult{}},
	protocol.MethodSessionSet:        {protocol.SetParams{}, protocol.SetResult{}},
	protocol.MethodSessionSetTitle:   {protocol.SetTitleParams{}, protocol.Empty{}},
	protocol.MethodSubagentCancel:    {protocol.SubagentCancelParams{}, protocol.Empty{}},
	protocol.MethodSessionStop:       {protocol.StopParams{}, protocol.Empty{}},
	protocol.MethodAsksList:          {protocol.AsksListParams{}, protocol.AsksListResult{}},
	protocol.MethodAsksGet:           {protocol.AsksGetParams{}, protocol.AsksGetResult{}},
	protocol.MethodAsksAnswer:        {protocol.AsksAnswerParams{}, protocol.Empty{}},
}

// errorResultTypes is the data.result a method's refusal carries beside it,
// for the methods that have one (§3.2): each file's $defs errorResult.
var errorResultTypes = map[string]any{
	protocol.MethodHello:         protocol.HelloErrorResult{},
	protocol.MethodSessionCancel: protocol.CancelResult{},
}

// notificationTypes is every notification's params Go type.
var notificationTypes = map[string]any{
	protocol.NotifyEvent:        protocol.EventParams{},
	protocol.NotifySynchronized: protocol.SynchronizedParams{},
	protocol.NotifyReady:        protocol.ReadyParams{},
	protocol.NotifyReset:        protocol.ResetParams{},
}

// envelopeTypes is the envelope's own Go types, by their $defs.
var envelopeTypes = map[string]any{
	"request":      protocol.Request{},
	"response":     protocol.Response{},
	"notification": protocol.Notification{},
	"error":        protocol.Error{},
	"errorData":    protocol.ErrorData{},
}

// protocolRaw accounts for every json.RawMessage of the protocol's own
// types: each payload of another codec is a $ref to that codec's schema
// (doc.go), and the envelope's own raw members are the ids and the payloads
// whose schema is the method's own file.
var protocolRaw = map[string]string{
	"Request.id":                 "envelope.json#/$defs/id",
	"Request.params":             "",
	"Response.id":                "",
	"Response.result":            "",
	"Notification.params":        "",
	"ErrorData.result":           "",
	"HelloParams.auth":           "",
	"HelloParams.via":            "",
	"AttachResult.snapshot":      "snapshot.json#",
	"StateResult.queue[]":        "event.json#/$defs/queued",
	"Settings.config":            "event.json#/$defs/config",
	"SnapshotResult.snapshot":    "snapshot.json#",
	"PromptResult.queued":        "event.json#/$defs/queued",
	"QueueAddResult.row":         "event.json#/$defs/queued",
	"QueueRemoveResult.row":      "event.json#/$defs/queued",
	"QueueClearResult.removed[]": "event.json#/$defs/queued",
	"AskRecord.body":             "event.json#/$defs/askBody",
	"EventParams.event":          "event.json#",
}

// tolerantObjects is every object allowed to accept fields it does not
// define: hello's params and what is inside them (§3.2), and nothing else.
var tolerantObjects = map[string]bool{
	"hello.json#/$defs/params":             true,
	"hello.json#/$defs/client":             true,
	"hello.json#/$defs/clientCapabilities": true,
	"hello.json#/$defs/resume":             true,
}

func protocolCoverage(t *testing.T) *protocol.Coverage {
	t.Helper()
	c, err := protocol.NewCoverage(protocol.CoverageOptions{Raw: protocolRaw, Tolerant: tolerantObjects})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSchemaCoversEveryWireField (plan 027 §3.11, A10) holds every params,
// result, error result, notification and envelope Go type to its schema by
// reflection, both ways: every JSON field is a property at its place, of a
// compatible type, required exactly when it is always written; every property
// is a field; every json.RawMessage is a $ref to the codec that writes it; no
// object but hello's is tolerant; and no $def of the protocol's own files is
// left undescribing anything on the wire. The codecs' own files are the
// twins' (internal/agent, internal/transcript).
func TestSchemaCoversEveryWireField(t *testing.T) {
	c := protocolCoverage(t)
	defined := 0
	for _, m := range protocol.Methods() {
		if m.Reserved {
			continue
		}
		defined++
		types, ok := methodTypes[m.Name]
		if !ok {
			t.Errorf("method %s has no Go types in this test", m.Name)
			continue
		}
		file := protocol.MethodSchema(m.Name)
		c.Check(file+"#/$defs/params", reflect.TypeOf(types.params))
		switch r := types.result.(type) {
		case nil:
		case oneOf:
			var variants []reflect.Type
			for _, v := range r {
				variants = append(variants, reflect.TypeOf(v))
			}
			c.CheckOneOf(file+"#/$defs/result", variants...)
		default:
			c.Check(file+"#/$defs/result", reflect.TypeOf(r))
		}
		if er, ok := errorResultTypes[m.Name]; ok {
			c.Check(file+"#/$defs/errorResult", reflect.TypeOf(er))
		}
	}
	if len(methodTypes) != defined {
		t.Errorf("methodTypes names %d methods, protocol 1 defines %d", len(methodTypes), defined)
	}
	for _, n := range protocol.Notifications() {
		typ, ok := notificationTypes[n]
		if !ok {
			t.Errorf("notification %s has no Go type in this test", n)
			continue
		}
		c.Check(protocol.NotificationSchema(n)+"#/$defs/params", reflect.TypeOf(typ))
	}
	for def, typ := range envelopeTypes {
		c.Check(protocol.SchemaEnvelope+"#/$defs/"+def, reflect.TypeOf(typ))
	}
	for _, p := range c.Problems() {
		t.Error(p)
	}
	// The one $def no Go type stands behind: the hub's roster result, which
	// protocol 1 leaves unspecified and no host ever sends.
	unspecified := map[string]bool{protocol.MethodSchema(protocol.MethodSessionsSubscribe) + "#/$defs/result": true}
	for _, name := range protocol.SchemaNames() {
		if name == protocol.SchemaEvent || name == protocol.SchemaSnapshot {
			continue
		}
		for _, def := range c.Unvisited(name) {
			if !unspecified[def] {
				t.Errorf("%s describes nothing any wire type carries", def)
			}
		}
	}
}

// TestCoverageCatchesWhatItIsFor is the reflection check's own negative
// control, kept: a Go field the schema lacks, a property the Go type lacks, a
// presence the two disagree on, a type they disagree on, an unaccounted
// json.RawMessage and a stale Raw entry each fail it.
func TestCoverageCatchesWhatItIsFor(t *testing.T) {
	type cursorPlus struct {
		Incarnation string `json:"incarnation"`
		Seq         uint64 `json:"seq"`
		Extra       string `json:"extra"`
	}
	type cursorMinus struct {
		Incarnation string `json:"incarnation"`
	}
	type cursorOptional struct {
		Incarnation string `json:"incarnation,omitempty"`
		Seq         uint64 `json:"seq"`
	}
	type cursorWrongType struct {
		Incarnation string `json:"incarnation"`
		Seq         string `json:"seq"`
	}
	type rowRaw struct {
		Row json.RawMessage `json:"row"`
	}
	for _, tc := range []struct {
		name, ref string
		typ       reflect.Type
		raw       map[string]string
		want      string
	}{
		{"a Go field the schema lacks", "session.attach.json#/$defs/cursor", reflect.TypeFor[cursorPlus](), nil, `"extra"`},
		{"a property the Go type lacks", "session.attach.json#/$defs/cursor", reflect.TypeFor[cursorMinus](), nil, `property "seq"`},
		{"a presence the two disagree on", "session.attach.json#/$defs/cursor", reflect.TypeFor[cursorOptional](), nil, "requires it"},
		{"a type the two disagree on", "session.attach.json#/$defs/cursor", reflect.TypeFor[cursorWrongType](), nil, "wants string"},
		{"an unaccounted raw payload", "session.queue.add.json#/$defs/result", reflect.TypeFor[rowRaw](), nil, "does not account for"},
		{"a raw payload with the wrong $ref", "session.queue.add.json#/$defs/result", reflect.TypeFor[rowRaw](),
			map[string]string{"rowRaw.row": "event.json#/$defs/todo"}, "want event.json#/$defs/todo"},
		{"a stale Raw entry", "session.queue.add.json#/$defs/result", reflect.TypeFor[rowRaw](),
			map[string]string{"rowRaw.row": "event.json#/$defs/queued", "rowRaw.gone": ""}, `"rowRaw.gone"`},
		{"a tolerant object nobody allowed", "hello.json#/$defs/resume", reflect.TypeFor[protocol.Resume](), nil, "allowed to be tolerant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := protocol.NewCoverage(protocol.CoverageOptions{Raw: tc.raw})
			if err != nil {
				t.Fatal(err)
			}
			c.Check(tc.ref, tc.typ)
			problems := strings.Join(c.Problems(), "\n")
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("want a problem mentioning %s; got:\n%s", tc.want, problems)
			}
		})
	}

	// CheckOneOf (X5): each variant is held to its own branch, strictly.
	type hostOptionalResumed struct {
		protocol.HubHelloResult
		ClientID     string                `json:"clientId"`
		Token        string                `json:"token"`
		Resumed      bool                  `json:"resumed,omitempty"`
		RetryHorizon protocol.RetryHorizon `json:"retryHorizon"`
	}
	type hubWithClient struct {
		protocol.HubHelloResult
		ClientID string `json:"clientId"`
	}
	for _, tc := range []struct {
		name     string
		variants []reflect.Type
		want     string
	}{
		{"a variant missing", []reflect.Type{reflect.TypeFor[protocol.HelloResult]()}, "a oneOf of 2 branches, and 1 Go types"},
		{"a host that may leave out resumed", []reflect.Type{reflect.TypeFor[hostOptionalResumed](), reflect.TypeFor[protocol.HubHelloResult]()},
			"resumed: omitted at its zero value"},
		{"a hub that carries a client id", []reflect.Type{reflect.TypeFor[protocol.HelloResult](), reflect.TypeFor[hubWithClient]()},
			`"clientId"`},
		{"the variants swapped", []reflect.Type{reflect.TypeFor[protocol.HubHelloResult](), reflect.TypeFor[protocol.HelloResult]()},
			`property "clientId"`},
	} {
		t.Run("CheckOneOf: "+tc.name, func(t *testing.T) {
			c, err := protocol.NewCoverage(protocol.CoverageOptions{})
			if err != nil {
				t.Fatal(err)
			}
			c.CheckOneOf("hello.json#/$defs/result", tc.variants...)
			problems := strings.Join(c.Problems(), "\n")
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("want a problem mentioning %s; got:\n%s", tc.want, problems)
			}
		})
	}
}

// newCompiler is a validator that resolves every $ref from the embedded
// schema and never from the network.
func newCompiler() *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.RegisterLoader("https", protocol.SchemaLoader)
	c.RegisterLoader("http", protocol.SchemaLoader)
	return c
}

// compiled is the schema at uri, failing the test if it does not compile or
// leaves a $ref unresolved (the validator would pass an unresolved one
// silently).
func compiled(t *testing.T, c *jsonschema.Compiler, uri string) *jsonschema.Schema {
	t.Helper()
	s, err := c.Schema(uri)
	if err != nil {
		t.Fatalf("compile %s: %v", uri, err)
	}
	if s == nil {
		t.Fatalf("compile %s: nothing there", uri)
	}
	if u := s.UnresolvedReferenceURIs(); len(u) > 0 {
		t.Fatalf("%s leaves $refs unresolved: %v", uri, u)
	}
	return s
}

// TestEverySchemaFileCompiles: every embedded file compiles as a draft
// 2020-12 schema whose $id is its published URI, every $ref in it resolves
// within the embedded set, and so does every $def on its own.
func TestEverySchemaFileCompiles(t *testing.T) {
	c := newCompiler()
	for _, name := range protocol.SchemaNames() {
		b, err := protocol.SchemaFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Schema string                     `json:"$schema"`
			ID     string                     `json:"$id"`
			Defs   map[string]json.RawMessage `json:"$defs"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if doc.Schema != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: $schema %q, want draft 2020-12", name, doc.Schema)
		}
		if doc.ID != protocol.SchemaURI(name) {
			t.Errorf("%s: $id %q, want %q", name, doc.ID, protocol.SchemaURI(name))
		}
		compiled(t, c, protocol.SchemaURI(name))
		for def := range doc.Defs {
			compiled(t, c, protocol.SchemaURI(name, "#/$defs/", def))
		}
	}
}

// TestEveryMethodAndNotificationHasItsFile: one file per method with its
// params and result, one per notification with its params, the four shared
// files, and nothing else; session.create, reserved, has none.
func TestEveryMethodAndNotificationHasItsFile(t *testing.T) {
	want := []string{protocol.SchemaEnvelope, protocol.SchemaEvent, protocol.SchemaSnapshot, protocol.SchemaInfo}
	defsOf := func(name string) map[string]json.RawMessage {
		b, err := protocol.SchemaFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var doc struct {
			Defs map[string]json.RawMessage `json:"$defs"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Defs
	}
	for _, m := range protocol.Methods() {
		if m.Reserved {
			continue
		}
		name := protocol.MethodSchema(m.Name)
		want = append(want, name)
		defs := defsOf(name)
		if defs["params"] == nil || defs["result"] == nil {
			t.Errorf("%s: want $defs params and result", name)
		}
	}
	for _, n := range protocol.Notifications() {
		name := protocol.NotificationSchema(n)
		want = append(want, name)
		if defsOf(name)["params"] == nil {
			t.Errorf("%s: want $defs params", name)
		}
	}
	sort.Strings(want)
	if got := protocol.SchemaNames(); !slices.Equal(got, want) {
		t.Fatalf("schema files\n got %v\nwant %v", got, want)
	}
	// session.create is reserved (X6): named, answered unsupported, reason
	// hub_only, and nothing else of it defined — no schema.
	if m, ok := protocol.Method(protocol.MethodSessionCreate); !ok || !m.Reserved || m.HostUnsupported != protocol.ReasonHubOnly {
		t.Fatalf("session.create is %+v, %v; want reserved, answered hub_only", m, ok)
	}
	if _, err := protocol.SchemaFile(protocol.MethodSchema(protocol.MethodSessionCreate)); err == nil {
		t.Fatal("session.create is reserved and has no schema")
	}
	for _, m := range protocol.Methods() {
		if m.Reserved && m.Name != protocol.MethodSessionCreate {
			t.Errorf("%s is reserved; protocol 1 reserves session.create alone", m.Name)
		}
	}
}

// TestScopedAndMutatingMethodsCarryTheirIds: a session-scoped method's
// params carry sessionId, and only those; a mutating method's carry
// commandId, and only those; both are required, and hello is the one
// tolerant method (§3.3).
func TestScopedAndMutatingMethodsCarryTheirIds(t *testing.T) {
	for _, m := range protocol.Methods() {
		if m.Reserved {
			continue
		}
		typ := reflect.TypeOf(methodTypes[m.Name].params)
		fields := map[string]bool{}
		for i := range typ.NumField() {
			name, opts, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			fields[name] = !strings.Contains(opts, "omit")
		}
		wantScoped := m.Name != protocol.MethodHello && !strings.HasPrefix(m.Name, "sessions.")
		if m.SessionScoped != wantScoped {
			t.Errorf("%s: SessionScoped %v, want %v", m.Name, m.SessionScoped, wantScoped)
		}
		if req, ok := fields["sessionId"]; ok != m.SessionScoped || ok && !req {
			t.Errorf("%s: sessionId field %v (required %v), session-scoped %v", m.Name, ok, req, m.SessionScoped)
		}
		if req, ok := fields["commandId"]; ok != m.Mutating || ok && !req {
			t.Errorf("%s: commandId field %v (required %v), mutating %v", m.Name, ok, req, m.Mutating)
		}
		if m.Tolerant != (m.Name == protocol.MethodHello) {
			t.Errorf("%s: Tolerant %v", m.Name, m.Tolerant)
		}
	}
}

// schemaValue is the value at a JSON pointer of one schema file.
func schemaValue(t *testing.T, name, pointer string) any {
	t.Helper()
	b, err := protocol.SchemaFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch x := v.(type) {
		case map[string]any:
			v = x[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(x) {
				t.Fatalf("%s#%s: no item %q", name, pointer, seg)
			}
			v = x[i]
		}
		if v == nil {
			t.Fatalf("%s#%s: nothing at %q", name, pointer, seg)
		}
	}
	return v
}

// schemaEnum is the enum at a JSON pointer, as strings.
func schemaEnum(t *testing.T, name, pointer string) []string {
	t.Helper()
	var out []string
	for _, e := range schemaValue(t, name, pointer).([]any) {
		switch x := e.(type) {
		case string:
			out = append(out, x)
		case float64:
			b, _ := json.Marshal(x)
			out = append(out, string(b))
		}
	}
	return out
}

func asStrings[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// TestTheSchemaEnumsAreTheGoSets: every enum the schema pins is exactly the
// Go set it describes — codes, reasons (and which code each goes with),
// reset and cursor reasons, activities, when, the prompt modes, the setting
// kinds, the cancel outcomes, the ask statuses, the JSON-RPC integers and the
// endpoint kind — in the same order, so a value added on one side and not
// the other fails here.
func TestTheSchemaEnumsAreTheGoSets(t *testing.T) {
	var hostReasons []string
	byCode := map[string][]string{}
	for _, r := range protocol.Reasons() {
		if r.ClientSide {
			continue
		}
		hostReasons = append(hostReasons, string(r.Reason))
		byCode[string(r.Code)] = append(byCode[string(r.Code)], string(r.Reason))
	}
	for _, tc := range []struct {
		name, file, pointer string
		want                []string
	}{
		{"codes", "envelope.json", "/$defs/code/enum", asStrings(protocol.Codes())},
		{"reasons", "envelope.json", "/$defs/reason/enum", hostReasons},
		{"reset reasons", "notification.reset.json", "/$defs/params/properties/reason/enum", asStrings(protocol.ResetReasons())},
		{"cursor reasons", "session.attach.json", "/$defs/cursorReason/enum", asStrings(protocol.CursorReasons())},
		{"activities", "info.json", "/$defs/activity/enum", asStrings(protocol.Activities())},
		{"when", "session.attach.json", "/$defs/params/properties/when/enum",
			asStrings([]protocol.When{protocol.WhenReady, protocol.WhenNow})},
		{"prompt modes", "session.prompt.json", "/$defs/mode/enum",
			asStrings([]protocol.PromptMode{protocol.PromptQueue, protocol.PromptSendNow, protocol.PromptInterject})},
		{"setting kinds", "session.set.json", "/$defs/setting/properties/kind/enum",
			asStrings([]protocol.SettingKind{protocol.SettingModel, protocol.SettingMode, protocol.SettingConfig})},
		{"cancel outcomes", "session.cancel.json", "/$defs/result/properties/outcome/enum",
			asStrings([]protocol.CancelOutcome{protocol.CancelRequested, protocol.CancelSettled, protocol.CancelUnknown})},
		{"ask statuses", "asks.get.json", "/$defs/record/properties/status/enum",
			asStrings([]protocol.AskStatus{protocol.AskOpen, protocol.AskResolved})},
		{"JSON-RPC integers", "envelope.json", "/$defs/error/properties/code/enum", []string{"-32700", "-32600", "-32601", "-32602", "-32000"}},
	} {
		if got := schemaEnum(t, tc.file, tc.pointer); !slices.Equal(got, tc.want) {
			t.Errorf("%s (%s#%s)\n got %v\nwant %v", tc.name, tc.file, tc.pointer, got, tc.want)
		}
	}
	if got := []int{protocol.RPCParseError, protocol.RPCInvalidRequest, protocol.RPCMethodNotFound, protocol.RPCInvalidParams, protocol.RPCRefused}; !slices.Equal(got, []int{-32700, -32600, -32601, -32602, -32000}) {
		t.Fatalf("the JSON-RPC integers are %v", got)
	}
	// The endpoint kinds (X5): hello's two results are told apart by them, one
	// constant each.
	for def, want := range map[string]string{"hostEndpoint": protocol.EndpointHost, "hubEndpoint": protocol.EndpointHub} {
		if got := schemaValue(t, "hello.json", "/$defs/"+def+"/properties/kind/const"); got != want {
			t.Errorf("hello.json's %s kind is %v, want %q", def, got, want)
		}
	}
	var hosts []string
	for _, b := range schemaValue(t, "hello.json", "/$defs/result/oneOf").([]any) {
		hosts = append(hosts, b.(map[string]any)["$ref"].(string))
	}
	if want := []string{"#/$defs/hostResult", "#/$defs/hubResult"}; !slices.Equal(hosts, want) {
		t.Errorf("hello's result is a oneOf of %v, want %v", hosts, want)
	}

	// Which reasons go with which code: errorData's if/then table, one entry
	// per code, is Reasons() grouped by code.
	table := map[string][]string{}
	for _, e := range schemaValue(t, "envelope.json", "/$defs/errorData/allOf").([]any) {
		m := e.(map[string]any)
		code := m["if"].(map[string]any)["properties"].(map[string]any)["code"].(map[string]any)["const"].(string)
		if _, dup := table[code]; dup {
			t.Errorf("errorData's table names %s twice", code)
		}
		for _, r := range m["then"].(map[string]any)["properties"].(map[string]any)["reason"].(map[string]any)["enum"].([]any) {
			table[code] = append(table[code], r.(string))
		}
	}
	if !reflect.DeepEqual(table, byCode) {
		t.Fatalf("errorData's reason table\n got %v\nwant %v", table, byCode)
	}
	if len(table) != len(protocol.Codes()) {
		t.Fatalf("errorData's table has %d codes, the closed set %d", len(table), len(protocol.Codes()))
	}
}
