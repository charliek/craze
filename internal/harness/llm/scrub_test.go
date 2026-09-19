package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// leaks lists every place needle can be found from v: its formatted forms,
// and every string and byte slice reachable from it by reflection —
// unexported fields, causes, maps and slices included. Reflection is the
// point: an error that prints clean but still holds the original in a field
// leaks the moment a later layer reads that field or formats it with %#v.
func leaks(v any, needle string) []string {
	var found []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if strings.Contains(fmt.Sprintf(verb, v), needle) {
			found = append(found, "formatted "+verb)
		}
	}
	w := &walker{needle: []byte(needle), seen: map[visit]bool{}}
	w.walk(reflect.ValueOf(v), "err")
	return append(found, w.found...)
}

type visit struct {
	ptr uintptr
	typ reflect.Type
}

type walker struct {
	needle []byte
	seen   map[visit]bool
	found  []string
}

func (w *walker) walk(v reflect.Value, path string) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), string(w.needle)) {
			w.found = append(w.found, path)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if bytes.Contains(v.Bytes(), w.needle) {
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
