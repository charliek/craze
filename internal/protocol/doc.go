// Package protocol is craze's control-socket wire, protocol 1 (plan 027
// §3.2–§3.4): the vocabulary every other program that speaks to a craze host
// pins — shed's craze lane first (discovery/session-control 06) — and the one
// place that vocabulary is written down in Go. The server (internal/control),
// the Go client (internal/remote) and the fake host all take their names,
// shapes, codes, reasons and limits from here, never from a second copy.
//
// What is here:
//
//   - the JSON-RPC 2.0 envelope and its error object (envelope.go), and the
//     framing both ends use: one JSON object per line, a bounded line reader
//     that survives an over-long line, and a writer that marshals one message
//     and its newline (frame.go);
//   - every method and notification name, which methods carry a commandId and
//     which a sessionId (methods.go);
//   - the params and result of every method, the params of every
//     notification, and the shared documents — the session info document, the
//     roster row and both capability sets (types.go);
//   - the closed code set, the exhaustive reason table (the engine's reasons,
//     the protocol's own, and the client-side ones no host sends) and the
//     retry policy by code (codes.go);
//   - the limits, as named constants (limits.go);
//   - the JSON Schema (2020-12) of all of it, hand-written, embedded, and
//     checked both ways against the Go types by reflection (schema.go,
//     coverage.go, schema/*.json).
//
// # Shapes that come from the codecs
//
// A payload the lossless event codec (internal/agent/eventcodec.go) or the
// snapshot codec (internal/transcript/codec.go) already encodes — an event, a
// snapshot, a queue row, an ask body, a settings section — is carried here as
// a json.RawMessage the server fills from that codec's own encoder or exported
// leaf wrapper, and its schema is a $ref into event.json or snapshot.json. So
// each such shape is described once, where the codec that writes it is
// checked against it (the codec twins: TestEventSchemaCoversTheCodec in
// internal/agent, TestSnapshotSchemaCoversTheCodec in internal/transcript),
// and never a second time by hand.
//
// # Conventions of the protocol's own documents
//
//   - Keys are camelCase. A field the plan marks optional (`field?`) is
//     omitted when it is absent; every other field is always present, an
//     empty list as [] — never null, never absent — and an empty string as "".
//     A nil slice marshals as null, so a server fills every list it sends,
//     and writes session.state's settings.config as {} when there are no
//     options (plan 027 X6).
//   - A time is RFC 3339 with nanoseconds, in UTC, as both codecs write it.
//   - Params are strict: a host refuses an unknown params field (-32602,
//     reason unknown_field). hello is the one tolerant method (§3.2): its
//     params, and every object inside them, may carry fields a host does not
//     know, which it ignores.
//   - Results and notifications are exactly what a host sends (their schemas
//     are strict); a client ignores fields, event kinds and notification
//     methods it does not know (05's "tolerant inbound, strict outbound").
//
// Import boundary (a depguard rule on **/internal/protocol/**,
// .golangci.yml): a non-test file here imports the standard library and
// nothing else — no internal/agent, internal/engine or internal/transcript, and
// nothing of craze's. That is what lets every other package, the codecs' own
// tests included, import it. Its tests may import github.com/kaptinlin/jsonschema,
// which compiles the embedded schema and validates instances against it.
package protocol
