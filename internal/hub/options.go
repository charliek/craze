package hub

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/charliek/craze/internal/protocol"
)

// sessions.createOptions (plan 036 §3.4): what a session.create can start on
// this machine — the agent providers with their availability, the provider a
// create that names none starts, and the directories sessions last ran in —
// for a client to offer before any session exists. The hub computes none of
// it: the answer is the CLI's (Creates.Options, internal/cli's hub column of
// the availability check, config.toml and the session index), so the hub
// still imports neither the config nor the providers. A hub whose Options is
// nil, or that has no Creates at all, serves no such method — its hello omits
// the connection capability createOptions, and the method falls through to
// the dispatch's unsupported, reason unsupported — and a host answers it
// unsupported, reason hub_only.

// options is the answer sessions.createOptions takes, nil for a hub that
// does not serve it (the file's comment).
func (h *hub) options() func(logf func(string, ...any)) protocol.CreateOptionsResult {
	if h.cr == nil {
		return nil
	}
	return h.cr.o.Options
}

// createOptions answers sessions.createOptions, on a hub that serves it
// (options non-nil, which the dispatch checked), on the connection's own
// goroutine, holding no lock while the answer is computed: its params must be
// {} (a member is unknown_field), and the answer is Options', handed the
// hub's log. It is written whole — no list is null, so a missing one is
// written [] — and recomputed at every call: nothing is kept.
func (c *conn) createOptions(req *request) bool {
	if perr := emptyParams(req.params); perr != nil {
		return c.replyErr(req.id, perr)
	}
	res := c.s.h.options()(c.s.h.logf)
	if res.Providers == nil {
		res.Providers = []protocol.ProviderOption{}
	}
	if res.RecentDirs == nil {
		res.RecentDirs = []protocol.RecentDir{}
	}
	return c.reply(req.id, res)
}

// CreateOptions asks the hub at socket what a session.create can start —
// hello as who, then sessions.createOptions — and answers the result as the
// hub wrote it, not re-encoded (a protocol.CreateOptionsResult), within ctx,
// as Create does. A refusal is an error wrapping the hub's *protocol.Error; a
// hub whose hello does not say createOptions — an older one, or one given no
// answer — a *LacksError, nothing asked.
func CreateOptions(ctx context.Context, socket string, who protocol.ClientInfo) (json.RawMessage, error) {
	hc, hello, err := openHub(ctx, socket, who)
	if err != nil {
		return nil, err
	}
	defer hc.close()
	if !hello.Capabilities.CreateOptions {
		return nil, &LacksError{Version: hello.Endpoint.CrazeVersion, Missing: []string{lacksCreateOptions}}
	}
	res, err := hc.call(protocol.MethodSessionsCreateOptions, protocol.CreateOptionsParams{})
	if err != nil {
		return nil, err
	}
	var check protocol.CreateOptionsResult
	if err := json.Unmarshal(res, &check); err != nil {
		return nil, fmt.Errorf("hub: %s's result: %w", protocol.MethodSessionsCreateOptions, err)
	}
	return res, nil
}

// lacksCreateOptions is a hub without the capability createOptions, in
// LacksError's words.
const lacksCreateOptions = "list what it can create"
