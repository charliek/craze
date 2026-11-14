package modeltable

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/charliek/craze/internal/atomicfile"
)

// The stored web search key (plan 051 §F.5). A key for something other than
// a model provider — the native harness's web search — is not a provider
// entry (everything that walks Table.Providers would have to learn to skip
// it) and not a table of providers.toml (decoded strictly, so an older craze
// sharing the home would refuse it). It lives in a file of its own beside
// providers.toml, CredentialsFile: one table per credential id, `[exa]` with
// `api_key`, which an older craze never reads.
//
//   - Reading is lenient about what it does not know: unknown tables and
//     keys are ignored. A file that cannot be parsed, or that is a symlink or
//     not a regular file, yields no stored credentials and one diagnostic; an
//     `[exa]` that is not a table, or an `api_key` that is not a string, is
//     no stored key and one diagnostic.
//   - Writing (SetCredential, RemoveCredential) is strict, on the provider
//     store's model (editKeyFile): under its own lock, credentialsLockName,
//     the file is read, refused if it cannot be parsed or is not a regular
//     file — an error, nothing written — one key changed, every table and key
//     craze does not know kept, and the whole written back atomically at
//     0600.
//   - Every diagnostic and error is value-free: a TOML parser's message can
//     quote the line holding the secret, so a syntax error keeps the file and
//     the line alone (credentialsDecodeError), and a structural one names the
//     table and the key, never what they hold.
//
// The registry, webCredentials, is the credentials the file can hold: what a
// Table loads (LoadWith reads the file for every kind of table, so Keys hands
// the redactor every stored value), what the web search resolves its key
// from (WebSearchKey: the variable, then the stored key, then none), and what
// `craze auth` stores and lists.

// CredentialsFile is the name of the stored web search keys' file in the
// native directory, beside ProvidersFile.
const CredentialsFile = "credentials.toml"

// credentialsLockName is the lock every writer of CredentialsFile takes,
// beside it: its own, so a writer of providers.toml never waits on it.
const credentialsLockName = CredentialsFile + ".lock"

// WebCredentialPrefix is what a web search credential's full name starts
// with: `websearch/exa`, which `craze auth` always takes, whatever a model
// provider is called.
const WebCredentialPrefix = "websearch/"

// WebCredential is one key the web search can be given (plan 051 §F.5).
type WebCredential struct {
	// ID is the credential's id: its table in CredentialsFile, and its bare
	// name for `craze auth` when no model provider has it.
	ID string
	// Name is its display name.
	Name string
	// Env is the variable a key for it is read from, ahead of a stored one.
	Env string
}

// FullName is c's name that no model provider can shadow: WebCredentialPrefix
// and its id.
func (c WebCredential) FullName() string { return WebCredentialPrefix + c.ID }

// webCredentials is the registry: every credential CredentialsFile can hold,
// in the order craze lists them. webCredentialEnv is built from their Env.
var webCredentials = []WebCredential{{ID: "exa", Name: "Exa", Env: ExaKeyEnv}}

// WebCredentials is the registry, a copy the caller owns.
func WebCredentials() []WebCredential { return slices.Clone(webCredentials) }

// webCredential is the registry's credential id, and whether there is one.
func webCredential(id string) (WebCredential, bool) {
	i := slices.IndexFunc(webCredentials, func(c WebCredential) bool { return c.ID == id })
	if i < 0 {
		return WebCredential{}, false
	}
	return webCredentials[i], true
}

var (
	// ErrUnknownCredential is SetCredential's and RemoveCredential's error
	// for an id the registry does not have.
	ErrUnknownCredential = errors.New("modeltable: unknown web search credential")

	// ErrSymlinkedCredentialsFile is the error for a CredentialsFile that is
	// a symlink: neither read (no stored credentials) nor written through.
	ErrSymlinkedCredentialsFile = errors.New("modeltable: " + CredentialsFile + " is a symlink")

	// ErrCredentialsNotRegular is the error for a CredentialsFile that is
	// not a regular file (a directory, a device, a pipe).
	ErrCredentialsNotRegular = errors.New("modeltable: " + CredentialsFile + " is not a regular file")

	// ErrCredentialNotATable is SetCredential's and RemoveCredential's
	// refusal of a file whose credential entry is something other than a
	// table — `exa = "…"` — which craze would have to replace whole: the
	// owner fixes it by hand.
	ErrCredentialNotATable = errors.New("modeltable: a web search credential that is not a table")

	// ErrKeyNotUTF8 is SetCredential's refusal of a key that is not valid
	// UTF-8 (review C5 item 2): the encoder would write it into a quoted
	// string that every later decode — the reading, login, logout — rejects.
	ErrKeyNotUTF8 = errors.New("modeltable: the API key is not valid UTF-8")
)

