package protocol

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Coverage holds Go wire types and the schema that describes them to each
// other, field for field and both ways, by reflection (plan 027 §3.11): every
// JSON field of a Go type is a property of its schema, of a compatible JSON
// type, required exactly when the field is always written, and every property
// of the schema is a field of the Go type. It is what
// TestSchemaCoversEveryWireField runs over this package's types, and what the
// codec twins — TestEventSchemaCoversTheCodec in internal/agent,
// TestSnapshotSchemaCoversTheCodec in internal/transcript — run over the
// codecs' unexported wire structs, which only their own packages can reach.
// It reads the embedded schema and nothing else, and it validates no
// instance: the tests do that with a JSON Schema validator.
//
// The rules it holds, beyond the fields themselves:
//
//   - An object is strict (additionalProperties or unevaluatedProperties
//     false) or explicitly tolerant (additionalProperties true); only the
//     objects CoverageOptions.Tolerant names may be tolerant.
//   - A json.RawMessage is a payload of another codec, or of the envelope's
//     own choosing: each one met must be listed in CoverageOptions.Raw with the
//     $ref its schema must be, so none is ever left undescribed.
//   - A field is required exactly when its json tag has neither omitempty nor
//     omitzero, unless CoverageOptions.Presence says otherwise.
//   - A document that is one of several Go types (hello's result: a host's
//     or a hub's, plan 027 X5) is a oneOf with a branch per type, and
//     CheckOneOf holds each type to its own branch by these same rules.
//   - A type with its own MarshalJSON or UnmarshalJSON (time.Time and
//     json.RawMessage aside) is a leaf: its schema is checked by the
//     instances a test validates, not here.
type Coverage struct {
	opts     CoverageOptions
	docs     map[string]any
	visited  map[string]bool
	checked  map[string]bool
	rawSeen  map[string]bool
	problems []string
}

// CoverageOptions tune a Coverage for one set of wire types.
type CoverageOptions struct {
	// Raw maps each json.RawMessage field met, as "GoType.jsonName" (with "[]"
	// appended for a list's items and "{}" for a map's values), to the schema
	// reference its property must be, absolute within the schema set
	// ("event.json#/$defs/queued", "snapshot.json#"), or to "" for a property
	// that must carry no $ref at all (a JSON-RPC id's own type, a result checked
	// by the method's own schema). A raw field not listed, and an entry that
	// matches no field, are problems.
	Raw map[string]string
	// Presence overrides the omitempty rule for a field, "GoType.jsonName" to
	// whether it is required: for a codec whose Go struct is a decoder's, not
	// the shape its encoder writes.
	Presence map[string]bool
	// Tolerant names the objects that may declare additionalProperties: true,
	// as "file#pointer" ("hello.json#/$defs/params").
	Tolerant map[string]bool
}

// NewCoverage reads every embedded schema file.
func NewCoverage(opts CoverageOptions) (*Coverage, error) {
	c := &Coverage{
		opts:    opts,
		docs:    map[string]any{},
		visited: map[string]bool{},
		checked: map[string]bool{},
		rawSeen: map[string]bool{},
	}
	for _, name := range SchemaNames() {
		b, err := fs.ReadFile(SchemaFS(), name)
		if err != nil {
			return nil, err
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("protocol: schema %s: %w", name, err)
		}
		c.docs[name] = doc
	}
	return c, nil
}

// Check holds Go type t to the schema at ref ("hello.json#/$defs/params",
// "event.json#" for a file's root).
func (c *Coverage) Check(ref string, t reflect.Type) {
	n, err := c.resolve(schemaNode{}, ref)
	if err != nil {
		c.problem("%s: %v", ref, err)
		return
	}
	c.visited[n.key()] = true
	c.check(t.String(), "", n, t)
}

