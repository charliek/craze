package websearch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// The MCP caller (plan 051 §B): not an MCP client. One function, call, POSTs
// a JSON-RPC tools/call to a streamable-HTTP MCP endpoint and returns the
// text of the result — no initialize, no session, no tools/list, no stdio. A
// general MCP client is its own later plan (D-18), which can adopt this.

// MaxReply is the most of a reply the caller reads, in bytes: a reply over it
// is an output_limit failure, never a success cut short.
const MaxReply = 256 << 10

// CallTimeout bounds one call to the provider: the request and the whole
// reply. It covers the provider call only, never time spent elsewhere (a
// person answering a question, later).
const CallTimeout = 25 * time.Second

// userAgent names craze to the provider, as the Messages API client does; the
// harness may not import internal/version, so it carries no version.
const userAgent = "craze"

// maxDetail is the most of a provider's own words, in bytes, a failure keeps
// for the model (failure.detail).
const maxDetail = 200

// failKind is what went wrong with a call, for the service's words for it.
type failKind int

const (
	// failOther is everything not below: a transport failure, a reply that
	// is not a JSON-RPC answer to the call, a JSON-RPC error, a provider
	// error that names no status, a stream that ended unanswered.
	failOther failKind = iota
	failStatus
	failTimeout
	failAborted
	failTooLarge
)

// failure is a failed call. It holds no cause and no text of the request or
// its transport: only the kind, the HTTP status (a reply's, or the one a
// provider's error names), and detail, the provider's own words — a JSON-RPC
// error's message or an error result's text, scrubbed of the key before it
// was cut (scrubber) — when the provider gave any and named no status.
type failure struct {
	kind   failKind
	status int
	detail string
}

func (f *failure) Error() string {
	switch f.kind {
	case failStatus:
		return fmt.Sprintf("websearch: HTTP %d", f.status)
	case failTimeout:
		return "websearch: timed out"
	case failAborted:
		return "websearch: aborted"
	case failTooLarge:
		return "websearch: reply too large"
	}
	if f.detail != "" {
		return "websearch: call failed: " + f.detail
	}
	return "websearch: call failed"
}

// caller calls tools on one MCP endpoint. It is safe for concurrent use.
type caller struct {
	endpoint string
	// key is sent in x-api-key when not "": the service has already judged
	// it usable and the endpoint the provider's own.
	key     string
	scrub   *scrubber
	http    *http.Client
	timeout time.Duration
	ids     atomic.Int64
}

// newCaller builds a caller of endpoint whose calls each last at most
// timeout. hc is copied with redirects refused: a 3xx is a failed call, never
// followed, so a key goes to the endpoint and nowhere else.
func newCaller(endpoint, key string, scrub *scrubber, hc *http.Client, timeout time.Duration) *caller {
	c := &http.Client{}
	if hc != nil {
		cp := *hc
		c = &cp
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &caller{endpoint: endpoint, key: key, scrub: scrub, http: c, timeout: timeout}
}

// call calls tool with args and returns the text of its result: its text
// content blocks joined in order (any other block ignored), with the key
// scrubbed out. Every failure is a *failure.
func (c *caller) call(ctx context.Context, tool string, args map[string]any) (string, error) {
	if ctx.Err() != nil {
		return "", &failure{kind: failAborted}
	}
	id := c.ids.Add(1)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return "", &failure{kind: failOther}
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	text, f := c.post(cctx, id, body)
	// The context decides, whatever the transport or the reader made of it,
	// a reply read whole included: the caller's own context ending is a
	// cancel, the call's deadline a timeout, never a success.
	switch {
	case ctx.Err() != nil:
		return "", &failure{kind: failAborted}
	case cctx.Err() != nil:
		return "", &failure{kind: failTimeout}
	case f != nil:
		return "", f
	}
	return text, nil
}

// post sends one request and reads its answer.
func (c *caller) post(ctx context.Context, id int64, body []byte) (string, *failure) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", &failure{kind: failOther}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", failureOf(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", &failure{kind: failStatus, status: resp.StatusCode}
	}
	r := &capped{r: resp.Body, left: MaxReply}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var env *envelope
	switch media {
	case "application/json":
		env, err = readJSON(r, id)
	case "text/event-stream":
		env, err = readSSE(r, id)
	default:
		return "", &failure{kind: failOther}
	}
	if err != nil {
		return "", failureOf(err)
	}
	return c.answer(env)
}

// failureOf is err, from the transport or a reader of the reply, as a
// failure: itself when it is one, a timeout when it is a net.Error that
// timed out, and failOther else. Never the error's text: a *url.Error names
// the endpoint.
func failureOf(err error) *failure {
	if f, ok := errors.AsType[*failure](err); ok {
		return f
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return &failure{kind: failTimeout}
	}
	return &failure{kind: failOther}
}

// envelope is a JSON-RPC response, as much of it as the caller reads.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
	// Method marks a request or notification the server sent on the
	// stream, which is not an answer.
	Method string `json:"method"`
}

// answerTo is the JSON-RPC message data holds when it answers the call id: a
// JSON-RPC 2.0 response (it carries a result or an error, and is no request
// of the server's) for that id. nil for anything else, JSON or not.
func answerTo(data []byte, id int64) *envelope {
	var env envelope
	if json.Unmarshal(data, &env) != nil || env.JSONRPC != "2.0" || env.Method != "" || (env.Result == nil && env.Error == nil) {
		return nil
	}
	var got int64
	if json.Unmarshal(env.ID, &got) != nil || got != id {
		return nil
	}
	return &env
}

