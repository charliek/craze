package responsesapi

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	turnStateHeader = "x-codex-turn-state"
	wsReadLimit     = 32 << 20
	wsMessageLimit  = 128 << 20
)

type stickyKey struct {
	route    Route
	endpoint string
}

// StickyRegistry tracks automatic SSE fallback by route and endpoint.
type StickyRegistry struct {
	mu     sync.Mutex
	states map[stickyKey]stickyState
}

type stickyState struct {
	sticky   bool
	failures int
}

// NewStickyRegistry creates an empty fallback registry.
func NewStickyRegistry() *StickyRegistry { return &StickyRegistry{} }

func (r *StickyRegistry) isSticky(key stickyKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[key].sticky
}

func (r *StickyRegistry) markSticky(key stickyKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set(key, stickyState{sticky: true})
}

func (r *StickyRegistry) recordFailure(key stickyKey) (becameSticky bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[key]
	if state.sticky {
		return false
	}
	state.failures++
	state.sticky = state.failures >= 2
	r.set(key, state)
	return state.sticky
}

func (r *StickyRegistry) recordSuccess(key stickyKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.states[key].sticky {
		delete(r.states, key)
	}
}

// set is called with r.mu held.
func (r *StickyRegistry) set(key stickyKey, state stickyState) {
	if r.states == nil {
		r.states = make(map[stickyKey]stickyState)
	}
	r.states[key] = state
}

// Stats counts requests, handshakes and recoveries over the client's lifetime.
type Stats struct {
	WSDials, WSRequests, WSIncremental      uint64
	SSERequests, SSEFallbacks               uint64
	StaleChainRecoveries, IdleSocketRetries uint64
}

type clientStats struct {
	wsDials, wsRequests, wsIncremental      atomic.Uint64
	sseRequests, sseFallbacks               atomic.Uint64
	staleChainRecoveries, idleSocketRetries atomic.Uint64
}

// Stats reads the client's transport counters.
func (c *Client) Stats() Stats {
	return Stats{
		WSDials: c.stats.wsDials.Load(), WSRequests: c.stats.wsRequests.Load(),
		WSIncremental: c.stats.wsIncremental.Load(), SSERequests: c.stats.sseRequests.Load(),
		SSEFallbacks: c.stats.sseFallbacks.Load(), StaleChainRecoveries: c.stats.staleChainRecoveries.Load(),
		IdleSocketRetries: c.stats.idleSocketRetries.Load(),
	}
}

type wsConnState struct {
	conn           *websocket.Conn
	authGen        uint64
	upgradeHeaders map[string]string // canonical, including session-id and User-Agent
	maxMsgBytes    int
	lastUsed       time.Time
	lastRespID     string
	lastProps      []byte
	lastChainItems [][]byte
	turnState      string
}

func (c *Client) isSticky() bool {
	return c.transport == TransportAuto && c.sticky.isSticky(c.stickyKey)
}

// PreDial opens the WebSocket in the background without sending a request.
func (c *Client) PreDial(sessionID string) {
	c.wsMu.Lock()
	if c.closed || c.transport == TransportSSE || c.isSticky() || c.ws != nil || c.inFlight || c.preDialing {
		c.wsMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.dialTimeout)
	done := make(chan struct{})
	c.preDialing, c.preDialCancel, c.preDialDone = true, cancel, done
	c.wsMu.Unlock()

	go func() {
		defer func() {
			cancel()
			c.wsMu.Lock()
			c.preDialing, c.preDialCancel, c.preDialDone = false, nil, nil
			close(done)
			c.wsMu.Unlock()
		}()
		token, gen, err := c.wsToken(ctx)
		if err != nil {
			return
		}
		var req Request
		if c.route == RouteChatGPT && sessionID != "" {
			req.Headers = map[string]string{"Session-Id": sessionID}
		}
		lease, err := c.dialWS(ctx, token, gen, c.upgradeHeaders(req))
		if err != nil {
			return
		}
		c.wsMu.Lock()
		publish := !c.closed && c.ws == nil && !c.inFlight
		if publish {
			c.ws = lease
		}
		c.wsMu.Unlock()
		if !publish {
			_ = lease.conn.CloseNow()
		}
	}()
}

// Close stops new WebSocket use and joins any background pre-dial. An active
// stream owns its connection until completion, failure or Stream.Close.
func (c *Client) Close() error {
	c.wsMu.Lock()
	c.closed = true
	cancel, done := c.preDialCancel, c.preDialDone
	var idle *wsConnState
	if !c.inFlight {
		idle, c.ws = c.ws, nil
	}
	c.wsMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if idle != nil {
		_ = idle.conn.CloseNow()
	}
	return nil
}

