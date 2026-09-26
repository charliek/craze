package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
)

// leaks lists every place needle can be found from v: its formatted forms,
// and every string and byte slice reachable from it by reflection —
// unexported fields, causes, maps and slices included. Reflection is the
// point: an error that prints clean but still holds the original in a field
// leaks the moment a later layer reads that field or formats it with %#v.
func leaks(v any, needle string) []string { return findLeaks(v, needle, nil) }

// decodedLeaks is leaks for needle however JSON spells it, and split across
// the chunks of a dumped response's chunked body: every string and byte
// slice it looks in, formatted forms included, is read as a caller decoding
// it would (jsonDecoded) — review r4 major 1.
func decodedLeaks(v any, needle string) []string { return findLeaks(v, needle, jsonDecoded) }

func findLeaks(v any, needle string, decode func([]byte) []byte) []string {
	has := func(b []byte) bool {
		if decode != nil {
			b = decode(b)
		}
		return bytes.Contains(b, []byte(needle))
	}
	var found []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if has([]byte(fmt.Sprintf(verb, v))) {
			found = append(found, "formatted "+verb)
		}
	}
	w := &walker{has: has, seen: map[visit]bool{}}
	w.walk(reflect.ValueOf(v), "err")
	return append(found, w.found...)
}

// jsonEscape is one escape in a JSON string.
var jsonEscape = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}|[^u])`)

// jsonDecoded is b as a caller decoding it gets it back: a dumped response
// whose head says its body is chunked has the chunking undone, and every
// JSON escape is the character it stands for.
func jsonDecoded(b []byte) []byte {
	if head, body, ok := bytes.Cut(b, []byte("\r\n\r\n")); ok && bytes.HasPrefix(head, []byte("HTTP/")) &&
		bytes.Contains(bytes.ToLower(head), []byte("transfer-encoding: chunked")) {
		if d, err := io.ReadAll(httputil.NewChunkedReader(bytes.NewReader(body))); err == nil {
			b = slices.Concat(head, []byte("\r\n\r\n"), d)
		}
	}
	short := map[byte]string{'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t"}
	return jsonEscape.ReplaceAllFunc(b, func(e []byte) []byte {
		if e[1] != 'u' {
			if c, ok := short[e[1]]; ok {
				return []byte(c)
			}
			return e[1:]
		}
		n, _ := strconv.ParseUint(string(e[2:]), 16, 16)
		return utf8.AppendRune(nil, rune(n))
	})
}

type visit struct {
	ptr uintptr
	typ reflect.Type
}

type walker struct {
	has   func([]byte) bool
	seen  map[visit]bool
	found []string
}

func (w *walker) walk(v reflect.Value, path string) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if w.has([]byte(v.String())) {
			w.found = append(w.found, path)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if w.has(v.Bytes()) {
				w.found = append(w.found, path)
			}
			return
		}
		for i := range v.Len() {
			w.walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Array:
		for i := range v.Len() {
			w.walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		k := visit{v.Pointer(), v.Type()}
		if w.seen[k] {
			return
		}
		w.seen[k] = true
		w.walk(v.Elem(), path)
	case reflect.Interface:
		w.walk(v.Elem(), path)
	case reflect.Struct:
		for i := range v.NumField() {
			w.walk(v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			w.walk(it.Key(), path+"{key}")
			w.walk(it.Value(), fmt.Sprintf("%s[%v]", path, it.Key()))
		}
	}
}

// quietError holds a key in an unexported field that its text never shows.
type quietError struct{ secret []byte }

func (quietError) Error() string { return "quiet" }

// TestLeaksFindsAHiddenKey keeps leaks honest: a key in an unexported field
// of a wrapped cause, which no formatted form shows, must still be found.
func TestLeaksFindsAHiddenKey(t *testing.T) {
	err := fmt.Errorf("clean text: %w", quietError{secret: []byte(canary)})
	found := leaks(err, canary)
	if len(found) != 1 || found[0] != "err.err.secret" {
		t.Fatalf("leaks = %v, want exactly the hidden field err.err.secret", found)
	}
}

// TestDecodedLeaksFindsAnEscapedKey keeps decodedLeaks honest: a key
// JSON-escaped, or split across two chunks of a chunked dump, is not the
// key's bytes — leaks misses both — but decodedLeaks finds each.
func TestDecodedLeaksFindsAnEscapedKey(t *testing.T) {
	for name, secret := range map[string]string{
		"escaped": `{"m":"` + escapedEveryOther(canary) + `"}`,
		"split":   "HTTP/1.1 401 Unauthorized\r\nTransfer-Encoding: chunked\r\n\r\n" + chunked(canary[:5], canary[5:]),
	} {
		err := fmt.Errorf("clean text: %w", quietError{secret: []byte(secret)})
		if found := leaks(err, canary); len(found) != 0 {
			t.Fatalf("%s: test setup: leaks = %v, want nothing: the key's bytes are not there", name, found)
		}
		if found := decodedLeaks(err, canary); len(found) != 1 || found[0] != "err.err.secret" {
			t.Errorf("%s: decodedLeaks = %v, want exactly the hidden field err.err.secret", name, found)
		}
	}
}

// escapedEveryOther is v with every other character written as a JSON \u
// escape, the case of the hex digits alternating: a spelling of v that is
// not v's bytes.
func escapedEveryOther(v string) string {
	var b strings.Builder
	for i, r := range []rune(v) {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, `\u%04x`, r)
		case 2:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// chunked is parts as a chunked HTTP body, one chunk each, ended.
func chunked(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&b, "%x\r\n%s\r\n", len(p), p)
	}
	return b.String() + "0\r\n\r\n"
}

// dirtyProviderError is the shape the OpenAI-compatible client builds for a
// failed request whose response echoed the key, with every field that can
// carry it filled.
func dirtyProviderError() *fantasy.ProviderError {
	return &fantasy.ProviderError{
		Title:       "unauthorized",
		Message:     "bad key " + canary,
		Cause:       &url.Error{Op: "Post", URL: "https://api.example/v1?key=" + canary, Err: errors.New("401")},
		URL:         "https://api.example/v1/chat/completions?api-key=" + canary + "#frag",
		StatusCode:  401,
		RequestBody: []byte("POST /v1/chat/completions HTTP/1.1\r\nAuthorization: Bearer " + canary + "\r\n\r\n{}"),
		ResponseHeaders: map[string]string{
			"Retry-After-Ms": "1",
			"X-Echo":         "Bearer " + canary,
		},
		ResponseBody: []byte(`{"error":{"message":"bad key ` + canary + `"}}`),
	}
}

func TestScrubProviderErrorKeepsClassification(t *testing.T) {
	s := newScrubber(canary)
	with := func(edit func(*fantasy.ProviderError)) *fantasy.ProviderError {
		pe := dirtyProviderError()
		edit(pe)
		return pe
	}
	cases := []struct {
		name string
		in   *fantasy.ProviderError
	}{
		{"401", dirtyProviderError()},
		{"auth flag", with(func(pe *fantasy.ProviderError) { pe.StatusCode = 400; pe.AuthError = true })},
		{"context too large", with(func(pe *fantasy.ProviderError) {
			pe.StatusCode = 400
			pe.ContextTooLargeErr = true
			pe.ContextMaxTokens = 1000
			pe.ContextUsedTokens = 2000
		})},
		{"retryable status", with(func(pe *fantasy.ProviderError) { pe.StatusCode = 503 })},
		{"transient flag", with(func(pe *fantasy.ProviderError) { pe.StatusCode = 0; pe.TransientError = true })},
		{"x-should-retry header", with(func(pe *fantasy.ProviderError) { pe.StatusCode = 400; pe.ResponseHeaders["X-Should-Retry"] = "true" })},
		// Retryable only through the cause the rebuild drops: the verdict
		// must be carried over.
		{"transport cause", with(func(pe *fantasy.ProviderError) {
			pe.StatusCode = 0
			pe.Cause = errors.New("http2: stream error: stream ID 3; INTERNAL_ERROR")
		})},
		{"unexpected EOF cause", with(func(pe *fantasy.ProviderError) {
			pe.StatusCode = 0
			pe.Cause = fmt.Errorf("reading %s: %w", canary, io.ErrUnexpectedEOF)
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.err(tc.in)
			if found := leaks(err, canary); len(found) > 0 {
				t.Fatalf("the key is reachable at %v", found)
			}
			var out *fantasy.ProviderError
			if !errors.As(err, &out) {
				t.Fatalf("scrubbed error %#v is no longer a ProviderError", err)
			}
			if out.StatusCode != tc.in.StatusCode || out.AuthError != tc.in.AuthError ||
				out.IsContextTooLarge() != tc.in.IsContextTooLarge() || out.IsRetryable() != tc.in.IsRetryable() ||
				out.ContextMaxTokens != tc.in.ContextMaxTokens || out.ContextUsedTokens != tc.in.ContextUsedTokens {
				t.Errorf("classification changed: status %d→%d auth %v→%v tooLarge %v→%v retryable %v→%v",
					tc.in.StatusCode, out.StatusCode, tc.in.AuthError, out.AuthError,
					tc.in.IsContextTooLarge(), out.IsContextTooLarge(), tc.in.IsRetryable(), out.IsRetryable())
			}
			if errors.Is(tc.in, io.ErrUnexpectedEOF) != errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("errors.Is(io.ErrUnexpectedEOF) changed to %v", errors.Is(err, io.ErrUnexpectedEOF))
			}
			if out.RequestBody != nil {
				t.Errorf("RequestBody kept: %q", out.RequestBody)
			}
			if out.ResponseHeaders["retry-after-ms"] != "1" {
				t.Errorf("headers = %v, want retry-after-ms under its lowercase key", out.ResponseHeaders)
			}
			if want := "https://api.example/v1/chat/completions?" + redacted + "#frag"; out.URL != want {
				t.Errorf("URL = %q, want %q", out.URL, want)
			}
		})
	}
}

func TestScrubOtherErrors(t *testing.T) {
	s := newScrubber(canary)

	t.Run("bare cancellation passes through as itself", func(t *testing.T) {
		for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
			if got := s.err(sentinel); got != sentinel {
				t.Errorf("s.err(%v) = %#v, want the sentinel itself", sentinel, got)
			}
		}
	})
	t.Run("wrapped cancellation keeps errors.Is", func(t *testing.T) {
		err := s.err(fmt.Errorf("posting with %s: %w", canary, context.Canceled))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(context.Canceled) lost: %#v", err)
		}
		if found := leaks(err, canary); len(found) > 0 {
			t.Errorf("the key is reachable at %v", found)
		}
	})
	t.Run("net.Error stays a net.Error", func(t *testing.T) {
		in := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1/chat/completions?key=" + canary,
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: &timeoutError{}}}
		err := s.err(in)
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("scrubbed %#v is not a timing-out net.Error", err)
		}
		if found := leaks(err, canary); len(found) > 0 {
			t.Errorf("the key is reachable at %v", found)
		}
		if want := `Post "http://127.0.0.1:1/v1/chat/completions?` + redacted + `": dial tcp: i/o timeout`; err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
	})
	t.Run("anything else is opaque", func(t *testing.T) {
		err := s.err(fmt.Errorf("wrapping: %w", errors.New("key "+canary)))
		if found := leaks(err, canary); len(found) > 0 {
			t.Errorf("the key is reachable at %v", found)
		}
		if errors.Unwrap(err) != nil {
			t.Errorf("an opaque error unwraps to %#v", errors.Unwrap(err))
		}
		if err.Error() != "wrapping: key "+redacted {
			t.Errorf("Error() = %q", err.Error())
		}
	})
	t.Run("nil", func(t *testing.T) {
		if err := s.err(nil); err != nil {
			t.Errorf("s.err(nil) = %#v", err)
		}
	})
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func TestScrubText(t *testing.T) {
	s := newScrubber(canary)
	cases := []struct{ in, want string }{
		{"no secrets here", "no secrets here"},
		{"key " + canary + " twice " + canary, "key [redacted] twice [redacted]"},
		{"https://h.example/v1/chat/completions", "https://h.example/v1/chat/completions"},
		{`Post "https://h.example/v1?key=abc&x=1": EOF`, `Post "https://h.example/v1?[redacted]": EOF`},
		{"see http://a.example/?t=1 and HTTPS://B.example/p?q=2#top", "see http://a.example/?[redacted] and HTTPS://B.example/p?[redacted]#top"},
		{"a question? no url here", "a question? no url here"},
	}
	for _, tc := range cases {
		if got := s.text(tc.in); got != tc.want {
			t.Errorf("text(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := newScrubber("").text("key " + canary); got != "key "+canary {
		t.Errorf("a scrubber with no key changed %q to %q", "key "+canary, got)
	}
}

// review r4 major 1: the key is scrubbed in every spelling a JSON string can
// give it — any subset of its characters \u-escaped, the hex in either case,
// and a slash as \/ — and a spelling of anything else is left as it is.
func TestScrubTextFindsTheKeyInEveryJSONSpelling(t *testing.T) {
	const key = "sk-Ab/cd+ef/gh" // a slash and a plus, as a base64 key has
	s := newScrubber(key)
	lower := func(r rune) string { return fmt.Sprintf(`\u%04x`, r) }
	upper := func(r rune) string { return fmt.Sprintf(`\u%04X`, r) }
	every := func(spell func(rune) string) string {
		var b strings.Builder
		for _, r := range key {
			b.WriteString(spell(r))
		}
		return b.String()
	}
	for _, tc := range []struct{ name, in, want string }{
		{"as itself", "bad key " + key + "!", "bad key " + redacted + "!"},
		{"every character escaped, lower-case hex", "bad key " + every(lower), "bad key " + redacted},
		{"every character escaped, upper-case hex", every(upper) + ".", redacted + "."},
		{"the slashes escaped", `"sk-Ab\/cd+ef\/gh"`, `"` + redacted + `"`},
		{"a mix", "k=" + lower('s') + "k-A" + upper('b') + `\/cd` + lower('+') + "ef/g" + upper('h'), "k=" + redacted},
		{"twice", key + " " + every(lower), redacted + " " + redacted},
		{"escaped, but not the key", `sk-Ab\/cd+ef\/gX`, `sk-Ab\/cd+ef\/gX`},
		{"an escaped backslash, then a slash", `sk-Ab\\/cd+ef/gh`, `sk-Ab\\/cd+ef/gh`},
	} {
		if got := s.text(tc.in); got != tc.want {
			t.Errorf("%s: text(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// review r4 major 1: a provider that echoes the key JSON-escaped in its error
// body — part of it, here — has not sent the key's bytes, so scrubbing the
// body's bytes left that spelling in the rebuilt ProviderError's
// ResponseBody, where whatever decoded the body got the key back. Sized and
// chunked (a server that streams its error response cannot size it), no
// field of the error, its Error() included, decodes to the key; the body is
// still the JSON it was, the key redacted, inside a dump that still undoes
// its chunking; and the message reads as it did.
func TestAnEscapedKeyNeverSurvivesTheResponseBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chunked bool
	}{{"sized", false}, {"chunked", true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				if tc.chunked {
					w.(http.Flusher).Flush() // the header goes out unsized
				}
				auth := strings.ReplaceAll(r.Header.Get("Authorization"), canary, escapedEveryOther(canary))
				fmt.Fprintf(w, `{"error":{"message":"invalid credentials: %s","type":"invalid_request_error"}}`, auth)
			})
			agent := fantasy.NewAgent(wrapped(t, srv.URL), fantasy.WithMaxRetries(0))
			_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "go"})
			var pe *fantasy.ProviderError
			if !errors.As(err, &pe) || pe.StatusCode != http.StatusUnauthorized {
				t.Fatalf("err = %#v, want a ProviderError with status 401", err)
			}
			if got := bytes.Contains(pe.ResponseBody, []byte("Transfer-Encoding: chunked")); got != tc.chunked {
				t.Fatalf("test setup: the dump is chunked: %v, want %v: %q", got, tc.chunked, pe.ResponseBody)
			}
			if found := decodedLeaks(err, canary); len(found) > 0 {
				t.Fatalf("the key, decoded, is reachable from the error at %v", found)
			}
			head, body, _ := bytes.Cut(pe.ResponseBody, []byte("\r\n\r\n"))
			if tc.chunked {
				d, derr := io.ReadAll(httputil.NewChunkedReader(bytes.NewReader(body)))
				if derr != nil {
					t.Fatalf("the rebuilt body no longer undoes its chunking (%v): %q", derr, pe.ResponseBody)
				}
				body = d
			}
			want := `{"error":{"message":"invalid credentials: Bearer ` + redacted + `","type":"invalid_request_error"}}`
			if !bytes.HasPrefix(head, []byte("HTTP/1.1 401 ")) || string(body) != want {
				t.Errorf("ResponseBody = %q, want the dump with body %s", pe.ResponseBody, want)
			}
			if want := "invalid credentials: Bearer " + redacted; pe.Message != want {
				t.Errorf("Message = %q, want %q", pe.Message, want)
			}
		})
	}
}

// review r4 major 1: the rest of a JSON body — every other kind of value,
// member order, an object key, a URL whose slashes are escaped — and the
// bodies no HTTP fixture sends: an in-band error event, a message the SDK
// took from the raw body when it could not parse it (so Error() holds the
// escaped key too), and a key split across two chunks, which is no one run
// of bytes at all. No field of the rebuilt error decodes to the key or the
// query string.
func TestAnEscapedKeyNeverSurvivesAnyField(t *testing.T) {
	esc := escapedEveryOther(canary)
	const head = "HTTP/1.1 401 Unauthorized\r\nTransfer-Encoding: chunked\r\nContent-Type: application/json\r\n\r\n"
	split := `{"error":{"message":"bad key ` + canary + `"}}`
	at := strings.Index(split, canary) + 5
	for _, tc := range []struct {
		name, title, message, body, want, wantMessage string
	}{
		{
			name: "an in-band error event", title: "stream error", message: "bad key " + canary + " <a&b>",
			body: `{"error":{"message":"bad key ` + esc + ` <a&b>","param":"https:\/\/h.example\/v1?token=abc123",` +
				`"code":429,"n":-1.50e3,"ok":true,"no":false,"x":null,"list":["a",{"b":[]},{}],"` + esc + `":1}}` + "\n",
			want: `{"error":{"message":"bad key ` + redacted + ` <a&b>","param":"https://h.example/v1?` + redacted + `",` +
				`"code":429,"n":-1.50e3,"ok":true,"no":false,"x":null,"list":["a",{"b":[]},{}],"` + redacted + `":1}}`,
			wantMessage: "bad key " + redacted + " <a&b>",
		},
		{
			name: "the SDK's message is the raw body", title: "unauthorized", message: `{"detail":"bad key ` + esc + `"}`,
			body:        `{"detail":"bad key ` + esc + `"}`,
			want:        `{"detail":"bad key ` + redacted + `"}`,
			wantMessage: `{"detail":"bad key ` + redacted + `"}`,
		},
		{
			name: "a key split across two chunks", title: "unauthorized", message: "bad key " + canary,
			body:        head + chunked(split[:at], split[at:]),
			want:        head + chunked(`{"error":{"message":"bad key `+redacted+`"}}`),
			wantMessage: "bad key " + redacted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := &fantasy.ProviderError{Title: tc.title, Message: tc.message, StatusCode: 401, ResponseBody: []byte(tc.body)}
			err := newScrubber(canary).err(pe)
			for _, gone := range []string{canary, "abc123"} {
				if found := decodedLeaks(err, gone); len(found) > 0 {
					t.Fatalf("%q, decoded, is reachable from the error at %v", gone, found)
				}
			}
			var out *fantasy.ProviderError
			if !errors.As(err, &out) {
				t.Fatalf("scrubbed error %#v is no longer a ProviderError", err)
			}
			if string(out.ResponseBody) != tc.want {
				t.Errorf("ResponseBody = %s, want %s", out.ResponseBody, tc.want)
			}
			if out.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", out.Message, tc.wantMessage)
			}
		})
	}
}

// A body that is not one JSON value is scrubbed as bytes, as every body was
// before review r4 major 1: text, an HTML page sized or chunked, two JSON
// values, a JSON value cut short.
func TestANonJSONBodyIsScrubbedAsBytesAsBefore(t *testing.T) {
	const page = "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/html\r\n"
	for _, tc := range []struct{ name, body, want string }{
		{"text", "upstream refused key " + canary + " at https://h.example/v1?token=abc",
			"upstream refused key " + redacted + " at https://h.example/v1?" + redacted},
		{"an HTML page", page + "\r\n<html>bad key " + canary + "</html>", ""},
		{"a chunked HTML page", page + "Transfer-Encoding: chunked\r\n\r\n" + chunked("<html>bad key "+canary+"</html>"), ""},
		{"two JSON values", `{"error":{"message":"a"}} {"error":{"message":"bad key ` + canary + `"}}`, ""},
		{"a JSON value cut short", `{"error":{"message":"bad key ` + canary + `"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = strings.ReplaceAll(tc.body, canary, redacted)
			}
			pe := &fantasy.ProviderError{Title: "t", Message: "m", StatusCode: 502, ResponseBody: []byte(tc.body)}
			var out *fantasy.ProviderError
			if err := newScrubber(canary).err(pe); !errors.As(err, &out) {
				t.Fatalf("scrubbed error %#v is no longer a ProviderError", err)
			}
			if string(out.ResponseBody) != want {
				t.Errorf("ResponseBody = %q, want %q", out.ResponseBody, want)
			}
		})
	}
}

