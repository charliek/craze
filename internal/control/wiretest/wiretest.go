// Package wiretest holds wire lines to the control socket's JSON Schema (plan
// 027 §3.11): a line a test sent or read — a request, a response, a
// notification — is validated against internal/protocol's embedded schema with
// github.com/kaptinlin/jsonschema, the envelope first and then the method's own
// params, result or errorResult. It is a helper for tests alone — the server's
// (internal/control), the client's (internal/remote) and the fake host's
// fixture replay (internal/fakehost) — and nothing that ships imports it.
//
// It lives outside internal/protocol because that package's non-test files are
// held to the standard library (its depguard rule), and a helper other
// packages' tests import has to be a non-test file.
package wiretest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/charliek/craze/internal/protocol"
	"github.com/kaptinlin/jsonschema"
)

// Checker validates wire lines. It compiles each schema once and is safe for
// concurrent use: the compiler and the compiled schemas are used under one
// mutex, since the validator documents neither as safe to share.
type Checker struct {
	mu       sync.Mutex
	compiler *jsonschema.Compiler
	schemas  map[string]*jsonschema.Schema
}

// New is a Checker whose compiler resolves every $ref from the embedded schema
// and never from the network.
func New() *Checker {
	c := jsonschema.NewCompiler()
	c.RegisterLoader("https", protocol.SchemaLoader)
	c.RegisterLoader("http", protocol.SchemaLoader)
	return &Checker{compiler: c, schemas: map[string]*jsonschema.Schema{}}
}

// shared is the process's one Checker (Default).
var shared = sync.OnceValue(New)

// Default is a Checker shared by every caller in the process, so each schema
// is compiled once per test binary.
func Default() *Checker { return shared() }

// validate holds instance to the schema at file#fragment.
func (c *Checker) validate(file, fragment string, instance []byte) error {
	uri := protocol.SchemaURI(file, fragment)
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.schemas[uri]
	if !ok {
		var err error
		s, err = c.compiler.Schema(uri)
		if err != nil {
			return fmt.Errorf("wiretest: compile %s: %w", uri, err)
		}
		if u := s.UnresolvedReferenceURIs(); len(u) > 0 {
			return fmt.Errorf("wiretest: %s leaves $refs unresolved: %v", uri, u)
		}
		c.schemas[uri] = s
	}
	r := s.Validate(instance)
	if r.IsValid() {
		return nil
	}
	details := r.DetailedErrors()
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	msgs := make([]string, 0, len(keys))
	for _, k := range keys {
		msgs = append(msgs, k+": "+details[k])
	}
	return fmt.Errorf("wiretest: %s%s rejects %s: %s", file, fragment, clip(instance), strings.Join(msgs, "; "))
}

// clip is a line short enough to put in an error.
func clip(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// hasDef reports whether file's $defs holds name.
func hasDef(file, name string) bool {
	b, err := protocol.SchemaFile(file)
	if err != nil {
		return false
	}
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return false
	}
	_, ok := doc.Defs[name]
	return ok
}

// Response validates one response line (a trailing newline is allowed): the
// envelope's response, then — when method is not "" — the result against the
// method's result, or an error's data.result against the method's errorResult
// (hello's and session.cancel's). method is the request's, which a response
// does not carry; "" checks the envelope alone (a response with id null, to a
// line whose method was never read).
func (c *Checker) Response(method string, line []byte) error {
	line = trim(line)
	if err := c.validate(protocol.SchemaEnvelope, "#/$defs/response", line); err != nil {
		return err
	}
	if method == "" {
		return nil
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("wiretest: %w", err)
	}
	file := protocol.MethodSchema(method)
	if _, err := protocol.SchemaFile(file); err != nil {
		// A method protocol 1 defines nothing of (session.create), or one it
		// does not know: its answer is an error, which the envelope has held.
		if resp.Error == nil {
			return fmt.Errorf("wiretest: a result for %q, which has no schema", method)
		}
		return nil
	}
	switch {
	case resp.Error == nil:
		return c.validate(file, "#/$defs/result", resp.Result)
	case len(resp.Error.Data.Result) > 0:
		if !hasDef(file, "errorResult") {
			return fmt.Errorf("wiretest: %s carries data.result, and %s defines no errorResult", clip(line), file)
		}
		return c.validate(file, "#/$defs/errorResult", resp.Error.Data.Result)
	}
	return nil
}

// Request validates one request line: the envelope's request and the method's
// params (a method with no schema file is refused).
func (c *Checker) Request(line []byte) error {
	line = trim(line)
	if err := c.validate(protocol.SchemaEnvelope, "#/$defs/request", line); err != nil {
		return err
	}
	var req protocol.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return fmt.Errorf("wiretest: %w", err)
	}
	params := req.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	return c.validate(protocol.MethodSchema(req.Method), "#/$defs/params", params)
}

// Notification validates one notification line: the envelope's notification
// and the notification's own params.
func (c *Checker) Notification(line []byte) error {
	line = trim(line)
	if err := c.validate(protocol.SchemaEnvelope, "#/$defs/notification", line); err != nil {
		return err
	}
	var n protocol.Notification
	if err := json.Unmarshal(line, &n); err != nil {
		return fmt.Errorf("wiretest: %w", err)
	}
	return c.validate(protocol.NotificationSchema(n.Method), "#/$defs/params", n.Params)
}

// ErrNotALine is Server's answer for a line that is neither a response nor a
// notification.
var ErrNotALine = errors.New("wiretest: neither a response nor a notification")

// Server validates one line a host wrote: a response (method as for
// Response) or a notification, told apart by the members it carries.
func (c *Checker) Server(method string, line []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trim(line), &probe); err != nil {
		return fmt.Errorf("wiretest: %w", err)
	}
	_, hasID := probe["id"]
	_, hasMethod := probe["method"]
	switch {
	case hasID && !hasMethod:
		return c.Response(method, line)
	case hasMethod && !hasID:
		return c.Notification(line)
	default:
		return fmt.Errorf("%w: %s", ErrNotALine, clip(line))
	}
}

func trim(line []byte) []byte {
	return []byte(strings.TrimSuffix(string(line), "\n"))
}