// reserveWS waits for a pending pre-dial and then reserves the single WS lane.
// A reservation can own a nil lease while it is dialing.
func (c *Client) reserveWS(ctx context.Context) (lease *wsConnState, reserved, sticky bool, err error) {
	for {
		c.wsMu.Lock()
		switch {
		case ctx.Err() != nil:
			c.wsMu.Unlock()
			return nil, false, false, ctx.Err()
		case c.isSticky():
			c.wsMu.Unlock()
			return nil, false, true, nil
		case c.closed || c.inFlight:
			c.wsMu.Unlock()
			return nil, false, false, nil
		case !c.preDialing:
			lease = c.ws
			c.inFlight = true
			c.wsMu.Unlock()
			return lease, true, false, nil
		}
		done := c.preDialDone
		c.wsMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, false, false, ctx.Err()
		}
	}
}

// replaceWS belongs to the foreground reservation. It keeps the lane reserved
// while an old connection is dropped or a new one is installed.
func (c *Client) replaceWS(lease *wsConnState) {
	c.wsMu.Lock()
	old := c.ws
	c.ws = lease
	c.wsMu.Unlock()
	if old != nil {
		_ = old.conn.CloseNow()
	}
}

func (c *Client) releaseWS(lease *wsConnState, keep bool) {
	c.wsMu.Lock()
	if c.ws != lease {
		c.wsMu.Unlock()
		return
	}
	keep = keep && !c.closed
	if !keep {
		c.ws = nil
	}
	c.inFlight = false
	c.wsMu.Unlock()
	if !keep && lease != nil {
		_ = lease.conn.CloseNow()
	}
}

func (c *Client) upgradeHeaders(req Request) map[string]string {
	h := make(map[string]string)
	for _, key := range slices.Sorted(maps.Keys(req.Headers)) {
		k := http.CanonicalHeaderKey(key)
		if protectedHeaders[k] || strings.HasPrefix(k, "Sec-Websocket-") || k == "Upgrade" || k == "Openai-Beta" {
			continue
		}
		h[k] = req.Headers[key]
	}
	h["User-Agent"] = cmp.Or(req.UserAgent, c.userAgent)
	return h
}

func (c *Client) wsToken(ctx context.Context) (string, uint64, error) {
	token, gen, err := c.creds.Token(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("responsesapi: no token for the request: %w", err)
	}
	if token == "" {
		return "", 0, errors.New("responsesapi: the credential source gave an empty token")
	}
	return token, gen, nil
}

func (c *Client) dialWS(ctx context.Context, token string, gen uint64, headers map[string]string) (lease *wsConnState, err error) {
	h := make(http.Header)
	for k, v := range headers {
		h.Set(k, v)
	}
	h.Set("Authorization", "Bearer "+token)
	h.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	dctx, cancel := context.WithTimeout(ctx, c.dialTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dctx, c.wsEndpoint, &websocket.DialOptions{
		HTTPClient: c.wsHTTP, HTTPHeader: h, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			e := statusError(resp)
			e.Route = c.route
			return nil, e
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, wsIOError(ctx, dctx, "dial", err)
	}
	defer func() {
		if lease == nil {
			_ = conn.CloseNow()
		}
	}()
	c.stats.wsDials.Add(1)
	conn.SetReadLimit(wsReadLimit)
	maxBytes, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-Codex-Websocket-Max-Message-Bytes")))
	if err != nil || maxBytes <= 0 {
		maxBytes = wsMessageLimit
	}
	return &wsConnState{conn: conn, authGen: gen, upgradeHeaders: headers, maxMsgBytes: maxBytes, lastUsed: c.now()}, nil
}

func wsIOError(ctx, ioCtx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var timeout net.Error
	if ioCtx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &timeout) && timeout.Timeout()) {
		return wsTimeoutError{operation: op}
	}
	return err
}

type wsFrameSource struct {
	conn    *websocket.Conn
	timeout time.Duration
	pending frame
}

func (s *wsFrameSource) next(ctx context.Context) (frame, error) {
	if s.pending.data != nil {
		f := s.pending
		s.pending = frame{}
		return f, nil
	}
	rctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	f, err := s.read(ctx, rctx)
	var timeout wsTimeoutError
	if errors.As(err, &timeout) {
		timeout.incomplete = true
		return frame{}, timeout
	}
	return f, err
}

func (s *wsFrameSource) close() error { return s.conn.CloseNow() }