// maxCredentialsFile is the most of a CredentialsFile craze reads, as for the
// other files it reads whole (maxChatGPTModelsFile, MaxAnthropicModelsFile):
// a larger one is damaged — no key is read from it and no write replaces it —
// rather than read into memory however large it is (review C5 item 2).
const maxCredentialsFile = 1 << 20

// credentialsHeader is written above the CredentialsFile SetCredential and
// RemoveCredential write. Every earlier comment is gone by then, so it says
// so.
const credentialsHeader = `# craze native harness: keys for things other than a model provider (the web search's [exa] api_key).
# craze rewrites this file for "craze auth": comments are not kept. Keep it 0600; never paste it.

`

// StoredCredential is one credential's api_key as CredentialsFile holds it:
// untrimmed and unjudged, and never blank. Key is a Secret, so a printed
// StoredCredential shows which credential has a key and never the key.
type StoredCredential struct {
	ID  string
	Key Secret
}

// StoredCredentials reads the keys dir's CredentialsFile stores, one per
// registry credential whose table writes a non-blank string api_key, in
// registry order: what a running session learns, for redaction only, at a
// turn's start (plan 051 §F.5), and what a caller with no Table reads. A
// missing file is no keys and no error.
//
// The error is the file's as a whole — it cannot be read, it is a symlink or
// not a regular file, or it is not valid TOML — naming the file (and the
// line) and never a value; the file then yields nothing. A credential entry
// of the wrong shape is skipped without an error (WebKeys and LoadWith say
// so). Nothing is judged: what may be learned is the caller's call (the
// harness's LearnKeys holds each value to KeyProblem). The file's mode is
// left alone.
func StoredCredentials(dir string) ([]StoredCredential, error) {
	stored, _, err := StoredCredentialsAndProblems(dir)
	return stored, err
}

// StoredCredentialsAndProblems is StoredCredentials with the value-free lines
// for each credential entry of the wrong shape it skipped — an [exa] that is
// not a table, an api_key that is not a string — exactly as LoadWith says
// them among a table's Warnings: what a running session's look says when the
// file changes to hold one (review C5 item 6). problems is nil when err is
// not.
func StoredCredentialsAndProblems(dir string) (stored []StoredCredential, problems []string, err error) {
	if dir == "" {
		return nil, nil, errors.New("modeltable: no directory to read keys from")
	}
	r := readCredentials(filepath.Join(dir, CredentialsFile))
	return r.stored, r.problems, r.err
}

// credentialsReading is one lenient reading of a CredentialsFile.
type credentialsReading struct {
	// has says the file is there (a symlink or a non-regular file included).
	has bool
	// err is the file's own problem, value-free; nothing else is set then.
	err error
	// stored is every registry credential with a non-blank string api_key,
	// untrimmed, in registry order.
	stored []StoredCredential
	// problems are the value-free lines for a credential entry of the wrong
	// shape, each ignored.
	problems []string
}

// readCredentials reads path leniently (see the file's comment). It opens
// the file without following a final symlink and reads only a regular file,
// so what is judged is what is read.
func readCredentials(path string) credentialsReading {
	doc, has, err := readCredentialsDoc(path)
	r := credentialsReading{has: has, err: err}
	if err != nil || !has {
		return r
	}
	for _, c := range webCredentials {
		k, ok, problem := credentialKey(path, doc, c.ID)
		if problem != "" {
			r.problems = append(r.problems, problem)
		}
		if ok {
			r.stored = append(r.stored, StoredCredential{ID: c.ID, Key: Secret(k)})
		}
	}
	return r
}

// credentialKey is id's api_key in doc: the value and true when it is a
// non-blank string, and a value-free line when the entry is there in a shape
// that cannot hold one — id not a table, or api_key not a string.
func credentialKey(path string, doc map[string]any, id string) (key string, ok bool, problem string) {
	v, has := doc[id]
	if !has {
		return "", false, ""
	}
	tbl, isTable := v.(map[string]any)
	if !isTable {
		return "", false, notATable(path, id, "no key is read from it").Error()
	}
	raw, has := tbl["api_key"]
	if !has {
		return "", false, ""
	}
	s, isString := raw.(string)
	if !isString {
		return "", false, (&FileError{File: path, Table: toml.Key{id}.String(), Key: "api_key",
			Reason: "not a string; it is ignored"}).Error()
	}
	if strings.TrimSpace(s) == "" {
		return "", false, ""
	}
	return s, true, ""
}

