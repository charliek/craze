package modeltable

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// redacted is what every rendering of a set Secret prints in its place.
const redacted = "[redacted]"

// Secret is an API key. Every way the standard library can render one —
// String, GoString, fmt's verbs through Format, encoding.TextMarshaler (which
// a TOML encoder uses), and encoding/json — prints "[redacted]" instead, so a
// stray %+v of a Table, a Provider or a Resolved, or a json.Marshal of one,
// cannot put a key in a log line, an error or an event. Reveal is the one way
// to read the value. Outside this package (where Save writes providers.toml
// and Resolve trims the inline key) only the llm factory calls it, to hand
// the key to the provider's HTTP client.
//
// The redaction only holds through exported fields: fmt and encoding/json
// read an unexported field by reflection without calling its methods. Every
// struct in this package that holds a Secret therefore holds it in an
// exported field.
//
// An empty Secret renders as "" rather than "[redacted]", so a printed Table
// shows which providers have an inline key without showing any key.
type Secret string

// Reveal returns the key itself. Call it only where the key leaves craze for
// its provider or its file; never format, log or wrap the result.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (s Secret) String() string { return s.shown() }

// GoString implements fmt.GoStringer, for %#v.
func (s Secret) GoString() string { return strconv.Quote(s.shown()) }

// Format implements fmt.Formatter. String and GoString alone would leave a
// gap: for a verb fmt thinks a string cannot take (%d, say) it reports the bad
// verb and then prints the raw underlying value, method-free, when the Secret
// sits inside a struct. Format takes every verb, so no verb reaches that path.
func (s Secret) Format(f fmt.State, verb rune) {
	switch {
	case verb == 'q', verb == 'v' && f.Flag('#'):
		_, _ = io.WriteString(f, s.GoString())
	default:
		_, _ = io.WriteString(f, s.shown())
	}
}

// MarshalText implements encoding.TextMarshaler, which the TOML encoder and
// encoding/json's map-key path both use.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.shown()), nil }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.shown()) }

// shown is the one place the rendering rule lives.
func (s Secret) shown() string {
	if s == "" {
		return ""
	}
	return redacted
}