func (s *wsFrameSource) read(ctx, rctx context.Context) (frame, error) {
	typ, data, err := s.conn.Read(rctx)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return frame{}, ctx.Err()
		case errors.Is(err, websocket.ErrMessageTooBig):
			return frame{}, errEventTooLarge
		case websocket.CloseStatus(err) == websocket.StatusMessageTooBig:
			return frame{}, ErrWSMessageTooBig
		}
		var timeout wsTimeoutError
		if err = wsIOError(ctx, rctx, "read", err); errors.As(err, &timeout) {
			return frame{}, err
		}
		return frame{}, ErrIncomplete
	}
	if typ != websocket.MessageText {
		return frame{}, errUnexpectedBinaryFrame
	}
	return frame{data: data}, nil
}

func (lease *wsConnState) request(w builtWireRequest, route Route) wireWSRequest {
	req := wireWSRequest{Type: "response.create", wireRequest: w.wireRequest}
	if lease.lastRespID != "" && bytes.Equal(w.wsProps, lease.lastProps) &&
		len(w.fullInputJSON) > len(lease.lastChainItems) &&
		slices.EqualFunc(w.fullInputJSON[:len(lease.lastChainItems)], lease.lastChainItems, bytes.Equal) {
		req.PreviousResponseID = lease.lastRespID
		req.Input = req.Input[len(lease.lastChainItems):]
	}
	if req.PreviousResponseID == "" || slices.ContainsFunc(req.Input, func(it Item) bool { return it.kind == userMessage }) {
		lease.turnState = ""
	}
	if route == RouteChatGPT && lease.turnState != "" {
		req.ClientMetadata = map[string]string{turnStateHeader: lease.turnState}
	}
	return req
}

func (c *Client) storeChain(lease *wsConnState, req builtWireRequest, s *Stream) {
	lease.lastRespID = ""
	lease.lastProps = nil
	lease.lastChainItems = nil
	lease.turnState = cmp.Or(s.turnState, lease.turnState)
	if c.replayOutput == nil || s.nonPrefixDelta || s.indexGap {
		return
	}
	items, ok := c.replayOutput(s.doneItems)
	if !ok || s.responseID == "" {
		return
	}
	if c.route == RouteKey {
		items = keyItems(items)
	}
	replay, err := encodeItems(items)
	if err != nil {
		return
	}
	lease.lastRespID = s.responseID
	lease.lastProps = req.wsProps
	lease.lastChainItems = append(req.fullInputJSON, replay...)
}

type startupFailure int

const (
	startupProtocol startupFailure = iota
	startupDial
	startupTransport
	startupOversize
	startupPeerLimit
)

func (c *Client) streamWS(ctx context.Context, req Request) (*Stream, bool, error) {
	if c.isSticky() {
		return nil, true, nil
	}
	lease, reserved, sticky, err := c.reserveWS(ctx)
	if !reserved {
		return nil, sticky, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			c.releaseWS(lease, false)
		}
	}()

	w, err := buildWireRequest(req, c.route, c.devInst)
	if err != nil {
		return nil, false, err
	}
	headers := c.upgradeHeaders(req)
	var recovery startupRecovery
	for {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if lease != nil && c.now().Sub(lease.lastUsed) > c.maxIdleAge {
			c.replaceWS(nil)
			lease = nil
		}
		token, gen, err := c.wsToken(ctx)
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if err != nil {
			return nil, false, err
		}
		if lease != nil && (lease.authGen != gen || !maps.Equal(lease.upgradeHeaders, headers)) {
			c.replaceWS(nil)
			lease = nil
		}

		reused := lease != nil
		if !reused {
			lease, err = c.dialWS(ctx, token, gen, headers)
			if err == nil {
				c.replaceWS(lease)
			}
		}
		var s *Stream
		var sentPrev bool
		kind := startupDial
		if err == nil {
			s, sentPrev, kind, err = c.startWS(ctx, w, lease, reused)
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if err == nil {
			c.bindStream(lease, w, s)
			handedOff = true
			return s, false, nil
		}
		retry, renew := recovery.retry(c, err, sentPrev, reused, kind)
		if !retry {
			if c.fallbackWS(kind, err) {
				return nil, true, nil
			}
			return nil, false, err
		}
		c.replaceWS(nil)
		lease = nil
		if renew {
			err := c.creds.Invalidate(ctx, gen)
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			if err != nil {
				return nil, false, fmt.Errorf("responsesapi: the server refused the token, and it could not be renewed: %w", err)
			}
		}
	}
}

