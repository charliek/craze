package harness

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// The doom-loop guard (plan 019 §3.7, D-42): a model that makes the same call
// over and over is refused, told so, and then stopped.
//
// It counts in OnToolCall rather than in the dispatcher because that callback
// runs on one goroutine, in call order, and it sees every call the model
// completed — including the ones Fantasy refuses as invalid and the ones
// naming a tool that does not exist, which never reach a tool at all
// (agent.go:809-836) and so would otherwise repeat, a paid request at a time,
// until the step limit ended the turn at step 200.
//
// Once craze has an approval channel the third call becomes an Ask instead,
// which is what opencode does (session/processor.ts:353-380); until then the
// nudge stands in for it (decision 2, D-42).

const (
	// doomNudgeAt is the consecutive identical call from which a call is
	// refused unrun and the model is told to stop.
	doomNudgeAt = 3
	// doomStopAt is the one that also ends the turn.
	doomStopAt = 5
	// doomStopped is added to the nudge of the call that ends the turn.
	doomStopped = "This turn is being stopped."
)

// doomLoop is the guard's state for one turn: the signature of the last call
// announced, and how many calls in a row have had it. It is guarded by the
// turn's mu, and it spans steps — "consecutive" means consecutive in call
// order, not within one step, so a model that repeats itself a step at a time
// is caught exactly as one that repeats itself in parallel.
//
// A retried step does not roll it back and needs no rollback: the wrapper
// retries only a step that failed before it produced any output, so a call a
// failed attempt began never arrives and was never counted (turn.go's retry,
// plan 018 §3.7).
type doomLoop struct {
	sig     string // the last announced call's signature; "" before the first
	n       int    // how many calls in a row have had it
	stopped bool   // the turn ends once this step does (halted), reporting max_turn_requests (finish)
}

// vet is the guard, called from OnToolCall on its goroutine with mu held,
// once per announced call in call order (toolbridge.go). It counts the call,
// and from the third identical one in a row refuses it: the veto stands in
// for the run, so the tool never acts, and the model reads why. The fifth
// also ends the turn, which Run reports as max_turn_requests; its failed card
// carries the same text, so the user sees why too.
//
// Fantasy dispatches a call only on a "tool-calls" finish, so a veto does not
// always reach runTool. Under any other finish that still announces calls the
// runner answers the call with the veto itself (stepResults); a call Fantasy
// refuses outright — invalid arguments, a tool that does not exist — is
// answered by Fantasy with its own reason, which is what the model reads, and
// is counted all the same. Either way the turn is ended by the stop condition
// (halted) and reported by finish, not by the result's StopTurn, which only a
// dispatched call could ever carry.
func (t *turn) vet(c *toolCall) {
	if sig := callSignature(c.name, c.input); sig != t.loop.sig {
		t.loop.sig, t.loop.n = sig, 1
		return
	}
	t.loop.n++
	if t.loop.n < doomNudgeAt {
		return
	}
	stop := t.loop.n >= doomStopAt
	t.loop.stopped = t.loop.stopped || stop
	// The tool's name is the model's own text, so the result and the report
	// are redacted, like every other result the runner writes itself.
	name := t.redactor().String(c.name)
	c.veto = &tool.Result{Text: doomText(name, t.loop.n, stop), IsError: true, Class: tool.ClassDoomLoop, StopTurn: stop}
	t.emit(Diag{Kind: DiagDoomLoop, Fields: map[string]string{
		"step": strconv.Itoa(t.step), "id": c.id, "tool": name,
		"count": strconv.Itoa(t.loop.n), "stopped": strconv.FormatBool(stop),
	}})
}

// doomText is what a refused call's result says: §3.7's sentence, with the
// stop sentence after it for the call that ends the turn.
func doomText(name string, n int, stop bool) string {
	text := fmt.Sprintf("You have called %s with the same arguments %d times in a row. "+
		"Stop repeating this call; change your approach or explain what is blocking you.", name, n)
	if stop {
		text += " " + doomStopped
	}
	return text
}

// callSignature identifies a call for the guard: the tool's name, quoted so
// the boundary between it and the arguments is unambiguous, and the arguments
// canonically.
func callSignature(name, input string) string {
	return strconv.Quote(name) + canonicalJSON(input)
}

// canonicalJSON is input with every object's keys sorted, at every depth
// (encoding/json marshals a map in key order) and every number spelled as the
// tools read it, so the same call written differently has the same signature.
// An array keeps its order, which is a value and not a spelling.
//
// Arguments that are not one whole JSON value are taken as they came: they
// are what the model sent, and repeating them is still repeating a call.
func canonicalJSON(input string) string {
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return input
	}
	b, err := json.Marshal(canonicalNumbers(v))
	if err != nil {
		return input
	}
	return string(b)
}

// canonicalNumbers rewrites every number in a decoded value (UseNumber, so
// each is a json.Number) to its canonical spelling. The value was decoded
// here and belongs to no one else, so it is rewritten in place.
func canonicalNumbers(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			v[k] = canonicalNumbers(x)
		}
		return v
	case []any:
		for i, x := range v {
			v[i] = canonicalNumbers(x)
		}
		return v
	case json.Number:
		return canonicalNumber(v)
	}
	return v
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, the largest
// integer past which a float64 no longer holds every one exactly, and the
// bound the tools themselves refuse a number above (tool/opencode/args.go).
// JSON has one number type and the tools read every number through a
// float64, so within this every integer literal parses exactly; above it two
// literals that are not the same number round to one value.
const maxSafeInteger = 1<<53 - 1

// canonicalNumber is n as the tools would read it: 5, 5.0, 5e0 and 5.00 are
// all the integer 5 (opencode's Schema.Int, ported in args.integer), so they
// are all spelled "5", and any other number is spelled by the shortest
// literal that reads back as the same float64. Without this a model
// alternating spellings of one argument would repeat a call for ever without
// ever repeating a signature.
//
// It is total, and it never folds together two literals that are not the same
// number: one that is not a float64 at all (1e999), and one whose value is
// past maxSafeInteger — where 9007199254740993 and 9007199254740992 both read
// back as 2^53 — keep the spelling they were written with. That can only miss
// a loop, never invent one.
func canonicalNumber(n json.Number) json.Number {
	f, err := strconv.ParseFloat(n.String(), 64)
	switch {
	case err != nil, f > maxSafeInteger, f < -maxSafeInteger:
		return n
	case f == math.Trunc(f):
		return json.Number(strconv.FormatInt(int64(f), 10))
	}
	return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
}
