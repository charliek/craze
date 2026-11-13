package messagesapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// redacted is what a scrubbed key reads as.
const redacted = "[redacted]"

// secret is the API key a request carries, held as the pattern that finds
// it in text (keyPattern), so no error this package returns carries it
// (P13): a server that echoes the key in an error's message, a request id
// that holds it, a transport or a body that names it. ListModels runs
// outside the model wrapper's scrubber, so the client takes it out itself.
// A nil secret hides nothing. The key is matched plain, JSON-escaped or
// percent-encoded, with format characters or invalid bytes between its
// characters (KeyPattern); a server that puts other characters inside it (a
// NUL, a space) is not matched — an accepted limit: those do not draw as
// nothing.
type secret struct{ re *regexp.Regexp }

func newSecret(key string) *secret {
	if key == "" {
		return nil
	}
	return &secret{re: KeyPattern(key)}
}

// text is v with every spelling of the key replaced by redacted, format
// characters and invalid bytes inside it included (KeyGap). A message shown
// to a person is made valid text before it (valid), so nothing dropped after
// the scrub can join a key back together (review c1r2 1).
func (s *secret) text(v string) string {
	if s == nil {
		return v
	}
	return s.re.ReplaceAllLiteralString(v, redacted)
}

// valid is v as text a scrub sees whole: invalid UTF-8 dropped, and so is
// U+FFFD, which a JSON decoder puts in an invalid byte's place — either,
// left inside a key, would hide it from the scrub and could be taken out
// after it.
func valid(v string) string {
	v = strings.ToValidUTF8(v, "")
	return strings.ReplaceAll(v, string(utf8.RuneError), "")
}

// classes are the failures a caller tells a transport's errors apart by:
// the context's, and a stream cut short or ended. A scrubbed error unwraps to
// the one its cause is, and to nothing else.
var classes = []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF, io.EOF}

// err is err as an error that cannot carry the key (P13): its text made
// valid and scrubbed, and never its cause — whether or not the text held
// the key, since a cause's fields can hold what its text does not show (a
// *url.Error's URL, a transport's own error type; review c1r2 2). What a
// caller classifies a failure by is kept without it: the class the cause is
// (classes), which the error unwraps to; for a net.Error, Timeout; and
// whether it was a transport's failure (Transport), read from the cause
// before it is dropped. One of the classes, itself, is returned as it is.
func (s *secret) err(err error) error {
	if err == nil || slices.Contains(classes, err) {
		return err
	}
	e := &scrubbedError{msg: s.text(valid(err.Error())), transport: transportFailure(err)}
	for _, c := range classes {
		if errors.Is(err, c) {
			e.class = c
			break
		}
	}
	if ne, ok := errors.AsType[net.Error](err); ok {
		return &scrubbedNetError{scrubbedError: e, timeout: ne.Timeout()}
	}
	return e
}

// scrubbedError is an error rebuilt from another's scrubbed text: it
// unwraps to the other's class (classes), or to nothing.
type scrubbedError struct {
	msg       string
	class     error
	transport bool
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.class }

// Transport reports whether the failure was the transport's — a network
// error, an HTTP/2 stream reset, connection error or GOAWAY (transportFailure)
// — which a caller may try again before any output, as Fantasy retries
// them: what the cause's own type said, kept now the cause is not (review
// c1r3 2).
func (e *scrubbedError) Transport() bool { return e.transport }

// IsTransport reports whether err is, or wraps, a failure this package
// scrubbed that was the transport's (scrubbedError.Transport).
func IsTransport(err error) bool {
	t, ok := errors.AsType[interface {
		error
		Transport() bool
	}](err)
	return ok && t.Transport()
}

// transportFragments are the words of an HTTP/2 transport failure's text:
// a stream reset, a connection error (what Fantasy's IsTransportError
// reads), and a GOAWAY, whose own text has neither.
var transportFragments = []string{"stream error:", "connection error:", "GOAWAY"}

// transportFailure reports whether err, as the transport returned it, is a
// transport's failure: a net.Error anywhere in its chain (Fantasy retries
// those), an error of an HTTP/2 package's type anywhere in it (http2Type),
// or text with transportFragments' words. It reads the cause itself, before
// the scrub, so a key that is one of those words changes nothing.
func transportFailure(err error) bool {
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	msg := err.Error()
	for _, f := range transportFragments {
		if strings.Contains(msg, f) {
			return true
		}
	}
	return inChain(err, http2Type)
}

