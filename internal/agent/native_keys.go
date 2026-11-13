package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// A running native session learns the keys stored in its providers.toml after
// it opened, so that it can redact them (plan 031 §3.8, P8). Whether the
// session also offers what such a key funds is the model table's reload
// (native_models.go, plan 034 §3.4), which runs right after this look at a
// turn's start; this look is about every stored key the session can meet — a
// tool that reads the file, a command that prints the key, a prompt it is
// pasted into — whether it is one the session sends or not, and nothing else
// would ever redact it.
//
// So every turn this adapter runs takes a look first, before anything of the
// turn is redacted or sent: an ordinary prompt, a /compact, and a wake (every
// path on which the harness takes a redactor up, its begin). The look is a
// stat of providers.toml in the Home the adapter opened the harness with —
// never paths.NativeDir() again, which a test's seam or a later environment
// can point elsewhere (panel astra 12) — and, when the file's stamp (size,
// modification time, inode) differs from the one of the last successful
// reading, a reading of the file's inline keys alone (modeltable.StoredKeys:
// no catalog, no models.toml, no merge), every one of them handed to the
// harness's LearnKeys, which judges each, adds the new ones to the session's
// redactor from this turn on, and refuses every turn from here if one is in
// the session's frozen prompt (harness.ErrStoredKeyFrozen). The first turn's
// look always reads. The keys the file held when the table was loaded are
// known already — learning them again changes nothing but what a sub-agent
// opened later starts with — all but one the merge dropped with its entry (an
// incomplete provider of the user's own, say), which is never sent either and
// which this is how the session first learns to redact.
//
// A shell or a sub-agent already running keeps the redactor it began with
// (R1), and a key stored during a turn is learned at the next: the residual
// window the configuration reference documents. So is a stamp that cannot see
// a change — a rewrite in place, to the same size, within one tick of the
// file system's clock of the last reading — which waits for the file's next
// change.
//
// The same look reads the ChatGPT plan's token file beside it (plan 033
// §3.12, P19): native/auth/chatgpt.json, whose access, refresh and id tokens
// every native session — on any provider, the plan's or not — learns the same
// way, stamp-gated, through LearnKeys. A session on another provider never
// asks the token source for a token, so it would otherwise never learn one,
// and a `cat` of the file would show the model what any process's refresh
// wrote. A rotation by this process is learned sooner, mid-turn, through the
// token source's subscription (AddSecrets); one by another process is learned
// here, at the next turn's start (R9: the cross-process mid-turn window).
//
// And the same look reads the web search's stored keys beside them (plan 051
// §F.5): native/credentials.toml, read leniently and stamp-gated, whatever
// the model table's state, every value handed to LearnKeys — so a key stored
// or rotated by `craze auth login websearch/exa` while a session runs is
// redacted from the next turn on, in the session and its live children, and
// refuses every turn if it is in the frozen prompt. Only that: the session's
// web service sends the key it resolved at Open until the session opens again
// (R3), and the model table's reload does not watch the file.

// storedKeys is the session's look at its providers.toml. mu serializes the
// looks — a turn's and a wake's are already one at a time behind the claim,
// and this does not lean on that — and guards the rest. It is never taken
// under a lock of the adapter's, and nothing under it takes one: only the
// harness's own leaf locks, through LearnKeys and Redact.
type storedKeys struct {
	mu sync.Mutex
	// home is the Home the harness was opened with (open(), after the seam):
	// the directory whose providers.toml is watched. "" watches nothing.
	home string
	// providers, tokens and creds are the looks at providers.toml, the
	// ChatGPT plan's token file and the stored web search keys
	// (credentials.toml, plan 051 §F.5), each its own.
	providers, tokens, creds fileLook
	// credShapes are the value-free lines last said for credentials.toml's
	// entries of the wrong shape (modeltable.StoredCredentialsAndProblems),
	// joined: a reading that finds the same ones says nothing, so each is
	// said once while it lasts (review C5 item 6). The load's own, said among
	// the table's warnings at Open, seed it.
	credShapes string
}

// fileLook is one file's look. stamp is the file as it was stat'ed before the
// last reading that succeeded, and read says there was one. The stamp is
// taken BEFORE the reading, so a write that lands between the two leaves the
// recorded stamp older than the file, and the next look reads again; taken
// after, that write's stamp would be recorded with the older content's keys,
// and its key never learned. problem is the last diagnostic said, so a file
// that stays unreadable is said once rather than at every turn; a reading
// that succeeds clears it.
type fileLook struct {
	stamp   fileStamp
	read    bool
	problem string
}