// CheckOneOf holds each of variants to the matching branch, in order, of the
// oneOf at ref: for a document that is one of several shapes, each its own Go
// type — hello's result, a host's (HelloResult) or a hub's (HubHelloResult),
// plan 027 X5. It is the one place the checker looks into a oneOf, and it
// does not relax anything: the oneOf must have exactly one branch per
// variant, and each branch is held to its own type as Check holds one, both
// ways, so a field a variant always writes is required in that variant's own
// branch rather than merely somewhere in the union. (A oneOf whose branches
// are the same Go type's alternatives — session.prompt's result — is one
// object with conditional members, and stays Check's.)
func (c *Coverage) CheckOneOf(ref string, variants ...reflect.Type) {
	n, err := c.resolve(schemaNode{}, ref)
	if err != nil {
		c.problem("%s: %v", ref, err)
		return
	}
	c.visited[n.key()] = true
	branches, _ := n.obj()["oneOf"].([]any)
	if len(branches) != len(variants) {
		c.problem("%s: a oneOf of %d branches, and %d Go types to hold them to", n.key(), len(branches), len(variants))
		return
	}
	for i, b := range branches {
		c.check(variants[i].String(), "", n.child(b, "oneOf", strconv.Itoa(i)), variants[i])
	}
}

// Problems is every difference found, sorted, with every Raw entry that
// matched no field among them.
func (c *Coverage) Problems() []string {
	out := slices.Clone(c.problems)
	for k := range c.opts.Raw {
		if !c.rawSeen[k] {
			out = append(out, fmt.Sprintf("Raw lists %q, which is no json.RawMessage field that was checked", k))
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// Unvisited is every $defs entry of file that no Check reached, by a $ref or
// as its entry: a description nothing on the wire uses.
func (c *Coverage) Unvisited(file string) []string {
	doc, _ := c.docs[file].(map[string]any)
	defs, _ := doc["$defs"].(map[string]any)
	var out []string
	for name := range defs {
		k := file + "#/$defs/" + escapePointer(name)
		if !c.visited[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (c *Coverage) problem(format string, a ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, a...))
}

// schemaNode is one place in the schema set: a file, a JSON pointer inside
// it, and the value there.
type schemaNode struct {
	file string
	ptr  string
	v    any
}

func (n schemaNode) key() string { return n.file + "#" + n.ptr }

func (n schemaNode) obj() map[string]any {
	m, _ := n.v.(map[string]any)
	return m
}

func (n schemaNode) child(v any, segments ...string) schemaNode {
	p := n.ptr
	for _, s := range segments {
		p += "/" + escapePointer(s)
	}
	return schemaNode{file: n.file, ptr: p, v: v}
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// resolve is ref read from where from is: "#/…" within from's file,
// "name.json#/…" in another, an absolute URI under SchemaBaseURI likewise.
func (c *Coverage) resolve(from schemaNode, ref string) (schemaNode, error) {
	file, frag, _ := strings.Cut(ref, "#")
	file = strings.TrimPrefix(file, SchemaBaseURI)
	if file == "" {
		file = from.file
	}
	doc, ok := c.docs[file]
	if !ok {
		return schemaNode{}, fmt.Errorf("no schema file %q", file)
	}
	v := doc
	for _, seg := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		if seg == "" {
			continue
		}
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			next, ok := x[seg]
			if !ok {
				return schemaNode{}, fmt.Errorf("%s#%s: no %q", file, frag, seg)
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(x) {
				return schemaNode{}, fmt.Errorf("%s#%s: no item %q", file, frag, seg)
			}
			v = x[i]
		default:
			return schemaNode{}, fmt.Errorf("%s#%s: %q is inside a scalar", file, frag, seg)
		}
	}
	return schemaNode{file: file, ptr: frag, v: v}, nil
}

// follow is n's $ref target when it has one, marked visited.
func (c *Coverage) follow(n schemaNode) (schemaNode, bool) {
	ref, ok := n.obj()["$ref"].(string)
	if !ok {
		return schemaNode{}, false
	}
	t, err := c.resolve(n, ref)
	if err != nil {
		c.problem("%s: $ref %q: %v", n.key(), ref, err)
		return schemaNode{}, false
	}
	c.visited[t.key()] = true
	return t, true
}

// jsonTypes is the set of JSON types n admits, from its type keyword, its
// const or enum, or its $ref; nil when it names none.
func (c *Coverage) jsonTypes(n schemaNode) []string {
	m := n.obj()
	if own := ownJSONTypes(m); own != nil {
		return own
	}
	var values []any
	if v, ok := m["const"]; ok {
		values = append(values, v)
	}
	if e, ok := m["enum"].([]any); ok {
		values = append(values, e...)
	}
	if len(values) > 0 {
		var out []string
		for _, v := range values {
			out = append(out, jsonTypeOf(v))
		}
		return out
	}
	if t, ok := c.follow(n); ok {
		return c.jsonTypes(t)
	}
	return nil
}

func jsonTypeOf(v any) string {
	switch x := v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case nil:
		return "null"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func (c *Coverage) wantType(path string, n schemaNode, want ...string) {
	got := c.jsonTypes(n)
	if got == nil {
		c.problem("%s (%s): the schema names no JSON type; want %s", path, n.key(), strings.Join(want, " or "))
		return
	}
	for _, w := range want {
		if slices.Contains(got, w) || w == "integer" && slices.Contains(got, "number") {
			return
		}
	}
	c.problem("%s (%s): the schema's type is %v; the Go type wants %s", path, n.key(), got, strings.Join(want, " or "))
}

var (
	rawMessageType = reflect.TypeFor[json.RawMessage]()
	timeType       = reflect.TypeFor[time.Time]()
	marshalerType  = reflect.TypeFor[json.Marshaler]()
	unmarshalType  = reflect.TypeFor[json.Unmarshaler]()
)

// check holds t to n. path names the place for a problem; raw is the Raw
// key a json.RawMessage here would be listed under.
func (c *Coverage) check(path, raw string, n schemaNode, t reflect.Type) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == rawMessageType:
		c.checkRaw(path, raw, n)
		return
	case t == timeType:
		c.wantType(path, n, "string")
		return
	case t.Implements(marshalerType) || reflect.PointerTo(t).Implements(unmarshalType) ||
		reflect.PointerTo(t).Implements(marshalerType):
		// A leaf that shapes its own JSON: the $def it names is its
		// description all the same.
		c.follow(n)
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		c.checkObject(path, n, t)
	case reflect.Slice, reflect.Array:
		c.wantType(path, n, "array")
		items, ok := c.member(n, "items")
		if !ok {
			c.problem("%s (%s): a list with no items schema", path, n.key())
			return
		}
		c.check(path+"[]", raw+"[]", items, t.Elem())
	case reflect.Map:
		c.wantType(path, n, "object")
		vals, ok := c.member(n, "additionalProperties")
		if !ok {
			c.problem("%s (%s): a map with no additionalProperties schema", path, n.key())
			return
		}
		c.check(path+"{}", raw+"{}", vals, t.Elem())
	case reflect.String:
		c.wantType(path, n, "string")
	case reflect.Bool:
		c.wantType(path, n, "boolean")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		c.wantType(path, n, "integer")
	case reflect.Float32, reflect.Float64:
		c.wantType(path, n, "number")
	case reflect.Interface:
		// Any JSON at all: nothing to hold it to.
	default:
		c.problem("%s: a Go %s cannot be on the wire", path, t.Kind())
	}
}

// member is n's schema-valued keyword key — items, additionalProperties —
// found on n or through its $ref.
func (c *Coverage) member(n schemaNode, key string) (schemaNode, bool) {
	if v, ok := n.obj()[key]; ok {
		if _, isSchema := v.(map[string]any); isSchema {
			return n.child(v, key), true
		}
	}
	if t, ok := c.follow(n); ok {
		return c.member(t, key)
	}
	return schemaNode{}, false
}

func (c *Coverage) checkRaw(path, raw string, n schemaNode) {
	c.rawSeen[raw] = true
	want, listed := c.opts.Raw[raw]
	if !listed {
		c.problem("%s: a json.RawMessage (%s) that CoverageOptions.Raw does not account for", path, raw)
		return
	}
	ref, hasRef := n.obj()["$ref"].(string)
	switch {
	case want == "" && hasRef:
		c.problem("%s (%s): want no $ref, the schema has %q", path, n.key(), ref)
	case want == "":
	case !hasRef:
		c.problem("%s (%s): want a $ref to %s, the schema has none", path, n.key(), want)
	default:
		got, err := c.resolve(n, ref)
		if err != nil {
			c.problem("%s (%s): $ref %q: %v", path, n.key(), ref, err)
			return
		}
		c.visited[got.key()] = true
		wf, wp, _ := strings.Cut(want, "#")
		if got.file != wf || got.ptr != wp {
			c.problem("%s (%s): the schema refers to %s, want %s", path, n.key(), got.key(), want)
		}
	}
}

// objectFacts is what one object schema says about its members, gathered
// through $ref and allOf: its properties, which are required, and whether it
// is strict or tolerant (and where it said so).
type objectFacts struct {
	isObject   bool
	props      map[string]schemaNode
	required   map[string]bool
	strict     bool
	tolerantAt string
}

func (c *Coverage) facts(n schemaNode, f *objectFacts, depth int) {
	if depth > 32 {
		c.problem("%s: $ref chain too deep", n.key())
		return
	}
	m := n.obj()
	if m == nil {
		c.problem("%s: not a schema object", n.key())
		return
	}
	if slices.Contains(ownJSONTypes(m), "object") {
		f.isObject = true
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, p := range props {
			if _, dup := f.props[name]; dup {
				c.problem("%s: property %q is described twice", n.key(), name)
			}
			f.props[name] = n.child(p, "properties", name)
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				f.required[s] = true
			}
		}
	}
	if m["additionalProperties"] == false || m["unevaluatedProperties"] == false {
		f.strict = true
	}
	if m["additionalProperties"] == true {
		f.tolerantAt = n.key()
	}
	if t, ok := c.follow(n); ok {
		c.facts(t, f, depth+1)
	}
	if all, ok := m["allOf"].([]any); ok {
		for i, a := range all {
			c.facts(n.child(a, "allOf", strconv.Itoa(i)), f, depth+1)
		}
	}
}

// ownJSONTypes is m's own type keyword as a list, nil when it has none.
func ownJSONTypes(m map[string]any) []string {
	switch t := m["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (c *Coverage) checkObject(path string, n schemaNode, t reflect.Type) {
	once := n.key() + " " + t.PkgPath() + "." + t.Name()
	if c.checked[once] {
		return
	}
	c.checked[once] = true

	f := &objectFacts{props: map[string]schemaNode{}, required: map[string]bool{}}
	c.facts(n, f, 0)
	if !f.isObject {
		c.problem("%s (%s): the Go type is a struct and the schema is not type object", path, n.key())
	}
	switch {
	case f.tolerantAt != "" && !c.opts.Tolerant[f.tolerantAt]:
		c.problem("%s (%s): additionalProperties true, and it is not an object allowed to be tolerant", path, f.tolerantAt)
	case f.tolerantAt == "" && !f.strict:
		c.problem("%s (%s): the object is neither strict (additionalProperties/unevaluatedProperties false) nor declared tolerant", path, n.key())
	}

	fields := jsonFields(t)
	seen := map[string]bool{}
	for _, fd := range fields {
		seen[fd.name] = true
		p, ok := f.props[fd.name]
		if !ok {
			c.problem("%s: Go field %s.%s (%q) has no property in %s", path, fd.decl, fd.goName, fd.name, n.key())
			continue
		}
		c.check(path+"."+fd.name, fd.decl+"."+fd.name, p, fd.typ)
		want := !fd.omit
		if o, ok := c.opts.Presence[fd.decl+"."+fd.name]; ok {
			want = o
		}
		if got := f.required[fd.name]; got != want {
			if want {
				c.problem("%s.%s: always written, and %s does not require it", path, fd.name, n.key())
			} else {
				c.problem("%s.%s: omitted at its zero value, and %s requires it", path, fd.name, n.key())
			}
		}
	}
	for name := range f.props {
		if !seen[name] {
			c.problem("%s: property %q of %s is no field of %s", path, name, n.key(), t)
		}
	}
	for name := range f.required {
		if _, ok := f.props[name]; !ok {
			c.problem("%s: %s requires %q, which it does not describe", path, n.key(), name)
		}
	}
}

// goField is one JSON member of a Go struct: its JSON name, the struct that
// declares it (so an embedded struct's fields keep their own), its Go type,
// and whether the zero value is omitted.
type goField struct {
	name, goName, decl string
	typ                reflect.Type
	omit               bool
}

// jsonFields is t's JSON members as encoding/json sees them: an untagged
// embedded struct's fields flattened in, "-" and unexported fields left out.
func jsonFields(t reflect.Type) []goField {
	var out []goField
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, jsonFields(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		omit := false
		for _, o := range strings.Split(opts, ",") {
			if o == "omitempty" || o == "omitzero" {
				omit = true
			}
		}
		out = append(out, goField{name: name, goName: f.Name, decl: t.Name(), typ: f.Type, omit: omit})
	}
	return out
}