func TestMidStreamErrorCopiesClassification(t *testing.T) {
	pe := dirtyProviderError()
	pe.Title = "stream error"
	pe.StatusCode = 0
	pe.AuthError = true
	pe.ContextTooLargeErr = true
	pe.TransientError = true
	mse := newScrubber(canary).midStream(pe)
	if found := leaks(mse, canary); len(found) > 0 {
		t.Fatalf("the key is reachable at %v", found)
	}
	if !mse.AuthError || !mse.IsContextTooLarge() || mse.StatusCode != 0 {
		t.Errorf("classification lost: %+v, tooLarge=%v", *mse, mse.IsContextTooLarge())
	}
	if !strings.HasPrefix(mse.Message, "stream error: bad key ") {
		t.Errorf("Message = %q, want the original text", mse.Message)
	}
	if fantasy.IsTransportError(mse) {
		t.Errorf("Error() %q reads as a transport error, which Fantasy would retry", mse.Error())
	}
	var asPE *fantasy.ProviderError
	if errors.As(error(mse), &asPE) {
		t.Error("a MidStreamError unwraps to a ProviderError, which Fantasy would retry")
	}
}

// review r3 major 2: the provider's names for a failure are read from its
// response as it arrived and kept only as short lowercase identifiers that
// the scrub leaves as they are — before output (on the rebuilt
// ProviderError) and after it (on the MidStreamError) alike. A key that is
// itself such an identifier is caught by the scrub's own pass; anything that
// is not an identifier is dropped, whatever it is.
func TestErrorNamesKeepOnlyIdentifiersTheScrubPasses(t *testing.T) {
	const idKey = "abcdef0123456789abcdef0123456789" // a key that is a lowercase identifier
	s := newScrubber(idKey)
	for _, tc := range []struct {
		name, body, code, typ string
	}{
		{"quota", `{"error":{"code":"insufficient_quota","type":"insufficient_quota"}}`, "insufficient_quota", "insufficient_quota"},
		{"an in-band event's names", `{"error":{"message":"m","type":"server_error"}}`, "", "server_error"},
		{"the key as a code", `{"error":{"code":"` + idKey + `","type":"rate_limit_exceeded"}}`, "", "rate_limit_exceeded"},
		{"not identifiers", `{"error":{"code":"Insufficient Quota","type":"sk-abc"}}`, "", ""},
		{"too long", `{"error":{"code":"` + strings.Repeat("a", 65) + `","type":"` + strings.Repeat("a", 64) + `"}}`, "", strings.Repeat("a", 64)},
		{"a number", `{"error":{"code":429,"type":"rate_limit_exceeded"}}`, "", "rate_limit_exceeded"},
		{"an HTTP dump", "HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\n\r\n" +
			`{"error":{"code":"insufficient_quota","type":"insufficient_quota"}}`, "insufficient_quota", "insufficient_quota"},
		{"no envelope", `upstream failed`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := &fantasy.ProviderError{StatusCode: 429, Message: "m", ResponseBody: []byte(tc.body)}
			err := s.err(pe)
			if code, typ := ErrorNames(err); code != tc.code || typ != tc.typ {
				t.Errorf("ErrorNames(the rebuilt ProviderError) = %q, %q; want %q, %q", code, typ, tc.code, tc.typ)
			}
			mse := s.midStream(pe)
			if code, typ := ErrorNames(mse); code != tc.code || typ != tc.typ || mse.Code != tc.code || mse.Type != tc.typ {
				t.Errorf("the MidStreamError's names = %q, %q; want %q, %q", mse.Code, mse.Type, tc.code, tc.typ)
			}
			for _, e := range []error{err, mse} {
				if found := leaks(e, idKey); len(found) > 0 {
					t.Fatalf("the key is reachable at %v", found)
				}
			}
			var out *fantasy.ProviderError
			if !errors.As(err, &out) || out.IsRetryable() != pe.IsRetryable() || fantasy.IsTransportError(out.Cause) {
				t.Errorf("the names changed how Fantasy classifies the error: %#v", err)
			}
		})
	}
}