// readJSON reads a reply sent as one JSON object, which must answer id.
func readJSON(r io.Reader, id int64) (*envelope, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if env := answerTo(data, id); env != nil {
		return env, nil
	}
	return nil, &failure{kind: failOther}
}

// readSSE reads a reply sent as server-sent events (the WHATWG rules): lines
// end in LF, CRLF or CR; a line starting with a colon is a comment; data
// lines are joined with LF; a blank line dispatches the event, and an event
// at the end of the stream with no blank line after it is never dispatched.
// The first "message" event whose data answers id is the answer; any other
// is skipped — a notification, a request of the server's, an answer to
// another id, or data that is not JSON. A stream that ends without one is a
// failure.
func readSSE(r io.Reader, id int64) (*envelope, error) {
	ls := &lines{br: bufio.NewReader(r)}
	var (
		data    strings.Builder
		hasData bool
		event   string
	)
	for {
		line, err := ls.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, &failure{kind: failOther}
			}
			return nil, err
		}
		if line == "" {
			if hasData && (event == "" || event == "message") {
				if env := answerTo([]byte(data.String()), id); env != nil {
					return env, nil
				}
			}
			data.Reset()
			hasData, event = false, ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData = true
		case "event":
			event = value
		}
	}
}

// lines reads a stream's lines.
type lines struct {
	br *bufio.Reader
	// afterCR is that the last line ended in a CR, so a LF that starts the
	// next read is the rest of that ending (CRLF), not an empty line.
	afterCR bool
}

// next reads one line with its ending (LF, CRLF or CR) taken off. A line cut
// off by the end of the stream is io.EOF: the event it belongs to was never
// finished. Bytes are read one at a time from the buffer, which is what lets
// a lone CR end a line; a reply is at most MaxReply bytes. A CR ends its line
// at once: whether a LF follows is learnt on the next read, never by waiting
// for the byte after the CR, which a server that keeps the stream open after
// its answer may never send.
func (l *lines) next() (string, error) {
	var b []byte
	for {
		c, err := l.br.ReadByte()
		if err != nil {
			return "", err
		}
		if l.afterCR {
			l.afterCR = false
			if c == '\n' {
				continue
			}
		}
		switch c {
		case '\n':
			return string(b), nil
		case '\r':
			l.afterCR = true
			return string(b), nil
		}
		b = append(b, c)
	}
}

// answer is the text of env's result, or the failure it reports.
func (c *caller) answer(env *envelope) (string, *failure) {
	if env.Error != nil && string(env.Error) != "null" {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(env.Error, &e)
		return "", &failure{kind: failOther, detail: detail(c.scrub.text(e.Message))}
	}
	if env.Result == nil || string(env.Result) == "null" {
		return "", &failure{kind: failOther}
	}
	// A result is an object with a content list, and a text block has a
	// string text: one missing either, or with null for it, is malformed —
	// a failure, never no results. An empty list is no results.
	var res struct {
		Content *[]struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil || res.Content == nil {
		return "", &failure{kind: failOther}
	}
	var b strings.Builder
	for _, block := range *res.Content {
		if block.Type != "text" {
			continue
		}
		if block.Text == nil {
			return "", &failure{kind: failOther}
		}
		b.WriteString(*block.Text)
	}
	// The key out of the whole text before anything reads it: a key split
	// across two blocks is whole here, and nothing has parsed or cut it yet.
	text := c.scrub.text(b.String())
	if res.IsError {
		if status, ok := statusOf(text); ok {
			return "", &failure{kind: failStatus, status: status}
		}
		return "", &failure{kind: failOther, detail: detail(text)}
	}
	return text, nil
}

// detail is a provider's message, already scrubbed, cut to its first line and
// maxDetail bytes.
func detail(msg string) string { return cut(firstLine(msg), maxDetail) }

// statusRE finds the HTTP status a provider's error text names, as Exa's do:
// "web_search_exa error (401): Invalid API key."
var statusRE = regexp.MustCompile(`\berror \(([1-5][0-9]{2})\)`)

// statusOf is the status text names, and whether it names one.
func statusOf(text string) (int, bool) {
	m := statusRE.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// firstLine is v up to its first line ending, trimmed.
func firstLine(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexAny(v, "\r\n"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// cut is v cut to at most n bytes on a rune boundary, with "…" when anything
// was cut.
func cut(v string, n int) string {
	if len(v) <= n {
		return v
	}
	i := n
	for i > 0 && !utf8.RuneStart(v[i]) {
		i--
	}
	return v[:i] + "…"
}

// capped reads at most left bytes from r and then fails with a failTooLarge
// failure, so a reply over the cap is a failure and never a reply cut short.
// The byte past the cap is never handed out: a reader of the reply sees the
// failure where that byte would be, so nothing it completes — an SSE event
// whose blank line ends there — can be read as an answer.
type capped struct {
	r    io.Reader
	left int64
	over bool
}

func (c *capped) Read(p []byte) (int, error) {
	if c.over {
		return 0, &failure{kind: failTooLarge}
	}
	// One byte past the cap: reading it is how a reply over the cap is
	// told from one exactly at it.
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.left {
		n, c.left, c.over = int(c.left), 0, true
		return n, &failure{kind: failTooLarge}
	}
	c.left -= int64(n)
	return n, err
}
