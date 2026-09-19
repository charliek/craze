package modeltable

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

var (
	// ErrNotConfigured is Load's error when the directory holds neither file:
	// the harness has never been set up, and the caller's actionable answer is
	// to run `craze import gx`. The error also matches fs.ErrNotExist.
	ErrNotConfigured = errors.New("modeltable: no models configured")

	// ErrUnknownModel is Resolve's error for an alias the table does not have.
	ErrUnknownModel = errors.New("modeltable: unknown model")

	// ErrNoAPIKey is Resolve's error when a provider has no usable key: none
	// of its env_keys is set to a non-empty value and it has no inline
	// api_key. The wrapped message names the provider and the variable names
	// it tried, never a value.
	ErrNoAPIKey = errors.New("modeltable: no API key")

	// ErrKeyTooShort is Keys' error for a key under MinKeyLen bytes. The
	// wrapped message names the provider and the variable, never the value.
	// Load reports a short inline key as a *FileError at its api_key.
	ErrKeyTooShort = errors.New("modeltable: API key too short to be real")

	// ErrKeyOverlapsMarker is Keys' error for a key the redaction marker
	// could print back (redact.MarkerOverlaps) — "credential", say, which
	// the marker contains. Like ErrKeyTooShort, its wrapped message names the
	// provider and the variable, and Load reports an inline one as a
	// *FileError at its api_key.
	ErrKeyOverlapsMarker = errors.New("modeltable: API key overlaps craze's redaction marker")
)

// FileError locates a problem in one of the two files: the file, and then
// the line (for a syntax error) or the table and key (for anything decoded).
// Load reports File as the full path; Validate on an in-memory table, which
// has no directory, reports the base name. Reason never carries a key value.
type FileError struct {
	File   string
	Line   int    // 1-based; 0 when the problem is not tied to a line
	Table  string // TOML table path, e.g. models."fireworks/kimi-k3"; "" for the root
	Key    string // the key within Table; "" when the table itself is the problem
	Reason string
}

func (e *FileError) Error() string {
	parts := []string{"modeltable: " + e.File}
	if e.Line > 0 {
		parts = append(parts, fmt.Sprintf("line %d", e.Line))
	}
	switch {
	case e.Table != "" && e.Key != "":
		parts = append(parts, "["+e.Table+"] "+e.Key)
	case e.Table != "":
		parts = append(parts, "["+e.Table+"]")
	case e.Key != "":
		parts = append(parts, e.Key)
	}
	parts = append(parts, e.Reason)
	return strings.Join(parts, ": ")
}

// decodeError re-wraps a TOML decode error. A syntax error (toml.ParseError)
// keeps the file and the line and nothing else: its message can quote a
// fragment of the offending line and ErrorWithPosition prints the line whole,
// and for a malformed api_key line that line is the key. Any other decode
// error is a value of the wrong type for its key, whose message names the key
// path and the two types but never the value, so it is wrapped as it is.
func decodeError(path string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		return &FileError{File: path, Line: pe.Position.Line, Reason: "not valid TOML"}
	}
	return fmt.Errorf("modeltable: %s: %w", path, err)
}

// unknownKey is the strict decode's error for the first key the schema does
// not have: a typo in a hand-edited entry would otherwise be silently
// ignored, and an ignored max_output_tokens or default_effort is a model
// behaving differently from what its file says.
func unknownKey(path string, k toml.Key) error {
	table := ""
	if len(k) > 1 {
		table = k[:len(k)-1].String()
	}
	return &FileError{File: path, Table: table, Key: toml.Key{k[len(k)-1]}.String(), Reason: "unknown key"}
}

// noAPIKey is ErrNoAPIKey for one provider, naming the variables it tried.
func noAPIKey(provider string, envKeys []string) error {
	if len(envKeys) == 0 {
		return fmt.Errorf("%w for provider %q: it names no env_keys and has no api_key in %s",
			ErrNoAPIKey, provider, ProvidersFile)
	}
	return fmt.Errorf("%w for provider %q: none of %s is set, and it has no api_key in %s",
		ErrNoAPIKey, provider, strings.Join(envKeys, ", "), ProvidersFile)
}