// stubModel is an inner model whose every method fails with err, or, for
// Stream and StreamObject when err is nil, yields parts.
type stubModel struct {
	err         error
	parts       []fantasy.StreamPart
	objectParts []fantasy.ObjectStreamPart
}

func (m stubModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, m.err
}

func (m stubModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, m.err
}

func (m stubModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return seq(m.parts), nil
}

func (m stubModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return seq(m.objectParts), nil
}

func (stubModel) Provider() string { return "stub" }
func (stubModel) Model() string    { return "stub-model" }

func seq[T any](items []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, it := range items {
			if !yield(it) {
				return
			}
		}
	}
}

// TestEveryErrorPathIsScrubbed covers the paths no HTTP fixture reaches: an
// error returned by the inner Stream itself, and every method besides Stream.
func TestEveryErrorPathIsScrubbed(t *testing.T) {
	dirty := fmt.Errorf("request to https://h.example/v1?key=%s failed: %w", canary, dirtyProviderError())
	failing := wrap(stubModel{err: dirty}, newScrubber(canary))
	ctx := context.Background()

	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("the error was swallowed")
		}
		if found := leaks(err, canary); len(found) > 0 {
			t.Fatalf("the key is reachable at %v", found)
		}
		var pe *fantasy.ProviderError
		if !errors.As(err, &pe) || pe.StatusCode != 401 {
			t.Fatalf("err = %#v, want the 401 ProviderError", err)
		}
	}
	t.Run("Stream", func(t *testing.T) {
		_, err := failing.Stream(ctx, fantasy.Call{})
		check(t, err)
	})
	t.Run("Generate", func(t *testing.T) {
		_, err := failing.Generate(ctx, fantasy.Call{})
		check(t, err)
	})
	t.Run("GenerateObject", func(t *testing.T) {
		_, err := failing.GenerateObject(ctx, fantasy.ObjectCall{})
		check(t, err)
	})
	t.Run("StreamObject", func(t *testing.T) {
		_, err := failing.StreamObject(ctx, fantasy.ObjectCall{})
		check(t, err)
	})
	// The part checks count what they saw: a wrapper that yielded nothing
	// would otherwise pass them.
	t.Run("StreamObject error part", func(t *testing.T) {
		m := wrap(stubModel{objectParts: []fantasy.ObjectStreamPart{{Type: fantasy.ObjectStreamPartTypeError, Error: dirty}}}, newScrubber(canary))
		parts, err := m.StreamObject(ctx, fantasy.ObjectCall{})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for part := range parts {
			n++
			check(t, part.Error)
		}
		if n != 1 {
			t.Fatalf("saw %d parts, want the 1 error part", n)
		}
	})
	t.Run("Stream error part before output", func(t *testing.T) {
		m := wrap(stubModel{parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: dirty}}}, newScrubber(canary))
		parts, err := m.Stream(ctx, fantasy.Call{})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for part := range parts {
			n++
			check(t, part.Error)
		}
		if n != 1 {
			t.Fatalf("saw %d parts, want the 1 error part", n)
		}
	})
}

// TestScrubNeverTouchesHeaderNames: a key whose text also occurs in a header
// name must not mangle the name, or Fantasy's lowercase retry-after-ms
// lookup misses and the retry waits its 5 s default.
func TestScrubNeverTouchesHeaderNames(t *testing.T) {
	s := newScrubber("After") // as it appears in the canonical name
	got := s.headers(map[string]string{"Retry-After-Ms": "1", "X-Echo": "Bearer After"})
	want := map[string]string{"retry-after-ms": "1", "x-echo": "Bearer " + redacted}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("headers = %v, want %v", got, want)
	}
}