// http2Type reports whether err's type is an HTTP/2 package's: any package
// whose path ends in /http2 — the standard library's own since Go 1.27
// (net/http/internal/http2), and golang.org/x/net/http2 — or the copy
// earlier standard libraries bundle into net/http, whose types are
// unexported there with an http2 prefix (http2StreamError,
// http2GoAwayError). This package can import none of them, so it reads the
// type's package and name.
func http2Type(err error) bool {
	t := reflect.TypeOf(err)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t != nil && isHTTP2Type(t.PkgPath(), t.Name())
}

func isHTTP2Type(pkg, name string) bool {
	return (pkg == "net/http" && strings.HasPrefix(name, "http2")) || strings.HasSuffix(pkg, "/http2")
}

// maxChain bounds inChain's walk: a chain that long is a loop.
const maxChain = 64

// inChain reports whether f holds for err or any error it wraps, through
// Unwrap() error and Unwrap() []error alike.
func inChain(err error, f func(error) bool) bool {
	return walk(err, f, 0)
}

func walk(err error, f func(error) bool, depth int) bool {
	for ; err != nil && depth < maxChain; depth++ {
		if f(err) {
			return true
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				if walk(e, f, depth+1) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// scrubbedNetError is a scrubbedError of a net.Error, which it still is: a
// timeout is a timeout.
type scrubbedNetError struct {
	*scrubbedError
	timeout bool
}

func (e *scrubbedNetError) Timeout() bool { return e.timeout }

// Temporary is part of net.Error though deprecated there; nothing reads it.
func (e *scrubbedNetError) Temporary() bool { return false }

// jsonEscapes are the escapes a JSON string has for a character besides
// \u: RFC 8259 §7.
var jsonEscapes = map[rune]string{
	'"': `\"`, '\\': `\\`, '/': `\/`, '\b': `\b`, '\f': `\f`, '\n': `\n`, '\r': `\r`, '\t': `\t`,
}

// KeyPattern matches key as itself and in every spelling an error's text
// can give it, character by character: as it is, as a JSON \u escape (hex in
// either case, a surrogate pair beyond the BMP), as its short JSON escape
// where it has one, or percent-encoded (hex in either case) — so a server
// that echoed the key escaped, in any subset of its characters, has not
// slipped it past (the rule of the model wrapper's keyPattern, llm/scrub.go,
// which this package may not import). A key that is not valid UTF-8 has its
// bad bytes stand for U+FFFD, which the pattern matches in place of each,
// along with each byte's own percent-encoding (%FF), as a URL escapes it.
//
// Between two of its characters it also matches any run of KeyGap: format
// characters and invalid bytes, which a display drops, joining the key
// again — so the whole key, separators and all, is what a scrub replaces,
// before anything cuts the text. It is this client's scrub, and the one the
// Anthropic model list's cache (internal/anthropicmodels), which runs outside
// it, takes the same key out of its own errors with.
func KeyPattern(key string) *regexp.Regexp {
	var b strings.Builder
	for i, r := range key {
		if i > 0 {
			b.WriteString(KeyGap)
		}
		b.WriteString("(?:" + regexp.QuoteMeta(string(r)))
		if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
			fmt.Fprintf(&b, `|\\u(?i:%04x)\\u(?i:%04x)`, r1, r2)
		} else {
			fmt.Fprintf(&b, `|\\u(?i:%04x)`, r)
		}
		if e, ok := jsonEscapes[r]; ok {
			b.WriteString("|" + regexp.QuoteMeta(e))
		}
		b.WriteString("|")
		for _, c := range []byte(string(r)) {
			fmt.Fprintf(&b, `%%(?i:%02x)`, c)
		}
		if _, size := utf8.DecodeRuneInString(key[i:]); r == utf8.RuneError && size == 1 {
			// An invalid byte percent-encodes as itself (%FF), not as the
			// U+FFFD it is read as.
			fmt.Fprintf(&b, `|%%(?i:%02x)`, key[i])
		}
		b.WriteString(")")
	}
	return regexp.MustCompile(b.String())
}

// KeyGap is what KeyPattern lets stand between two of a key's characters:
// format characters (Unicode Cf: a zero-width space, a word joiner, a
// byte-order mark, a soft hyphen, a bidi control) and invalid bytes (read as
// U+FFFD), which draw as nothing once a display drops them. The model
// wrapper's own keyPattern (llm/scrub.go) takes its gap from here, so every
// scrub of a key reads the same characters as nothing.
const KeyGap = `[\p{Cf}\x{FFFD}]*`
