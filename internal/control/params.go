package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// Params are strict (§3.2, "tolerant inbound, strict outbound"): a member a
// method does not define is refused -32602, reason unknown_field, naming it —
// at any depth — because a swallowed option lets a client believe it took
// effect. A member the method requires (its Go field has no omitempty or
// omitzero, which is also the schema's rule) that is missing, a null where a
// value belongs, or a value of the wrong type is -32602, reason bad_request.
// hello alone is tolerant: members it does not know are ignored at every
// depth, and what it requires is still required.

// decodeParams decodes raw — a JSON object, or nothing for {} — into v, a
// pointer to a params struct, strictly.
func decodeParams(raw json.RawMessage, v any) *protocol.Error {
	return decode(raw, v, true)
}

// decodeTolerant is decodeParams for hello: unknown members are ignored.
func decodeTolerant(raw json.RawMessage, v any) *protocol.Error {
	return decode(raw, v, false)
}

func decode(raw json.RawMessage, v any, strict bool) *protocol.Error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return badParams("params: %v", err)
	}
	if tree == nil {
		return badParams("params must be an object, not null")
	}
	if perr := checkShape(tree, reflect.TypeOf(v).Elem(), "params", strict); perr != nil {
		return perr
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return badParams("params: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// checkShape holds a decoded JSON value to a Go type's shape: the members of
// every object, which are required, and where null may stand. Scalar types are
// left to json.Unmarshal, which refuses a mismatch.
func checkShape(val any, t reflect.Type, path string, strict bool) *protocol.Error {
	switch {
	case t == rawType:
		return nil
	case t.Kind() == reflect.Pointer:
		if val == nil {
			return nil
		}
		return checkShape(val, t.Elem(), path, strict)
	case t == timeType:
		// A time is a string; Unmarshal checks its form.
	case t.Kind() == reflect.Struct:
		obj, ok := val.(map[string]any)
		if !ok {
			return badParams("%s must be an object", path)
		}
		fs := fieldsOf(t)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f, ok := fs.byName[k]
			if !ok {
				if strict {
					return unknownField(path + "." + k)
				}
				continue
			}
			if obj[k] == nil && !nullable(f.typ) {
				return badParams("%s.%s must not be null", path, k)
			}
			if perr := checkShape(obj[k], f.typ, path+"."+k, strict); perr != nil {
				return perr
			}
		}
		for _, f := range fs.list {
			if _, ok := obj[f.name]; f.required && !ok {
				return badParams("%s.%s is required", path, f.name)
			}
		}
	case t.Kind() == reflect.Map:
		if val == nil {
			return nil
		}
		obj, ok := val.(map[string]any)
		if !ok {
			return badParams("%s must be an object", path)
		}
		for k, v := range obj {
			if v == nil && !nullable(t.Elem()) {
				return badParams("%s[%q] must not be null", path, k)
			}
			if perr := checkShape(v, t.Elem(), fmt.Sprintf("%s[%q]", path, k), strict); perr != nil {
				return perr
			}
		}
	case t.Kind() == reflect.Slice:
		if val == nil {
			return nil
		}
		arr, ok := val.([]any)
		if !ok {
			return badParams("%s must be an array", path)
		}
		for i, v := range arr {
			if v == nil && !nullable(t.Elem()) {
				return badParams("%s[%d] must not be null", path, i)
			}
			if perr := checkShape(v, t.Elem(), fmt.Sprintf("%s[%d]", path, i), strict); perr != nil {
				return perr
			}
		}
	}
	return nil
}

// nullable reports whether null may stand for a value of t: an absent
// optional (a pointer), an empty collection, or a raw payload.
func nullable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	return false
}

// field is one JSON member of a struct: its name, Go type, and whether it is
// required (its tag has neither omitempty nor omitzero).
type field struct {
	name     string
	typ      reflect.Type
	required bool
}

type fields struct {
	list   []field
	byName map[string]field
}

var fieldCache sync.Map // reflect.Type → *fields

// fieldsOf is t's JSON members, embedded structs' flattened in.
func fieldsOf(t reflect.Type) *fields {
	if f, ok := fieldCache.Load(t); ok {
		return f.(*fields)
	}
	fs := &fields{byName: map[string]field{}}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			sf := t.Field(i)
			tag := sf.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if sf.Anonymous && name == "" && sf.Type.Kind() == reflect.Struct {
				walk(sf.Type)
				continue
			}
			if !sf.IsExported() {
				continue
			}
			if name == "" {
				name = sf.Name
			}
			f := field{name: name, typ: sf.Type,
				required: !strings.Contains(","+opts+",", ",omitempty,") && !strings.Contains(","+opts+",", ",omitzero,")}
			fs.list = append(fs.list, f)
			fs.byName[name] = f
		}
	}
	walk(t)
	v, _ := fieldCache.LoadOrStore(t, fs)
	return v.(*fields)
}

// canonicalCommandID reports whether id is a canonical positive decimal
// (SF-12): no sign, no leading zero, within uint64 — the engine's own rule
// (parseCommand), checked on the wire first.
func canonicalCommandID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == id
}

// idsOf reads a params struct's sessionId and commandId, which every
// session-scoped and every mutating method's params carry under those names.
func idsOf(p any) (sessionID, commandID string) {
	v := reflect.ValueOf(p)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if f := v.FieldByName("SessionID"); f.IsValid() && f.Kind() == reflect.String {
		sessionID = f.String()
	}
	if f := v.FieldByName("CommandID"); f.IsValid() && f.Kind() == reflect.String {
		commandID = f.String()
	}
	return sessionID, commandID
}