// readCredentialsDoc is path's tables, decoded leniently into a map: has is
// false, with no error, for a missing file. A symlink (not followed), a file
// that is not regular, one that cannot be read, one larger than
// maxCredentialsFile (no more than that and a byte is read) and one that is
// not valid TOML are each a value-free error, with has true.
func readCredentialsDoc(path string) (doc map[string]any, has bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		// O_NOFOLLOW refuses a symlink with ELOOP; Lstat tells it apart from
		// any other failure to open.
		if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, true, fmt.Errorf("%w: %s points elsewhere; craze neither reads nor writes through it — replace the link with the file itself", ErrSymlinkedCredentialsFile, path)
		}
		return nil, true, fmt.Errorf("modeltable: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, true, fmt.Errorf("modeltable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%w: %s", ErrCredentialsNotRegular, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialsFile+1))
	if err != nil {
		return nil, true, fmt.Errorf("modeltable: %s: %w", path, err)
	}
	if len(data) > maxCredentialsFile {
		return nil, true, &FileError{File: path, Reason: fmt.Sprintf("larger than %d MiB, which no credentials file is", maxCredentialsFile>>20)}
	}
	doc = map[string]any{}
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, true, credentialsDecodeError(path, err)
	}
	return doc, true, nil
}

// credentialsDecodeError is a CredentialsFile's decode error, value-free
// whatever it is: a syntax error through decodeError (the file and the line,
// never the parser's words, which can quote the line holding the key), and
// anything else — a map holds any value, so there should be nothing else —
// the same fixed reason, the decoder's own message dropped.
func credentialsDecodeError(path string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		return decodeError(path, err)
	}
	return &FileError{File: path, Reason: "not valid TOML"}
}

// WebKeyInfo is one web search credential as key management sees it
// (WebKeys).
type WebKeyInfo struct {
	WebCredential
	// Via is how it is keyed now, in the order WebSearchKey reads it:
	// KeyFromEnv (Env holds a usable key), KeyStored (a usable stored key and
	// no usable variable ahead of it) or KeyNone — the keyless endpoint.
	Via KeySource
	// EnvProblem is why Env, set, cannot be a key — ErrKeyTooShort or
	// ErrKeyOverlapsMarker, naming the rule and never the value — and nil
	// when it is unset, blank or usable. Such a value is skipped.
	EnvProblem error
	// Stored says the file holds a non-blank string api_key for it, usable
	// or not.
	Stored bool
	// StoredProblem is why that stored key cannot be one, as EnvProblem;
	// nil for a usable key or none. It is skipped too.
	StoredProblem error
}

// Connected reports whether w is keyed now: by its variable or a stored key.
func (w WebKeyInfo) Connected() bool { return w.Via == KeyFromEnv || w.Via == KeyStored }

// WebKeys is every registry credential and how it is keyed now through
// getenv (nil is os.Getenv), from dir's CredentialsFile read leniently, and
// the lines to say about it: the file's own problem, an entry of the wrong
// shape, a stored key that cannot be one — each naming the file and never a
// value, exactly as LoadWith says them — and a variable set to a value that
// cannot be a key, as EnvWarnings says it. It never fails and writes
// nothing.
func WebKeys(dir string, getenv func(string) string) ([]WebKeyInfo, []string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	path := filepath.Join(dir, CredentialsFile)
	r := readCredentials(path)
	out := make([]WebKeyInfo, 0, len(webCredentials))
	for _, c := range webCredentials {
		info := WebKeyInfo{WebCredential: c, Via: KeyNone}
		if k, err := envKey(getenv, c.Env); err != nil {
			info.EnvProblem = err
		} else if k != "" {
			info.Via = KeyFromEnv
		}
		for _, sc := range r.stored {
			if sc.ID != c.ID {
				continue
			}
			info.Stored, info.StoredProblem = true, KeyProblem(sc.Key.Reveal())
			if info.Via == KeyNone && info.StoredProblem == nil {
				info.Via = KeyStored
			}
		}
		out = append(out, info)
	}
	lines := r.lines(path)
	for _, w := range out {
		if w.EnvProblem != nil {
			lines = append(lines, envWarning(w.Env, w.EnvProblem))
		}
	}
	return out, lines
}