// say notes msg unless it is l's last diagnostic, which it then is.
func (l *fileLook) say(note func(string), msg string) {
	if msg == l.problem {
		return
	}
	l.problem = msg
	note(msg)
}

// due stamps path and reports whether it is to be read now, with the stamp
// done records once the reading succeeds. It is not when the file is missing
// — nothing is stored, and a file created later is read whatever its stamp:
// a removed file's inode can be the next file's — nor when it cannot be
// stat'ed (one line, through say, that it cannot be checked for what), nor
// when its stamp is the last successful reading's.
func (l *fileLook) due(path, what string, note func(string)) (fileStamp, bool) {
	stamp, err := stampOf(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		l.read = false
		return stamp, false
	case err != nil:
		l.say(note, fmt.Sprintf("cannot check %s for %s since this session started: %v", path, what, err))
		return stamp, false
	case l.read && stamp == l.stamp:
		return stamp, false
	}
	return stamp, true
}

// done records a reading that succeeded of the file as stamp found it, and
// clears its diagnostic.
func (l *fileLook) done(stamp fileStamp) { l.stamp, l.read, l.problem = stamp, true, "" }

// fileStamp is what a look compares: a file's size, its modification time in
// nanoseconds and its inode — which an atomic replace (write a temporary file,
// rename it over) always changes, whatever it does to the other two.
type fileStamp struct {
	size  int64
	mtime int64
	ino   uint64
}

// stampOf is path's stamp, following a symlink to the file it names: a
// providers.toml that is a link is read through it, so its target's changes
// are what count. The inode comes from syscall.Stat_t, which Linux and macOS —
// the two systems craze is built for — both give os.Stat's Sys.
func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	st := fileStamp{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino // uint64 on Linux and on macOS
	}
	return st, nil
}

// watch sets the directory the looks read, once the harness's Home is known.
func (k *storedKeys) watch(home string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.home = home
}

// learnStoredKeys is one look (the file's comment), made at a turn's start
// with no lock of the adapter's held, before the turn redacts or sends
// anything: hs is the harness the turn runs on. Every diagnostic it writes is
// one line through s.note that names a file, a provider id or a rule — never
// a value — redacted by the session's own redactor after this look's learning
// and sanitized, since a provider id is text from a file. It looks at
// providers.toml, then at the ChatGPT plan's token file, then at the stored
// web search keys.
func (s *nativeSession) learnStoredKeys(hs *harness.Session) {
	k := &s.keys
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.home == "" {
		return
	}
	note := func(msg string) { s.note(nativeSafe{red: hs.Redact}.line(msg)) }
	s.lookAtProviders(hs, note)
	s.lookAtTokens(hs, note)
	s.lookAtCredentials(hs, note)
}

// lookAtProviders is the look at providers.toml. k.mu is held.
func (s *nativeSession) lookAtProviders(hs *harness.Session, note func(string)) {
	k := &s.keys
	path := filepath.Join(k.home, modeltable.ProvidersFile)
	stamp, due := k.providers.due(path, "keys stored", note)
	if !due {
		return
	}
	stored, err := modeltable.StoredKeys(k.home)
	if s.keysSeam != nil {
		s.keysSeam()
	}
	if err != nil {
		// One value-free line (modeltable's errors name a file and a line,
		// table or key, never a value), and nothing learned: the stamp stays
		// unrecorded, so the next look reads the file again.
		k.providers.say(note, fmt.Sprintf("%s changed, but its keys cannot be read (%v); this session learns none of them until it is fixed", path, err))
		return
	}
	k.providers.done(stamp)
	keys := make([]modeltable.Secret, len(stored))
	for i, sk := range stored {
		keys[i] = sk.Key
	}
	skipped, err := hs.LearnKeys(keys)
	for i, problem := range skipped {
		if problem != nil {
			note(fmt.Sprintf(
				"%s: provider %q: its api_key %s, so it cannot be an API key; this session ignores it, and the next one will refuse the file until it is fixed",
				path, stored[i].Provider, modeltable.KeyRule(problem)))
		}
	}
	if errors.Is(err, harness.ErrStoredKeyFrozen) {
		note(fmt.Sprintf(
			"a key stored in %s since this session started appears in its frozen prompt; every turn from now on is refused — start a new session",
			path))
	}
}

