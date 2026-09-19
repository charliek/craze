package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, the bound
// opencode's integer schemas carry (parameters.test.ts's "bounds bare integer
// fields to safe integer range"), so the schemas here say it too.
const maxSafeInteger = 1<<53 - 1

// args is a call's argument object, decoded one field at a time so that an
// error names the field and says what it should have been. Fantasy checks
// only that the input is JSON and that required names are present (plan 019
// §3.1), so the type, sign and range checks opencode's schemas make happen
// here, in Prepare. Like opencode's schemas, an unknown field is ignored and
// nothing is coerced: a number sent as a string, or null for an optional
// field, is refused.
type args map[string]json.RawMessage

func parseArgs(in json.RawMessage) (args, error) {
	var a args
	if err := json.Unmarshal(in, &a); err != nil || a == nil {
		return nil, errors.New("the arguments must be a JSON object")
	}
	return a, nil
}

// str returns the string field name. ok is false when an optional field is
// absent; a required one that is absent is an error, and so is any value
// that is not a string.
func (a args) str(name string, required bool) (s string, ok bool, err error) {
	raw, present := a[name]
	if !present {
		if required {
			return "", false, fmt.Errorf("%s is required: want a string", name)
		}
		return "", false, nil
	}
	if t := jsonType(raw); t != "string" {
		return "", false, fmt.Errorf("%s must be a string, got a JSON %s", name, t)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, fmt.Errorf("%s must be a string: %v", name, err)
	}
	return s, true, nil
}

// integer returns the optional integer field name, which must be a JSON
// number with no fraction, from min to maxSafeInteger. ok is false when it
// is absent. As in opencode's schemas (Schema.Int), 5.0 and 5e0 are the
// integer 5.
func (a args) integer(name string, min int64) (n int64, ok bool, err error) {
	raw, present := a[name]
	if !present {
		return 0, false, nil
	}
	if t := jsonType(raw); t != "number" {
		return 0, false, fmt.Errorf("%s must be an integer, got a JSON %s", name, t)
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	switch {
	case err != nil || f != math.Trunc(f):
		return 0, false, fmt.Errorf("%s must be an integer, not %s", name, raw)
	case f < float64(min):
		return 0, false, fmt.Errorf("%s must be at least %d, not %s", name, min, raw)
	case f > maxSafeInteger:
		return 0, false, fmt.Errorf("%s must be at most %d, not %s", name, int64(maxSafeInteger), raw)
	}
	return int64(f), true, nil
}

// jsonType names the type of a JSON value by its first byte. The value came
// out of a successful decode, so it is valid and has no leading space.
func jsonType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "nothing"
	}
	switch raw[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}
