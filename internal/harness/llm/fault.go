package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/fantasy"
)

type retractionKey struct{}

// WithRetraction marks ctx as a step's whose caller takes back what a failed
// attempt streamed (rule 3): the turn, which tells every consumer of its events
// (harness.Retrying). A caller that hands text on as it comes and cannot — a
// side question — or that keeps none until a step is whole and sets its own
// retries — a summary — sends without it, so a failure after output stays final.
func WithRetraction(ctx context.Context) context.Context {
	return context.WithValue(ctx, retractionKey{}, true)
}

func retracts(ctx context.Context) bool { return ctx.Value(retractionKey{}) != nil }

// transportTitle is the title Fantasy gives a transport failure it wraps
// (fantasy.NewTransportError, NewIncompleteStreamError, WrapTransportError),
// which is all a rebuilt or driver-made ProviderError has to say so.
const transportTitle = "stream transport error"

// protocolTitle is the title of a stream that broke its protocol (messagesError):
// retried before any output, as an incomplete stream is, and final after it. A
// malformed response is no transport's failure, and the same request may get it
// again.
const protocolTitle = "stream protocol error"

// serverFaults are the provider's names for a failure of its own, in the
// stream: Fantasy's TransientStreamErrorTypes less rate_limit_error, which is
// the account's limit and no transport failure. A code or type the set does not
// hold — a content filter, a schema refusal, a context too large — is the
// request's, and the same request would meet it again.
var serverFaults = map[string]bool{
	"server_error":     true,
	"internal_error":   true,
	"overloaded_error": true,
	"api_error":        true,
}

// streamFault reports whether err, what a step's stream ended with, is the
// transport's or the server's and says nothing of the request, so that the same
// request may do better:
//
//   - a provider's failure in a status: a 5xx (Anthropic's error events carry
//     the status their type stands for, 529 for overloaded_error);
//   - a provider's in-band error event, by its code and type (serverFaults), or
//     a numeric 5xx code, which is how OpenRouter's event says it;
//   - a transport's failure, wrapped by Fantasy or not (transportFailure).
//
// An auth failure and a context too large never are, whatever else they say, and
// nor is a stream a parser found malformed, which is read first, ahead of any
// text: Fantasy's own for a chunk it could not read is a *fantasy.Error titled
// "stream error", in the words fantasy.IsTransportError looks for, and a broken
// protocol unwraps to the unexpected EOF of a truncated stream. A *FinalError is
// ruled out by the caller.
func streamFault(err error) bool {
	var fe *fantasy.Error
	if errors.As(err, &fe) {
		return false
	}
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) {
		return transportFailure(err)
	}
	switch {
	case pe.AuthError || pe.IsContextTooLarge() || pe.Title == protocolTitle:
		return false
	case pe.StatusCode != 0:
		return pe.StatusCode >= 500
	}
	if said, fault := inBandFault(pe.ResponseBody); said {
		return fault
	}
	return pe.Title == transportTitle || pe.Cause != nil && transportFailure(pe.Cause)
}

// inBandFault reads an in-band error event's body: said is false when it is not
// the OpenAI-family envelope, which every driver's error events are written in
// (errorBody, Fantasy's stream errors), and fault whether it names the server's
// own failure.
func inBandFault(body []byte) (said, fault bool) {
	var env struct {
		Error *struct {
			Code json.RawMessage `json:"code"`
			Type json.RawMessage `json:"type"`
		} `json:"error"`
	}
	if json.NewDecoder(bytes.NewReader(body)).Decode(&env) != nil || env.Error == nil {
		return false, false
	}
	var status int
	if json.Unmarshal(env.Error.Code, &status) == nil && status >= 500 && status < 600 {
		return true, true
	}
	code, typ := jsonString(env.Error.Code), jsonString(env.Error.Type)
	return true, serverFaults[strings.ToLower(code)] || serverFaults[strings.ToLower(typ)]
}

// transportPhrasesFailed are the words Go's transports use for a connection or a
// stream that went under a request, where they have no type to test: the
// standard library bundles its own HTTP/2 package, whose errors match none of
// golang.org/x/net/http2's types. They are tighter than
// fantasy.IsTransportError's "stream error:", which anything so titled says, and
// its GOAWAY's text is none of Fantasy's phrases.
var transportPhrasesFailed = []string{
	"stream error: stream ID",
	"connection error: ",
	"GOAWAY",
	"http2: client connection lost",
	"connection reset",
	"unexpected EOF",
}

// transportFailure reports whether err is a connection's or a stream's failure:
// an HTTP/2 reset or GOAWAY, a truncated body, a reset connection, any network
// error.
func transportFailure(err error) bool {
	var ne net.Error
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return true
	case errors.As(err, &ne):
		return true
	}
	msg := err.Error()
	for _, p := range transportPhrasesFailed {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// longWait is how long a wait a provider may ask for and a step still be sent
// again. Fantasy ignores a Retry-After of this or more and waits its own 5 s,
// which would spend the budget on a provider that said not soon; so a failure after
// output that asks for it stays final (stepError). Before any output is Fantasy's
// own rule, as ever.
const longWait = 60 * time.Second

// asksLongWait reports whether err asks for a wait of longWait or more, read as
// Fantasy reads it (getRetryDelayInMs): retry-after-ms, else retry-after, seconds
// or an HTTP date.
func asksLongWait(err error) bool {
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) {
		return false
	}
	var ms, after string
	for k, v := range pe.ResponseHeaders {
		switch {
		case strings.EqualFold(k, "retry-after-ms"):
			ms = v
		case strings.EqualFold(k, "retry-after"):
			after = v
		}
	}
	if n, err := strconv.ParseFloat(ms, 64); err == nil && n > 0 {
		return time.Duration(n*float64(time.Millisecond)) >= longWait
	}
	if n, err := strconv.ParseFloat(after, 64); err == nil {
		return time.Duration(n*float64(time.Second)) >= longWait
	}
	if at, err := time.Parse(time.RFC1123, after); err == nil {
		return time.Until(at) >= longWait
	}
	return false
}