// lines are r's diagnostics, value-free, in the words LoadWith and WebKeys
// both say: the file's own problem, then each entry of the wrong shape, then
// each stored key that cannot be one.
func (r credentialsReading) lines(path string) []string {
	if r.err != nil {
		return []string{r.err.Error() + "; no stored web search key is read from it"}
	}
	out := slices.Clone(r.problems)
	for _, sc := range r.stored {
		if err := KeyProblem(sc.Key.Reveal()); err != nil {
			c, _ := webCredential(sc.ID)
			out = append(out, (&FileError{File: path, Table: toml.Key{sc.ID}.String(), Key: "api_key",
				Reason: keyReason(err) + "; it is ignored (replace it with \"craze auth login " + c.FullName() + "\")"}).Error())
		}
	}
	return out
}

// usable is r's stored keys that can be keys, trimmed, by credential id; nil
// when there are none.
func (r credentialsReading) usable() map[string]Secret {
	var out map[string]Secret
	for _, sc := range r.stored {
		k := strings.TrimSpace(sc.Key.Reveal())
		if keyProblem(k) != nil {
			continue
		}
		if out == nil {
			out = make(map[string]Secret, len(r.stored))
		}
		out[sc.ID] = Secret(k)
	}
	return out
}

// loadCredentials is LoadWith's reading of dir's CredentialsFile, for every
// kind of table: the reading itself (what the table reports it said,
// CredentialsReport), the usable stored keys for WebSearchKey and Keys, and
// the lines for the table's Warnings — the file made 0600 when group or
// others could read it (tighten, a regular file only, whether or not it
// parses: one craze cannot read can still hold a key, review C5 item 2; a
// link is the reading's own line), and the reading's diagnostics.
func loadCredentials(dir string) (credentialsReading, map[string]Secret, []string) {
	path := filepath.Join(dir, CredentialsFile)
	r := readCredentials(path)
	var warnings []string
	if r.has && !errors.Is(r.err, ErrSymlinkedCredentialsFile) {
		if w := tighten(path); w != "" {
			warnings = append(warnings, w)
		}
	}
	return r, r.usable(), append(warnings, r.lines(path)...)
}

// SetCredential stores key as credential id's api_key in dir's
// CredentialsFile, trimmed. An empty key, one that is not valid UTF-8, or one
// KeyProblem refuses, is refused before anything is read or written, with an
// error that names the rule and never the value (ErrEmptyKey, ErrKeyNotUTF8,
// ErrKeyTooShort, ErrKeyOverlapsMarker through errors.Is). The directory is made (0700) when
// missing. Under the file's lock: a file that cannot be parsed, a symlink and
// a non-regular file are refused (an error, nothing written), and so is an
// entry for id that is not a table (ErrCredentialNotATable); otherwise only
// id's api_key changes — every other key of its table, and every table and
// key craze does not know, is written back — and the file is written whole,
// atomically, at 0600.
func SetCredential(dir, id, key string) error {
	c, ok := webCredential(id)
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownCredential, id)
	}
	k := strings.TrimSpace(key)
	rule := keyProblem(k)
	switch {
	case k == "":
		rule = ErrEmptyKey
	case !utf8.ValidString(k):
		rule = ErrKeyNotUTF8
	}
	if rule != nil {
		return &keyRefusedError{what: c.FullName(), rule: rule}
	}
	return editCredentials(dir, true, func(path string, doc map[string]any) (bool, error) {
		tbl, err := credentialTable(path, doc, id)
		if err != nil {
			return false, err
		}
		if tbl == nil {
			tbl = map[string]any{}
			doc[id] = tbl
		}
		tbl["api_key"] = k
		return true, nil
	})
}

// RemoveCredential clears credential id's api_key in dir's CredentialsFile,
// and reports whether it held one (a non-blank string, usable or not). Only
// the key goes: a table that holds anything else keeps it, and one left
// empty is dropped. A credential with no key stored is (false, nil) with
// nothing written, as is a directory with no CredentialsFile — and then no
// directory or lock is made either. The refusals are SetCredential's.
func RemoveCredential(dir, id string) (removed bool, err error) {
	if _, ok := webCredential(id); !ok {
		return false, fmt.Errorf("%w %q", ErrUnknownCredential, id)
	}
	if dir != "" {
		if _, err := os.Lstat(filepath.Join(dir, CredentialsFile)); errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
	}
	err = editCredentials(dir, false, func(path string, doc map[string]any) (bool, error) {
		tbl, err := credentialTable(path, doc, id)
		if err != nil || tbl == nil {
			return false, err
		}
		raw, has := tbl["api_key"]
		if !has {
			return false, nil
		}
		s, isString := raw.(string)
		removed = !isString || strings.TrimSpace(s) != ""
		delete(tbl, "api_key")
		if len(tbl) == 0 {
			delete(doc, id)
		}
		return true, nil
	})
	return removed && err == nil, err
}

