package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/paths"
)

// skillScanner keeps one live session's disk skills current for its slash
// catalog (plan 043 P5, D2). The scan is a filesystem walk (DiscoverSkills,
// bounded at 256 SKILL.md candidates, depth 10, 1 MiB each), so it never runs
// under a lock of the session's and never on the ACP reader; a trigger only
// asks for one, and returns at once.
//
//   - **Single flight.** At most one scan runs, on a goroutine of its own.
//   - **Coalesced.** A request while a scan runs sets dirty and returns; when
//     the scan ends, a dirty scanner runs exactly one more, however many
//     requests came in meanwhile — the newest of them is satisfied by a scan
//     that began after it.
//   - **Generation-numbered.** Each scan takes the next generation when it
//     starts, and apply is handed it with the result: the session applies a
//     result only if its generation is newer than the last one it applied,
//     under s.mu, so an older result can never land over a newer one
//     (session.applySkills).
//   - **Armed.** Requests before arm are remembered, not run: the session arms
//     its scanner once its install has published the catalog the first scan's
//     result is compared against. A request made during Start — an attach
//     while the host's session is still starting, an early catalog update —
//     becomes one scan after the install.
//   - **Closed.** close cancels the scan's context (the walk stops at its next
//     entry) and refuses every later request; a result that comes back after
//     it is dropped by the session, which checks s.closed under s.mu. Close
//     does not wait for the goroutine: a walk stuck in a stat on a dead mount
//     must not hold a session's Close, and nothing it could still do after
//     close is published.
//
// mu is a leaf: it is held only to read and write the fields below, never
// across a scan, an apply or any other lock, and the session takes it with s.mu
// released.
type skillScanner struct {
	scan  func(ctx context.Context) []Skill
	apply func(gen uint64, skills []Skill)

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	armed   bool
	running bool
	dirty   bool
	closed  bool
	// gen is the generation of the newest scan started.
	gen uint64
}

func newSkillScanner(scan func(ctx context.Context) []Skill, apply func(gen uint64, skills []Skill)) *skillScanner {
	ctx, cancel := context.WithCancel(context.Background())
	return &skillScanner{scan: scan, apply: apply, ctx: ctx, cancel: cancel}
}

// request asks for a scan and returns at once (SkillRescanner's contract): it
// starts one when the scanner is armed and idle, and marks one owed otherwise.
func (sc *skillScanner) request() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return
	}
	if !sc.armed || sc.running {
		sc.dirty = true
		return
	}
	sc.startLocked()
}

// arm lets requests run from now on, and runs the one a request before it
// left owed.
func (sc *skillScanner) arm() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed || sc.armed {
		return
	}
	sc.armed = true
	// Nothing can be running yet: only an armed scanner starts a scan.
	if sc.dirty {
		sc.startLocked()
	}
}

// startLocked starts a scan under the next generation. sc.mu is held.
func (sc *skillScanner) startLocked() {
	sc.running, sc.dirty = true, false
	sc.gen++
	go sc.run(sc.gen)
}

// run is the one flight: scan, apply, and scan again for as long as a request
// came in while it was scanning.
func (sc *skillScanner) run(gen uint64) {
	for {
		skills := sc.scan(sc.ctx)
		if sc.ctx.Err() == nil {
			sc.apply(gen, skills)
		}
		sc.mu.Lock()
		if sc.closed || !sc.dirty {
			sc.running = false
			sc.mu.Unlock()
			return
		}
		sc.dirty = false
		sc.gen++
		gen = sc.gen
		sc.mu.Unlock()
	}
}

// close refuses every later request and cancels the scan in flight, if any.
func (sc *skillScanner) close() {
	sc.mu.Lock()
	sc.closed = true
	sc.mu.Unlock()
	sc.cancel()
}

// knownProviderKeys is every key craze knows of, read afresh for each scan so
// a key stored since the last one is covered by the next. It is never weaker
// than a native session's redactor (plan 043 P18; astra C1 H9), whose sources
// it takes one by one:
//
//   - the model table's keys (nativeTableKeys, modeltable.Table.Keys): every
//     provider's env_keys values, its inline api_key, and every other
//     credential variable the merge saw declared — what harness.Open builds a
//     native session's redactor from;
//   - the keys stored in providers.toml (modeltable.StoredKeys) — where
//     `craze auth` and /connect keep a key — read on their own, as a native
//     session learns them at every turn's start (learnStoredKeys), so a key the
//     merge dropped with its entry is covered too;
//   - the ChatGPT plan's token values (chatgptauth.StoredValues), which a
//     native session learns the same way.
//
// Each source fails alone, and none of them takes the others with it: a table
// that does not load — a models.toml that is not TOML, say — is no reason to
// stop redacting a key the environment holds. So the shipped catalog's own
// credential variables (modeltable.CatalogEnvNames) are read whatever the
// user's files say: a key exported under one of them is a key whether or not
// the table merges. Each value is held to the rule a native session learns a
// key by (modeltable.KeyProblem, after trimming), so nothing too short to
// redact safely joins the set.
func knownProviderKeys() []string {
	var keys []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && modeltable.KeyProblem(v) == nil && !slices.Contains(keys, v) {
			keys = append(keys, v)
		}
	}
	for _, name := range modeltable.CatalogEnvNames() {
		add(os.Getenv(name))
	}
	dir := paths.NativeDir()
	if dir == "" {
		// No craze directory: the environment is all there is. The two file
		// readers below would read a relative path from the working directory.
		return keys
	}
	if table, err := modeltable.Load(dir); err == nil {
		for _, k := range nativeTableKeys(table, os.Getenv) {
			add(k)
		}
	}
	if stored, err := modeltable.StoredKeys(dir); err == nil {
		for _, sk := range stored {
			add(sk.Key.Reveal())
		}
	}
	if values, err := chatgptauth.StoredValues(dir); err == nil {
		for _, v := range values {
			add(v)
		}
	}
	return keys
}

// redactSkills is the redaction native applies to its own entries before
// anything is stored (native.go's Start, redactNativeEntries), applied to an
// ACP session's disk skills for the same reason (plan 043 P18): a skill's
// description travels into the snapshot, the stream and the journal, and one
// with no frontmatter description has one taken from its body, where a
// provider key can be.
//
// The description is redacted, folded onto one line, and redacted again — the
// native session's own rule for a line (nativeSafe.line): the catalog's
// builder folds it with DisplayLine, which strips an escape sequence, and a key
// split by one would otherwise come back whole after the first pass had missed
// it. A skill whose name holds a key is dropped, as native drops an entry
// whose identity holds one (dropKeyBearing): the name is what the row runs, so
// redacting it would change the command (P7's never-cut rule), and keeping it
// would print the key.
func redactSkills(skills []Skill, keys []string) []Skill {
	if len(keys) == 0 || len(skills) == 0 {
		return skills
	}
	red := redact.New(keys...)
	out := make([]Skill, 0, len(skills))
	for _, sk := range skills {
		if holdsNativeKey(sk.Name, keys) {
			continue
		}
		sk.Description = red.String(DisplayLine(red.String(sk.Description)))
		out = append(out, sk)
	}
	return out
}
