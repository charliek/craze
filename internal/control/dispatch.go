package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/charliek/craze/internal/protocol"
)

// request is one line the reader parsed into a request: its id verbatim, its
// method and its params, raw.
type request struct {
	id     json.RawMessage
	method string
	params json.RawMessage
}

// envelopeMembers are the members a request may carry (envelope.json's
// request is strict).
var envelopeMembers = []string{"jsonrpc", "id", "method", "params"}

// parseRequest reads one line as a request (§3.2), or the error that answers
// it: -32700 for a line that is not JSON and -32600 for a batch, a value that
// is not an object, or an object that is not a request. The id is echoed
// whenever it could be read, and is null otherwise.
//
// Validity is decided first: protocol.IsBatch looks only at the first byte, so
// "[garbage" is a line that is not JSON (-32700), and only a VALID array is a
// batch (-32600).
func parseRequest(line []byte) (*request, json.RawMessage, *protocol.Error) {
	if !json.Valid(line) {
		return nil, nil, &protocol.Error{Code: protocol.RPCParseError, Message: "not JSON",
			Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonBadRequest}}
	}
	if protocol.IsBatch(line) {
		return nil, nil, invalidRequest("a batch: protocol 1 takes one request per line")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, nil, invalidRequest("not a JSON object")
	}
	var id json.RawMessage
	if raw, ok := m["id"]; ok {
		if b := bytes.TrimSpace(raw); len(b) > 0 && (b[0] == '"' || b[0] == '-' || (b[0] >= '0' && b[0] <= '9')) {
			id = b
		}
	}
	if id == nil {
		return nil, nil, invalidRequest("no id: a request carries a number or a string")
	}
	for k := range m {
		if !slices.Contains(envelopeMembers, k) {
			return nil, id, invalidRequest(fmt.Sprintf("unknown member %q", k))
		}
	}
	var version, method string
	if json.Unmarshal(m["jsonrpc"], &version) != nil || version != protocol.JSONRPCVersion {
		return nil, id, invalidRequest(`jsonrpc must be "2.0"`)
	}
	if json.Unmarshal(m["method"], &method) != nil || method == "" {
		return nil, id, invalidRequest("no method")
	}
	return &request{id: id, method: method, params: m["params"]}, id, nil
}

// dispatch answers one line. It runs on the reader, holding the line's
// admission slot, which the reply (or a handler's) takes over. What the
// envelope and the connection's state alone decide is answered here; hello
// runs here too, so a request pipelined behind it sees its binding; every
// other method goes to a handler goroutine.
func (c *conn) dispatch(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		// A blank line asks nothing and is answered with nothing.
		c.release()
		return
	}
	req, id, perr := parseRequest(line)
	if perr != nil {
		c.replyErr(id, perr)
		return
	}
	info, known := protocol.Method(req.method)
	switch {
	case !known:
		c.replyErr(req.id, &protocol.Error{Code: protocol.RPCMethodNotFound,
			Message: fmt.Sprintf("unknown method %q", req.method),
			Data:    protocol.ErrorData{Code: protocol.CodeUnsupported, Reason: protocol.ReasonUnknownMethod}})
	case req.method == protocol.MethodHello:
		c.hello(req)
	case c.bound == nil:
		c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonHelloRequired,
			"%s before hello: every method but hello needs one first", req.method))
	case info.HostUnsupported != "":
		c.replyErr(req.id, refused(protocol.CodeUnsupported, info.HostUnsupported,
			"%s is not served by a session host", req.method))
	default:
		b := c.bound
		c.srv.handlers.add()
		go func() {
			defer c.srv.handlers.done()
			c.handle(b, info, req)
		}()
	}
}

// ------------------------------------------------------------------- hello

// resumeReq is hello's resume, as the binding section takes it.
type resumeReq struct {
	clientID string
	token    string
}