// lookAtTokens is the look at the ChatGPT plan's token file (plan 033 §3.12):
// stamp-gated like the look at providers.toml, and every value the file holds
// — the access, refresh and id tokens (chatgptauth.StoredValues, which reads
// no network and takes no lock) — handed to LearnKeys. A file that cannot be
// read is one diagnostic, said once, naming the file and never a value, and
// nothing is learned until it can be. k.mu is held.
func (s *nativeSession) lookAtTokens(hs *harness.Session, note func(string)) {
	k := &s.keys
	path := chatgptauth.TokenFile(k.home)
	stamp, due := k.tokens.due(path, "ChatGPT tokens written", note)
	if !due {
		return
	}
	values, err := chatgptauth.StoredValues(k.home)
	if err != nil {
		k.tokens.say(note, fmt.Sprintf("%s changed, but its tokens cannot be read (%v); this session learns none of them until it is fixed", path, err))
		return
	}
	k.tokens.done(stamp)
	keys := make([]modeltable.Secret, len(values))
	for i, v := range values {
		keys[i] = modeltable.Secret(v)
	}
	if _, err := hs.LearnKeys(keys); errors.Is(err, harness.ErrStoredKeyFrozen) {
		note(fmt.Sprintf(
			"a ChatGPT token in %s appears in this session's frozen prompt; every turn from now on is refused — start a new session",
			path))
	}
}

// credentialsUnreadable is the look's one line for a credentials.toml it
// cannot read: value-free, as StoredCredentials' error is.
func credentialsUnreadable(path string, err error) string {
	return fmt.Sprintf("%s cannot be read (%s); this session learns none of its keys until it is fixed",
		path, strings.TrimPrefix(err.Error(), "modeltable: "))
}

// openedWithCredentials records, at Open, what table — just loaded from
// k.home — already said of its credentials.toml in its warnings
// (modeltable.Table.CredentialsReport): a file the look cannot read is then
// not said again by the first turn's look, only once it changes and is still
// unreadable, or is read; and neither is an entry of the wrong shape. It is
// the load's own reading, never a second one (review C5 item 6): a file
// broken after the load was never said, and the first look says it.
func (k *storedKeys) openedWithCredentials(table *modeltable.Table) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.home == "" {
		return
	}
	problems, err := table.CredentialsReport()
	if err != nil {
		k.creds.problem = credentialsUnreadable(filepath.Join(k.home, modeltable.CredentialsFile), err)
	}
	k.credShapes = strings.Join(problems, "\n")
}

// lookAtCredentials is the look at the stored web search keys,
// credentials.toml (plan 051 §F.5): stamp-gated like the other two, whether
// or not the model table loads, and every key the file stores
// (modeltable.StoredCredentials, a lenient reading) handed to LearnKeys —
// for redaction and the frozen-prompt check only: the session's web service
// keeps the key it opened with (R3), and a key stored now is sent from the
// next session. A file that cannot be read — not valid TOML, a symlink, not a
// regular file — is one diagnostic, said once, naming the file and never a
// value, and nothing is learned from it until it can be read; an entry of the
// wrong shape in a file read is one value-free line, said once while it
// lasts (credShapes). k.mu is held.
func (s *nativeSession) lookAtCredentials(hs *harness.Session, note func(string)) {
	k := &s.keys
	path := filepath.Join(k.home, modeltable.CredentialsFile)
	stamp, due := k.creds.due(path, "web search keys stored", note)
	if !due {
		return
	}
	stored, shapes, err := modeltable.StoredCredentialsAndProblems(k.home)
	if err != nil {
		k.creds.say(note, credentialsUnreadable(path, err))
		return
	}
	k.creds.done(stamp)
	// An entry of the wrong shape — an api_key replaced by an array — is no
	// key, and is said, value-free, once while it lasts (review C5 item 6).
	if joined := strings.Join(shapes, "\n"); joined != k.credShapes {
		k.credShapes = joined
		for _, line := range shapes {
			note(line)
		}
	}
	keys := make([]modeltable.Secret, len(stored))
	for i, sc := range stored {
		keys[i] = sc.Key
	}
	skipped, err := hs.LearnKeys(keys)
	for i, problem := range skipped {
		if problem != nil {
			note(fmt.Sprintf("%s: [%s] api_key %s, so it cannot be an API key; this session ignores it", path, stored[i].ID, modeltable.KeyRule(problem)))
		}
	}
	if errors.Is(err, harness.ErrStoredKeyFrozen) {
		note(fmt.Sprintf(
			"a web search key stored in %s since this session started appears in its frozen prompt; every turn from now on is refused — start a new session",
			path))
	}
}