func (c *Client) startWS(ctx context.Context, w builtWireRequest, lease *wsConnState, reused bool) (*Stream, bool, startupFailure, error) {
	req := lease.request(w, c.route)
	sentPrev := req.PreviousResponseID != ""
	data, err := encode(req)
	if err != nil {
		return nil, sentPrev, startupProtocol, err
	}
	if len(data) > lease.maxMsgBytes {
		return nil, sentPrev, startupOversize, ErrWSMessageTooBig
	}
	if err := c.writeWS(ctx, lease.conn, data); err != nil {
		if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
			return nil, sentPrev, startupPeerLimit, ErrWSMessageTooBig
		}
		return nil, sentPrev, startupTransport, err
	}
	c.stats.wsRequests.Add(1)
	if sentPrev {
		c.stats.wsIncremental.Add(1)
	}
	source := &wsFrameSource{conn: lease.conn, timeout: c.idleReadTimeout}
	s := newFrameStream(ctx, source)
	s.route, s.ws = c.route, true
	timeout := c.idleReadTimeout
	if reused {
		timeout = c.firstFrameTimeout
	}
	kind, err := peekWS(ctx, source, s, timeout)
	return s, sentPrev, kind, err
}

func (c *Client) writeWS(ctx context.Context, conn *websocket.Conn, data []byte) error {
	wctx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()
	err := conn.Write(wctx, websocket.MessageText, data)
	if err != nil {
		return wsIOError(ctx, wctx, "write", err)
	}
	return nil
}

// peekWS uses one deadline across startup metadata and the first response
// event. Only the boundary frame needs buffering.
func peekWS(ctx context.Context, source *wsFrameSource, s *Stream, timeout time.Duration) (startupFailure, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		f, err := source.read(ctx, rctx)
		if err != nil {
			var timeout wsTimeoutError
			switch {
			case errors.Is(err, ErrWSMessageTooBig):
				return startupPeerLimit, err
			case errors.Is(err, ErrIncomplete), errors.As(err, &timeout):
				return startupTransport, err
			default:
				return startupProtocol, err
			}
		}
		if string(f.data) == "[DONE]" {
			return startupTransport, ErrIncomplete
		}
		typ, err := f.eventType()
		if err != nil {
			return startupProtocol, err
		}
		switch {
		case typ == "error":
			s.readEvent(typ, f.data)
			return startupProtocol, s.err
		case typ == "response.created", handled[typ], strings.HasPrefix(typ, "response.output"):
			source.pending = f
			return startupProtocol, nil
		default:
			s.readEvent(typ, f.data)
		}
	}
}

type startupRecovery struct {
	auth, stale, limit, idle bool
}

func (r *startupRecovery) retry(c *Client, err error, sentPrev, reused bool, kind startupFailure) (retry, renew bool) {
	var e *Error
	if errors.As(err, &e) {
		switch {
		case c.route == RouteChatGPT && !r.auth && e.StatusCode == http.StatusUnauthorized:
			r.auth = true
			return true, true
		case !r.stale && isStalePreviousResponse(e, sentPrev):
			r.stale = true
			c.stats.staleChainRecoveries.Add(1)
			return true, false
		case !r.limit && e.Code == "websocket_connection_limit_reached":
			r.limit = true
			return true, false
		}
	}
	if !r.idle && reused && kind == startupTransport {
		r.idle = true
		c.stats.idleSocketRetries.Add(1)
		return true, false
	}
	return false, false
}

func (c *Client) fallbackWS(kind startupFailure, err error) bool {
	if c.transport != TransportAuto {
		return false
	}
	switch kind {
	case startupOversize:
		return true
	case startupPeerLimit:
		c.sticky.markSticky(c.stickyKey)
		return true
	case startupDial:
		var e *Error
		if errors.As(err, &e) && stickyDialStatus(e.StatusCode) {
			c.sticky.markSticky(c.stickyKey)
		} else {
			c.sticky.recordFailure(c.stickyKey)
		}
		return true
	case startupTransport:
		c.sticky.recordFailure(c.stickyKey)
		return true
	default:
		return false
	}
}

func stickyDialStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusUpgradeRequired, http.StatusNotImplemented:
		return true
	default:
		return status >= 300 && status < 400
	}
}

func (c *Client) bindStream(lease *wsConnState, req builtWireRequest, s *Stream) {
	s.onFinish = func(completed bool, err error) {
		keep := false
		defer func() { c.releaseWS(lease, keep) }()
		if completed {
			c.storeChain(lease, req, s)
			lease.lastUsed = c.now()
			c.sticky.recordSuccess(c.stickyKey)
			keep = true
		} else if c.transport == TransportAuto && s.ctx.Err() == nil {
			switch {
			case errors.Is(err, ErrWSMessageTooBig):
				c.sticky.markSticky(c.stickyKey)
			case errors.Is(err, ErrIncomplete):
				c.sticky.recordFailure(c.stickyKey)
			}
		}
	}
}
