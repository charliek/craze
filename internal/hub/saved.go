package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"github.com/charliek/craze/internal/protocol"
)

// sessions.saved (plan 046 B2): the saved sessions — the hub's own session
// index's sessions that no running host serves, chosen as the session list
// chooses its saved rows — each with the id that reopens it. The hub holds
// the params to the method's form and computes nothing else: the answer is
// the CLI's (Creates.Saved: the index, the registry and internal/roster's
// SavedSessions, which the session list's saved rows are chosen by too), so
// the hub still imports neither the providers nor the index. A hub whose
// Saved is nil, or that has no Creates at all, serves no such method — it
// falls through to the dispatch's unsupported, reason unsupported — and a
// host answers it unsupported, reason hub_only. The connection capability
// reopen says a hub serves it and session.reopen both (plan 046 B1); a client
// that asks a hub without it takes unsupported (or, a hub from before it,
// unknown_method) as a hub that cannot.

// savedAnswer is the answer sessions.saved takes, nil for a hub that does
// not serve it (the file's comment).
func (h *hub) savedAnswer() func(protocol.SavedParams, func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
	if h.cr == nil {
		return nil
	}
	return h.cr.o.Saved
}

// saved answers sessions.saved, on a hub that serves it (savedAnswer
// non-nil, which the dispatch checked), on the connection's own goroutine,
// holding no lock while the answer is computed: its params held to the
// method's form (savedParams), then Saved's answer or refusal, handed the
// hub's log. The list is written whole — [] for none, never null — and
// recomputed at every call: nothing is kept.
func (c *conn) saved(req *request) bool {
	q, perr := savedParams(req.params)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	res, perr := c.s.h.savedAnswer()(q, c.s.h.logf)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	if res.Sessions == nil {
		res.Sessions = []protocol.SavedSession{}
	}
	return c.reply(req.id, res)
}

// savedMembers is every member sessions.saved's params define.
var savedMembers = []string{"workspace", "limit", "subAgents"}

// savedParams holds raw to sessions.saved's params' form (strict, as a host
// holds a method's): absent, or an object whose members are workspace — a
// non-empty string, an absolute path of at most SavedWorkspaceMax
// characters, cleaned — limit — an integer from SavedLimitMin to
// SavedLimitMax, SavedLimitDefault when absent — and subAgents — true or
// false. A member it does not define is unknown_field, naming it; null
// params, a member given twice (repeatedMember: a map keeps only the last,
// which would hide an invalid first), a null member, a member of another
// type, a relative workspace and a limit out of range are bad_request.
func savedParams(raw json.RawMessage) (protocol.SavedParams, *protocol.Error) {
	q := protocol.SavedParams{Limit: protocol.SavedLimitDefault}
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return q, nil
	}
	if string(b) == "null" {
		return q, badParams("params must be an object, not null")
	}
	if k, ok := repeatedMember(b); ok {
		return q, badParams("params.%s is given more than once", k)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return q, badParams("params must be an object")
	}
	var unknown []string
	for k := range m {
		if !slices.Contains(savedMembers, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return q, &protocol.Error{Code: protocol.RPCInvalidParams, Message: "unknown field params." + unknown[0],
			Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonUnknownField}}
	}
	if v, ok := m["workspace"]; ok {
		var ws string
		switch {
		case isNull(v) || json.Unmarshal(v, &ws) != nil:
			return q, badParams("params.workspace must be a string")
		case ws == "":
			return q, badParams("params.workspace must not be empty")
		case utf8.RuneCountInString(ws) > protocol.SavedWorkspaceMax:
			return q, badParams("params.workspace is longer than %d characters", protocol.SavedWorkspaceMax)
		case !filepath.IsAbs(ws):
			return q, badParams("params.workspace %q is not an absolute path", ws)
		}
		q.Workspace = filepath.Clean(ws)
	}
	if v, ok := m["limit"]; ok {
		var n int
		if isNull(v) || json.Unmarshal(v, &n) != nil {
			return q, badParams("params.limit must be an integer")
		}
		if n < protocol.SavedLimitMin || n > protocol.SavedLimitMax {
			return q, badParams("params.limit %d is not from %d to %d", n, protocol.SavedLimitMin, protocol.SavedLimitMax)
		}
		q.Limit = n
	}
	if v, ok := m["subAgents"]; ok {
		if isNull(v) || json.Unmarshal(v, &q.SubAgents) != nil {
			return q, badParams("params.subAgents must be true or false")
		}
	}
	return q, nil
}

// repeatedMember is the first member b — a JSON object — names twice, by
// its decoded name (an escaped spelling of a name is the name), and true; ""
// and false when it names none twice, and for anything that is not a
// well-formed object, which decoding it refuses next.
func repeatedMember(b []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		k, ok := tok.(string)
		if !ok {
			return "", false
		}
		if seen[k] {
			return k, true
		}
		seen[k] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return "", false
		}
	}
	return "", false
}

// Saved asks the hub at socket for the saved sessions q keeps — hello as
// who, then sessions.saved — and answers the result as the hub wrote it, not
// re-encoded (a protocol.SavedResult), within ctx, as List does. A refusal is
// an error wrapping the hub's *protocol.Error: unsupported from a hub that
// cannot answer it (reason unsupported) or one from before it (reason
// unknown_method, -32601).
func Saved(ctx context.Context, socket string, who protocol.ClientInfo, q protocol.SavedParams) (json.RawMessage, error) {
	hc, _, err := openHub(ctx, socket, who)
	if err != nil {
		return nil, err
	}
	defer hc.close()
	res, err := hc.call(protocol.MethodSessionsSaved, q)
	if err != nil {
		return nil, err
	}
	var check protocol.SavedResult
	if err := json.Unmarshal(res, &check); err != nil {
		return nil, fmt.Errorf("hub: %s's result: %w", protocol.MethodSessionsSaved, err)
	}
	return res, nil
}