// hello opens the connection (§3.2, §3.3, §3.6): the one tolerant method. It
// negotiates the protocol, binds a client (bind.go) and answers with the
// host's identity, the client id and its token. It runs on the reader.
func (c *conn) hello(req *request) {
	if c.bound != nil {
		c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonBadRequest,
			"hello was already answered on this connection: a connection is bound once"))
		return
	}
	var p protocol.HelloParams
	if perr := decodeTolerant(req.params, &p); perr != nil {
		c.replyErr(req.id, perr)
		return
	}
	switch {
	case len(p.Protocols) == 0:
		c.replyErr(req.id, badParams("params.protocols is required: every protocol version the client speaks"))
		return
	case p.Client.Kind == "":
		c.replyErr(req.id, badParams("params.client.kind is required"))
		return
	case !isNull(p.Auth):
		c.replyErr(req.id, badParams("params.auth is reserved and must be absent or null in protocol 1"))
		return
	case !isNull(p.Via):
		c.replyErr(req.id, badParams("params.via is reserved and must be absent or null in protocol 1"))
		return
	}
	version := 0
	for _, v := range p.Protocols {
		if slices.Contains(protocol.SupportedProtocols(), v) && v > version {
			version = v
		}
	}
	if version == 0 {
		supported, err := rawJSON(protocol.HelloErrorResult{Supported: protocol.SupportedProtocols()})
		if err != nil {
			c.replyErr(req.id, failed(err))
			return
		}
		e := refused(protocol.CodeBadRequest, protocol.ReasonProtocolVersion,
			"no protocol version in common: this host speaks %v", protocol.SupportedProtocols())
		e.Data.Result = supported
		c.replyErr(req.id, e)
		return
	}
	var resume *resumeReq
	if p.Resume != nil {
		resume = &resumeReq{clientID: p.Resume.ClientID, token: p.Resume.Token}
	}
	b, resumed, old, err := c.srv.bind(c, resume)
	switch {
	case errors.Is(err, errConnClosing):
		c.release()
		return
	case errors.Is(err, errNotReady):
		c.replyErr(req.id, refused(protocol.CodeUnavailable, protocol.ReasonNotReady,
			"the host is not serving a session yet; retry"))
		return
	case errors.Is(err, errBadToken):
		c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonBadToken,
			"the resume token does not match that client id"))
		return
	case err != nil:
		c.replyErr(req.id, failed(err))
		return
	}
	if old != nil {
		old.close("superseded by a resume")
	}
	st := b.eng.State()
	c.srv.connNote(c.id, map[string]any{"event": "hello", "clientId": b.client, "resumed": resumed})
	c.reply(req.id, protocol.HelloResult{
		Protocol: version,
		Endpoint: protocol.Endpoint{Kind: protocol.EndpointHost, HostID: c.srv.hostID,
			CrazeVersion: c.srv.version, PID: c.srv.pid},
		ClientID:     b.client,
		Token:        b.token,
		Resumed:      resumed,
		Capabilities: protocol.HostCapabilities(),
		Codecs:       codecs(),
		Limits:       protocol.HostLimits(),
		RetryHorizon: retryHorizon(st.RetryHorizon),
	})
}

func isNull(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) == 0 || string(b) == "null"
}

// ------------------------------------------------------------------ replies

// reply queues result as the answer to id, taking over the line's admission
// slot: it is given back once the reply is on the socket, or at once if the
// connection has closed. A reply longer than the outbound limit is replaced
// by failed, reason response_too_large — never truncated (§3.2).
func (c *conn) reply(id json.RawMessage, result any) {
	raw, err := rawJSON(result)
	if err != nil {
		c.replyErr(id, failed(err))
		return
	}
	c.send(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Result: raw})
}

// replyErr queues e as the answer to id (nil: null), as reply does.
func (c *conn) replyErr(id json.RawMessage, e *protocol.Error) {
	c.send(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Error: e})
}

func (c *conn) send(resp protocol.Response) {
	line, err := protocol.MarshalLine(resp)
	if err == nil && len(line)-1 > c.srv.maxLine {
		resp.Result = nil
		resp.Error = refused(protocol.CodeFailed, protocol.ReasonResponseTooLarge,
			"the response would be %d bytes, over the %d-byte line limit", len(line)-1, c.srv.maxLine)
		line, err = protocol.MarshalLine(resp)
	}
	if err != nil {
		resp.Result = nil
		resp.Error = failed(err)
		line, err = protocol.MarshalLine(resp)
		if err != nil {
			c.release()
			return
		}
	}
	if c.out.push(c.ctx, line, c.release) != nil {
		c.release()
	}
}

// drop gives up a request's reply: the connection is gone.
func (c *conn) drop() { c.release() }

// rawJSON is v as the wire writes it — compact, HTML not escaped — without
// its newline: what a result or an embedded document is carried as. Only
// codec output is ever embedded raw inside v (plan 027 X6).
func rawJSON(v any) (json.RawMessage, error) {
	b, err := protocol.MarshalLine(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b[:len(b)-1]), nil
}