// localTOMLTime is a TOML local date, time or date-time, written back as it
// was read (MarshalTOML: the text, unquoted).
type localTOMLTime string

func (l localTOMLTime) MarshalTOML() ([]byte, error) { return []byte(l), nil }

// localTimeFormats are the layouts of the decoder's three local kinds, by
// the name of the location it decodes each into.
var localTimeFormats = map[string]string{
	"datetime-local": "2006-01-02T15:04:05.999999999",
	"date-local":     "2006-01-02",
	"time-local":     "15:04:05.999999999",
}

// keepLocalTimes is v, a decoded document or a value in one, with every
// local date, time and date-time replaced in place by its text as read. The
// encoder converts such a value to UTC before formatting it, which moves its
// wall clock by the machine's offset: a rewrite would change a value craze
// does not know, which the store promises never to do.
func keepLocalTimes(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = keepLocalTimes(e)
		}
	case []map[string]any:
		for _, e := range x {
			keepLocalTimes(e)
		}
	case []any:
		for i, e := range x {
			x[i] = keepLocalTimes(e)
		}
	case time.Time:
		if layout, ok := localTimeFormats[x.Location().String()]; ok {
			return localTOMLTime(x.Format(layout))
		}
	}
	return v
}

// credentialTable is id's table in doc, nil when doc has none, and
// ErrCredentialNotATable, naming the file and the key, when doc's entry for
// id is something else.
func credentialTable(path string, doc map[string]any, id string) (map[string]any, error) {
	v, has := doc[id]
	if !has {
		return nil, nil
	}
	tbl, ok := v.(map[string]any)
	if !ok {
		return nil, notATableError{notATable(path, id, "craze does not replace it — fix it by hand; nothing was written")}
	}
	return tbl, nil
}

// notATable is the value-free line for path's entry for id that is not a
// table — the file and the key, never what it holds — ending with then, what
// craze does about it.
func notATable(path, id, then string) *FileError {
	return &FileError{File: path, Key: toml.Key{id}.String(), Reason: "not a table (want [" + id + "] holding api_key); " + then}
}

// notATableError is credentialTable's refusal: a FileError, naming the file
// and the key and never a value, that is ErrCredentialNotATable.
type notATableError struct{ *FileError }

func (e notATableError) Is(target error) bool { return target == ErrCredentialNotATable }

// editCredentials is one read-modify-write of dir's CredentialsFile: the
// directory made (0700) when create says so, the lock taken (a failure to
// take it is the caller's error, never skipped), the file read leniently but
// refused whole when it cannot be read as a regular file of valid TOML (a
// missing one is empty), and — when edit, which changes doc in place, says
// there is something to write — the whole file written back atomically at
// 0600, once what is written is known to decode. An error from edit is
// returned with nothing written.
func editCredentials(dir string, create bool, edit func(path string, doc map[string]any) (bool, error)) error {
	if dir == "" {
		// paths.NativeDir is "" with no home directory; joining "" would
		// write the file into the working directory.
		return errors.New("modeltable: no directory to keep keys in")
	}
	path := filepath.Join(dir, CredentialsFile)
	if create {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("modeltable: %w", err)
		}
	}
	unlock, err := atomicfile.Lock(filepath.Join(dir, credentialsLockName))
	if err != nil {
		return fmt.Errorf("modeltable: locking %s: %w", path, err)
	}
	defer unlock()

	doc, _, err := readCredentialsDoc(path)
	if err != nil {
		return fmt.Errorf("%w; nothing was written", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	if changed, err := edit(path, doc); err != nil || !changed {
		return err
	}
	var b bytes.Buffer
	b.WriteString(credentialsHeader)
	if err := toml.NewEncoder(&b).Encode(keepLocalTimes(doc)); err != nil {
		// Value-free: the encoder's words are not passed on.
		return fmt.Errorf("modeltable: %s could not be written back; nothing was written", path)
	}
	// What is written must read back (review C5 item 2): a document the
	// reading rejects would take every stored key — and every later login
	// and logout — with it. Value-free, as above.
	if _, err := toml.Decode(b.String(), &map[string]any{}); err != nil {
		return fmt.Errorf("modeltable: %s could not be written back as valid TOML; nothing was written", path)
	}
	if err := atomicfile.Write(path, b.Bytes(), providersPerm); err != nil {
		return fmt.Errorf("modeltable: %w", err)
	}
	return nil
}
