package protocol

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

// The schema (plan 027 §3.11, SD-14): hand-written JSON Schema, draft
// 2020-12, one file per method and per notification plus four shared ones,
// embedded here for every test that checks the wire against it — this
// package's own, the codec twins in internal/agent and internal/transcript,
// the fake host's fixture replay (C9) and the published copy's comparison
// (C10). Rejected: generating it from the Go types, which gives weaker schemas
// (no enums from sentinels) and cannot describe the codecs' hand-shaped fields.
//
// The layout:
//
//   - <method>.json for each method (session.attach.json, …): its params and
//     result as $defs "params" and "result", beside any $defs of its own —
//     hello.json's and session.cancel.json's "errorResult" is the data.result
//     their refusals carry;
//   - notification.<name>.json for each notification: its params as $defs
//     "params";
//   - envelope.json: the JSON-RPC request, response, notification and error
//     object, with the code and reason enums and which reasons go with which
//     code;
//   - event.json: the lossless event codec, internal/agent's wireEvent at the
//     root and every nested wire struct as a $def;
//   - snapshot.json: the snapshot codec, internal/transcript's, at the root;
//   - info.json: the session info document, the sessions.list row and both
//     capability sets.
//
// Every file's $id is SchemaBaseURI plus its name, and a $ref between files
// is relative ("event.json#/$defs/queued"), so the published copy resolves
// where it is served.

//go:embed schema/*.json
var schemaFiles embed.FS

// SchemaBaseURI is the base of every schema file's $id: where the published
// copy (docs/reference/protocol/schema/) is served (C10; plan 027 X6).
const SchemaBaseURI = "https://charliek.github.io/craze/reference/protocol/schema/"

// The shared schema files.
const (
	SchemaEnvelope = "envelope.json"
	SchemaEvent    = "event.json"
	SchemaSnapshot = "snapshot.json"
	SchemaInfo     = "info.json"
)

// SchemaFS is the schema directory: every file at its name, no directory
// prefix.
func SchemaFS() fs.FS {
	sub, err := fs.Sub(schemaFiles, "schema")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// SchemaNames is every schema file's name, sorted.
func SchemaNames() []string {
	entries, err := fs.ReadDir(SchemaFS(), ".")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// SchemaFile is one schema file's bytes, by name ("session.attach.json").
func SchemaFile(name string) ([]byte, error) {
	return fs.ReadFile(SchemaFS(), name)
}

// MethodSchema is the name of method's schema file: its params at
// "#/$defs/params" and its result at "#/$defs/result".
func MethodSchema(method string) string { return method + ".json" }

// NotificationSchema is the name of notification's schema file: its params at
// "#/$defs/params".
func NotificationSchema(notification string) string { return "notification." + notification + ".json" }

// SchemaURI is the absolute URI of name's schema file, and of a place inside
// it with a fragment ("#/$defs/params"): what a validator is asked to compile.
func SchemaURI(name string, fragment ...string) string {
	return SchemaBaseURI + name + strings.Join(fragment, "")
}

// SchemaLoader serves the embedded files at their URIs, and nothing else: a
// validator's loader for the schema's scheme, so compiling any file resolves
// every $ref between them from here and never from the network. A fragment is
// ignored (the validator resolves it); a URI outside SchemaBaseURI, or a name
// that is not one of the files, is an error.
func SchemaLoader(uri string) (io.ReadCloser, error) {
	uri, _, _ = strings.Cut(uri, "#")
	name, ok := strings.CutPrefix(uri, SchemaBaseURI)
	if !ok || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("protocol: no embedded schema at %q", uri)
	}
	b, err := SchemaFile(name)
	if err != nil {
		return nil, fmt.Errorf("protocol: no embedded schema at %q: %w", uri, err)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
