package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http/httputil"
	"regexp"
	"slices"
	"strings"

	"charm.land/fantasy"
)

// redacted is what a scrubbed key or URL query string reads as.
const redacted = "[redacted]"

// urlQuery matches a URL up to the end of its query string; group 1 is the
// part before the "?". A query string is where a key rides when a provider
// takes one as a parameter instead of a header, and a base URL can carry one
// (modeltable never echoes base_url for the same reason).
var urlQuery = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^\s?#"'<>]*)\?[^\s#"'<>]*`)

// scrubber removes the model's own API key, and any URL query string, from
// every error the model returns. Fantasy's errors carry far more than their
// message: a *fantasy.ProviderError from the OpenAI-compatible client holds
// the dumped request (Authorization header included), the response body (a
// provider may echo the header into a 401's message), the URL and the
// response headers, and its Cause is the SDK's error, which holds the
// *http.Request itself. Wrapping such an error would keep all of that
// reachable through errors.Unwrap and errors.As — one %+v or field read in a
// later layer from a leak — so every error is rebuilt from scrubbed values
// instead, keeping only what the harness and Fantasy's retry logic classify
// by.
//
// It holds the key only inside a strings.Replacer, reached through a pointer,
// so formatting a model with %+v prints an address, never the key.
type scrubber struct {
	key *strings.Replacer // nil when there is no key to hide
}

func newScrubber(key string) *scrubber {
	s := &scrubber{}
	if key != "" {
		s.key = strings.NewReplacer(key, redacted)
	}
	return s
}

// text scrubs one string: the key first, then every URL query string.
func (s *scrubber) text(v string) string {
	if s.key != nil {
		v = s.key.Replace(v)
	}
	return urlQuery.ReplaceAllString(v, "${1}?"+redacted)
}

func (s *scrubber) bytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return []byte(s.text(string(b)))
}

// headers scrubs a response header map's values and lowercases its keys.
// Lowercasing is a fix, not only hygiene: Fantasy looks up "retry-after-ms"
// and "retry-after" in lowercase (retry.go:29-49), but the OpenAI-compatible
// client builds this map from canonical keys and adds the lowercase ones by
// inserting into the map it is ranging over (providers/openai/error.go,
// toHeaderMap), which Go leaves to chance — about half of all errors lost
// the server's retry delay and fell back to the 5 s default. Keys are visited
// in sorted order so that two spellings of one header collapse the same way
// every time.
//
// Header names are never scrubbed: a server does not put a key in one, and
// a name that happened to contain the key's text would come out mangled
// ("retry-[redacted]-ms") and stop matching the lookups above.
func (s *scrubber) headers(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for _, k := range slices.Sorted(maps.Keys(h)) {
		out[strings.ToLower(k)] = s.text(h[k])
	}
	return out
}

// err returns err with nothing unscrubbed left reachable from it.
//
//   - The bare context.Canceled and context.DeadlineExceeded sentinels pass
//     through unchanged: they carry no text of their own, and callers compare
//     them by identity as well as with errors.Is.
//   - An error holding a *fantasy.ProviderError becomes a fresh one built
//     from scrubbed fields. The status code, the auth and context-too-large
//     classification, and retryability survive; the dumped request does not
//     (it is the Authorization header plus the whole conversation, and
//     nothing downstream reads it), and neither does the original cause.
//   - Anything else becomes an opaque error with the original's scrubbed
//     text. A net.Error stays a net.Error, because Fantasy retries one that
//     arrives before any output and the scrub must not change that.
//
// Whatever it builds unwraps only to a fixed-text sentinel found in the
// original chain (see sentinel) — a rebuilt ProviderError by way of the
// provider's names for the failure, when it sent any (providerNames), which
// hold no text but two identifiers — so errors.Is(err, context.Canceled) and
// Fantasy's own abort and EOF checks answer as they did before.
func (s *scrubber) err(err error) error {
	// Identity, not errors.Is: only the bare sentinel is known to carry
	// nothing to scrub.
	switch err {
	case nil, context.Canceled, context.DeadlineExceeded:
		return err
	}
	var pe *fantasy.ProviderError
	if errors.As(err, &pe) {
		return s.providerError(pe, sentinel(err))
	}
	se := &scrubbedError{msg: s.text(err.Error()), cause: sentinel(err)}
	var ne net.Error
	if errors.As(err, &ne) {
		return &scrubbedNetError{scrubbedError: se, timeout: ne.Timeout()}
	}
	return se
}

