package websearch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/charliek/craze/internal/atomicfile"
)

// The remembered choice (plan 051 §C.2): what a person answered to the
// consent question, in StateFile under the native directory, so the next
// session neither asks again nor offers web tools the person turned down.
// config.toml wins over it; the harness never rewrites config.toml.
const (
	// StateFile is the remembered choice's name in the native directory
	// ($CRAZE_HOME/native). The file tools refuse it as they refuse
	// providers.toml; tool/opencode keeps its own copy of the name, which a
	// test here pins to this one.
	StateFile = "websearch.json"
	// StateVersion is the one version of the file this craze reads and
	// writes.
	StateVersion = 1
	// stateMaxBytes is the most of the file that is read: two members fit
	// in far less, and a larger file is not one craze wrote.
	stateMaxBytes = 4 << 10
	// statePerm and stateDirPerm are the file's and its directory's modes,
	// the native directory's own.
	statePerm    = 0o600
	stateDirPerm = 0o700
)

// Choice is a remembered choice: allowed through a provider, disabled, or
// (the zero value) nothing remembered.
type Choice struct {
	// Provider is the provider web search was allowed through, "" when it
	// was not.
	Provider string
	// Disabled is web search turned off from the consent card.
	Disabled bool
}

// stateDoc is the file: {"version":1,"provider":"exa"} or
// {"version":1,"provider":false}. Provider is kept raw, since it is a string
// or a bool.
type stateDoc struct {
	Version  *int            `json:"version"`
	Provider json.RawMessage `json:"provider"`
}

// ReadChoice reads the remembered choice at path, leniently: a missing file
// is nothing remembered and says nothing; a file that cannot be read, is not
// a regular file, is larger than any craze writes, is not JSON, has a version
// this craze does not know, or names no provider it has, is nothing
// remembered too, and why says so in a line that names the file and never
// its contents. It never fails, and it writes nothing.
func ReadChoice(path string) (c Choice, why string) {
	b, err := readState(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Choice{}, ""
	}
	if err != nil {
		return Choice{}, StateFile + " could not be read, so no web search choice is remembered"
	}
	var doc stateDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return Choice{}, StateFile + " is not valid JSON, so no web search choice is remembered"
	}
	if doc.Version == nil || *doc.Version != StateVersion {
		return Choice{}, StateFile + " has a version this craze does not read, so no web search choice is remembered"
	}
	if bytes.Equal(doc.Provider, []byte("false")) {
		return Choice{Disabled: true}, ""
	}
	var id string
	if err := json.Unmarshal(doc.Provider, &id); err == nil && slices.Contains(Providers(), id) {
		return Choice{Provider: id}, ""
	}
	return Choice{}, StateFile + " names no web search provider this craze has, so no web search choice is remembered"
}

// readState is the file's bytes, and an fs.ErrNotExist error when there is
// none. It is opened without blocking (a FIFO opens at once), refused unless
// it is a regular file, and read up to stateMaxBytes: modeltable's
// recent.json rule.
func readState(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("websearch: not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, stateMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > stateMaxBytes {
		return nil, errors.New("websearch: larger than the remembered choice can be")
	}
	return b, nil
}

// WriteChoice remembers c at path: the whole file written atomically at 0600,
// its directory made 0700 first. Two sessions answering at once: the last
// write wins, and each keeps its own answer. A choice that remembers nothing,
// or names a provider the table does not have, is an error and writes
// nothing.
func WriteChoice(path string, c Choice) error {
	var provider any
	switch {
	case c.Disabled && c.Provider == "":
		provider = false
	case !c.Disabled && slices.Contains(Providers(), c.Provider):
		provider = c.Provider
	default:
		return errors.New("websearch: not a choice to remember")
	}
	b, err := json.Marshal(struct {
		Version  int `json:"version"`
		Provider any `json:"provider"`
	}{StateVersion, provider})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), stateDirPerm); err != nil {
		return fmt.Errorf("websearch: %w", err)
	}
	if err := atomicfile.Write(path, append(b, '\n'), statePerm); err != nil {
		return fmt.Errorf("websearch: %w", err)
	}
	return nil
}