// providerError rebuilds pe from scrubbed fields. When pe was retryable for a
// reason the rebuild drops — its cause was an HTTP/2 transport error, say —
// TransientError carries the verdict over, so scrubbing never turns a retry
// into a failed turn. The provider's names for the failure, read from pe's
// response before it is scrubbed (names), ride in front of cause
// (providerNames), where ErrorNames finds them.
func (s *scrubber) providerError(pe *fantasy.ProviderError, cause error) *fantasy.ProviderError {
	if code, typ := s.names(pe.ResponseBody); code != "" || typ != "" {
		cause = &providerNames{code: code, typ: typ, cause: cause}
	}
	out := &fantasy.ProviderError{
		Title:              s.text(pe.Title),
		Message:            s.text(pe.Message),
		Cause:              cause,
		URL:                s.text(pe.URL),
		StatusCode:         pe.StatusCode,
		ResponseHeaders:    s.headers(pe.ResponseHeaders),
		ResponseBody:       s.bytes(pe.ResponseBody),
		ContextUsedTokens:  pe.ContextUsedTokens,
		ContextMaxTokens:   pe.ContextMaxTokens,
		ContextTooLargeErr: pe.ContextTooLargeErr,
		AuthError:          pe.AuthError,
		TransientError:     pe.TransientError,
	}
	if pe.IsRetryable() && !out.IsRetryable() {
		out.TransientError = true
	}
	return out
}

// midStream builds the error that replaces one arriving after output began
// (see MidStreamError), from scrubbed text and the original's classification.
func (s *scrubber) midStream(err error) *MidStreamError {
	me := &MidStreamError{Message: s.text(err.Error())}
	var pe *fantasy.ProviderError
	if errors.As(err, &pe) {
		me.StatusCode = pe.StatusCode
		me.AuthError = pe.AuthError
		me.contextTooLarge = pe.IsContextTooLarge()
		me.Code, me.Type = s.names(pe.ResponseBody)
	}
	return me
}

// errorName is what a provider's code or type for a failure must look like
// to be kept: a short lowercase identifier — "insufficient_quota",
// "rate_limit_exceeded", "server_error". Anything else names nothing craze
// decides by, and could be anything at all, the key included.
var errorName = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// names is the code and type of the structured error in raw, a
// *fantasy.ProviderError's response body as it arrived, unscrubbed
// (structuredError), each kept only when it is an errorName that the scrub
// leaves exactly as it is, and "" otherwise. They are read before the scrub
// because the scrub rewrites bytes: a key the provider sent JSON-escaped is
// not the bytes it looks for, and decoding a scrubbed body afterwards turned
// the escape back into the key (review r3 major 2); and a chunked body's
// scrub changes a chunk's length and not the size the chunk declares, so the
// scrubbed body no longer undoes its chunking and loses what followed. The
// identifier rule is what keeps reading the raw bytes safe: a key or a URL
// is never a short lowercase identifier, and the scrub's own pass catches a
// key that happens to be one.
func (s *scrubber) names(raw []byte) (code, typ string) {
	code, typ = structuredError(raw)
	return s.name(code), s.name(typ)
}

// name is v when it is an errorName the scrub leaves as it is, else "".
func (s *scrubber) name(v string) string {
	if !errorName.MatchString(v) || s.text(v) != v {
		return ""
	}
	return v
}

// providerNames carries the provider's code and type for a failure (names)
// on the *fantasy.ProviderError providerError rebuilds, as its Cause — the
// struct has no field for them — in front of the sentinel the rebuild keeps,
// which it unwraps to. It holds nothing but the two names, each an errorName
// the scrub passed, so it cannot carry the key, and its text is fixed, so
// Fantasy's transport check on a cause's text never matches it.
type providerNames struct {
	code, typ string
	cause     error // a sentinel, or nil
}

func (e *providerNames) Error() string { return "llm: the provider's code and type for the failure" }
func (e *providerNames) Unwrap() error { return e.cause }

// ErrorNames is the provider's own machine-readable code and type for the
// failure err carries — the OpenAI-family envelope's error.code and
// error.type, "insufficient_quota" for a quota that is gone — from a
// *MidStreamError or a *fantasy.ProviderError this package rebuilt, or "", ""
// when it sent none. Each was read from the response as it arrived and is
// "" unless it is a short lowercase identifier the scrub leaves as it is
// (names): what the provider says the failure is, where a message is text.
func ErrorNames(err error) (code, typ string) {
	var mse *MidStreamError
	if errors.As(err, &mse) {
		return mse.Code, mse.Type
	}
	var pn *providerNames
	if errors.As(err, &pn) {
		return pn.code, pn.typ
	}
	return "", ""
}

// structuredError is the code and type of the structured error in body, a
// *fantasy.ProviderError's response body: the OpenAI-family envelope,
// {"error": {"code": …, "type": …}}, which both of craze's drivers speak.
// body is either that envelope itself — a stream's in-band error event — or,
// for an HTTP error, the response dumped whole: status line and headers, a
// blank line, then the body, which may be chunked. A code or type that is
// not a string (OpenRouter's code is the HTTP status, a number) names
// nothing, and is "". Anything that does not parse is "", "".
func structuredError(body []byte) (code, typ string) {
	if !bytes.HasPrefix(body, []byte("HTTP/")) {
		code, typ, _ = errorEnvelope(body)
		return code, typ
	}
	_, rest, ok := bytes.Cut(body, []byte("\r\n\r\n"))
	if !ok {
		return "", ""
	}
	if code, typ, ok = errorEnvelope(rest); ok {
		return code, typ
	}
	// A chunked body leads with its first chunk's size: undo the chunking,
	// keeping whatever reads before an error.
	dechunked, _ := io.ReadAll(httputil.NewChunkedReader(bytes.NewReader(rest)))
	code, typ, _ = errorEnvelope(dechunked)
	return code, typ
}

// errorEnvelope decodes the first JSON value in b as the OpenAI-family error
// envelope, ignoring whatever follows it (a chunked body's trailer); ok is
// false when b does not start with one.
func errorEnvelope(b []byte) (code, typ string, ok bool) {
	var env struct {
		Error *struct {
			Code json.RawMessage `json:"code"`
			Type json.RawMessage `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&env); err != nil || env.Error == nil {
		return "", "", false
	}
	return jsonString(env.Error.Code), jsonString(env.Error.Type), true
}

// jsonString is raw as a string when it is a JSON string, else "".
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// sentinel returns the first fixed-text sentinel in err's chain that callers
// test for with errors.Is — cancellation, deadline, and the unexpected EOF
// that marks Fantasy's retryable incomplete stream — or nil. It is the only
// part of an original chain a scrubbed error keeps: a sentinel has no text
// but its own, so it cannot carry a key.
func sentinel(err error) error {
	for _, s := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		if errors.Is(err, s) {
			return s
		}
	}
	return nil
}

// scrubbedError is an error rebuilt from another's scrubbed text.
type scrubbedError struct {
	msg   string
	cause error // a sentinel, or nil
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.cause }

// scrubbedNetError is a scrubbedError that still satisfies net.Error.
type scrubbedNetError struct {
	*scrubbedError
	timeout bool
}

func (e *scrubbedNetError) Timeout() bool { return e.timeout }

// Temporary is part of net.Error though deprecated there; nothing reads it.
func (e *scrubbedNetError) Temporary() bool { return false }
