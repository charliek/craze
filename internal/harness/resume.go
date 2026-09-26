package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

// Resuming a stored session (plan 028 §3.3): Open with Options.Resume. The
// session continues its own transcript — the same id and file, reopened for
// appending under the store's lock — from the state the transcript's path
// records: the model, the effort and the mode, what the model was told of the
// mode, the todo list and the turn numbering. Every read walks the path, root
// to leaf (Transcript.Branch), never file order.
//
// The order is forced by the store. Its Open holds a resume entry recording
// the incarnation's contract — the rebuilt system prompt's digest and the
// tools' — so the tools must exist before it; and the tools are the header's
// profile's, which must be known before them. The header is the file's first
// line and never changes once written, so it alone is read first, unlocked
// (ReadHeader); everything else is read from the transcript as the store's
// Open read it, under the lock, which also refuses a damaged file.

// openResumed is Open for Options.Resume. s is Open's session, its runner and
// base set; everything else is filled in here.
func (s *Session) openResumed(opts Options) (_ *Session, err error) {
	id := opts.Resume
	fail := func(err error) error { return fmt.Errorf("harness: resume %s: %w", id, err) }

	// An explicit mode is checked before anything is read, as a new
	// session's is; "" is the transcript's (below).
	explicitMode := ""
	if opts.Mode != "" {
		if explicitMode, err = normalizeMode(opts.Mode); err != nil {
			return nil, err
		}
	}
	// Nothing below has a toolset to redact its errors with until the tools
	// exist, and the store's name the file, under a home that can hold
	// anything (plan 019 §3.8): this is the redactor the tools will have, over
	// every key the table knows.
	keys, err := opts.Table.Keys(s.getenv)
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = k.Reveal()
	}
	early := redact.New(vals...)

	path, err := store.Find(opts.Home, opts.Workspace, id)
	if err != nil {
		return nil, fail(redactErrWith(early, err))
	}
	head, err := store.ReadHeader(path)
	if err != nil {
		return nil, fail(redactErrWith(early, err))
	}
	profile := head.ToolProfile

	// The tools and the prompt are the header's profile's, whatever model the
	// session resumes on, and are rebuilt: the prompt is frozen per
	// incarnation (D-30), from this Open's Options.Prompt. The mode only seeds
	// the gate; the session's own is set below, before it is handed out.
	ws := filepath.Clean(opts.Workspace)
	if s.tools, err = openTools(opts.Home, ws, modeAgent, opts.Asker, opts.Table, s.getenv,
		profileRef(profile), opts.Prompt, opts.Personas, nil, s.subs, opts.tools); err != nil {
		return nil, err
	}
	s.system = s.tools.system
	// The resume entry records these two digests verbatim, as a new session's
	// header does (Open's errDigestKey).
	s.promptSHA = promptDigest(s.system)
	if s.tools.holdsKey(s.promptSHA) || (len(s.tools.wire) > 0 && s.tools.holdsKey(promptDigest(string(s.tools.wire)))) {
		return nil, errDigestKey
	}

	st, err := store.Open(store.Options{
		Home:         opts.Home,
		Workspace:    opts.Workspace,
		CrazeVersion: opts.Version,
		SystemPrompt: s.system,
		ToolProfile:  s.tools.profile,
		Tools:        s.tools.wire,
		Now:          opts.Now,
		SessionID:    id,
	}, path)
	if err != nil {
		return nil, fail(s.tools.redactErr(err))
	}
	// From here the session holds the file and its lock: every way out that
	// is not a session handed back releases both.
	defer func() {
		if err != nil {
			_ = st.Close()
		}
	}()
	// The header the tools were chosen by is the one the store read under the
	// lock: a file swapped between the two reads is not resumed on a guess.
	if got := st.Header().ToolProfile; got != profile {
		return nil, fail(fmt.Errorf("%w: the transcript's tool profile changed from %q to %q while it was reopened", store.ErrCorrupt, profile, got))
	}
	tr := st.Transcript()
	branch, err := tr.Branch(tr.Leaf())
	if err != nil { // the leaf is always known
		return nil, fail(err)
	}
	was := readPath(branch)

	m, err := s.resumeModel(opts, was)
	if err != nil {
		return nil, err
	}
	var effort string
	switch {
	case opts.Effort != "":
		effort = opts.Effort
	case was.hasEffort:
		effort = carriedEffort(was.effort, m.r)
	default:
		effort = m.r.DefaultEffort
	}
	if m, err = withEffort(m, effort); err != nil {
		return nil, err
	}

	// The mode the transcript last recorded is what the model was told of
	// (P29): a mode_change is written only with the step whose request carried
	// the notice. One this craze does not know — a newer craze's — is treated
	// as agent, with nothing to announce.
	told := modeAgent
	if was.mode != "" {
		if known, err := normalizeMode(was.mode); err == nil {
			told = known
		} else if s.warn != nil {
			s.warn(fmt.Sprintf("resume: session %s's last mode %q is not one this craze knows; continuing in agent mode", id, s.tools.redactor().String(was.mode)))
		}
	}
	mode := told
	if explicitMode != "" {
		mode = explicitMode
	}

	s.store = st
	s.cur = m
	s.turns = was.turns
	// What the transcript last recorded, not what the session opens on: a
	// model, an effort or a mode that differs is written with the next step,
	// as a live switch is (begin, recordMode). A path with none of one — every
	// entry of it trimmed — has nothing to differ from.
	s.logged = logged{model: m.id(), effort: m.effort, mode: modeAgent}
	if was.hasModel {
		s.logged.model = was.model
	}
	if was.hasEffort {
		s.logged.effort = was.effort
	}
	if was.mode != "" {
		s.logged.mode = was.mode
	}

	// The plan file is the transcript's sibling, as a new session's is, and
	// is created only when missing, in plan mode.
	plan := store.PlanPath(st.Path())
	if err := s.tools.adoptPlanPath(plan); err != nil {
		return nil, err
	}
	s.modes = resumedModes(mode, told, plan, s.tools.modeGate)
	if mode == modePlan {
		if err := store.CreatePlanFile(plan); err != nil {
			return nil, fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	}
	// The list comes back as it was written: redacted with the redactor of the
	// turn that wrote it (redactTodos), so a key in an item is the marker from
	// here on, in the list the model builds on and in the next list written.
	// That is the transcript's own rule for everything it keeps; a key learned
	// since is redacted where the list is shown (Replay). Every item the live
	// list had is there: ids that redacted alike were told apart by a suffix,
	// never dropped.
	if was.todos != nil {
		s.tools.todos.restore(toolTodos(*was.todos))
	}
	return s, nil
}

// promptDigest is the hex SHA-256 the transcript records for a text: the
// store's rule for the header's and the resume entry's digests.
func promptDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// pathState is what a transcript's path says of the session it records.
type pathState struct {
	// model and effort are the last on the path — a message entry's own, or a
	// switch's — and has* says there was one.
	model     store.Model
	hasModel  bool
	effort    string
	hasEffort bool
	// mode is the last mode_change's, "" for none.
	mode string
	// todos is the list the last tool entry that carries one recorded; nil
	// for none, which a resume restores as empty.
	todos *[]store.Todo
	// turns is how many turns the path has numbered: the largest turn it
	// records, or for a transcript written before turns were, the turns its
	// user entries are read as opening (turnReader).
	turns int
}

// readPath reads a transcript's path, root to leaf.
func readPath(path []store.Entry) pathState {
	var ps pathState
	var turns turnReader
	for i := range path {
		e := &path[i]
		switch e.Type {
		case store.TypeModelChange:
			ps.model, ps.hasModel = e.Model, true
		case store.TypeEffortChange:
			ps.effort, ps.hasEffort = e.Effort, true
		case store.TypeModeChange:
			ps.mode = e.Mode
		case store.TypeMessage:
			// Every message entry records the model and the effort its turn
			// ran on.
			ps.model, ps.hasModel = e.Model, true
			ps.effort, ps.hasEffort = e.Effort, true
			if e.Message.Role == fantasy.MessageRoleTool && e.Todos != nil {
				ps.todos = e.Todos
			}
		}
		turns.read(e)
	}
	ps.turns = turns.numbered()
	return ps
}

// turnReader tells, along a path, the user entries that open a turn from the
// steers interjected into one (plan 028 §3.2, §3.4, P10). An entry that opens
// turn N records it (MessageEntry.Turn); from the first entry a turn-aware
// craze wrote — one such entry, or the resume entry an incarnation writes
// before its first step — every user entry with no turn that is not a
// results entry is a steer. Before that, in a transcript written before turns
// were recorded, it is inferred: a user entry is a steer when the message
// entry before it on the path is a tool entry — a steer is taken up only at a
// step boundary, after the step's results — or another steer, as a second
// steer of one step follows the first.
type turnReader struct {
	aware bool // a turn-aware craze wrote the entries from here on
	// prev is the last message entry read: prevTool, prevSteer, or prevOther
	// (a prompt, results, an answer, or nothing yet).
	prev     int
	largest  int // the largest turn recorded
	inferred int // the turns inferred before aware
}

const (
	prevOther = iota
	prevTool
	prevSteer
)

// read takes the next entry on the path, and reports, for a user entry that
// is not a results entry, whether it is a steer; results reports a results
// entry (subagent_results), which is neither a prompt nor a steer.
//
// Every turn an entry records counts toward the largest, whatever the entry:
// a prompt's, a wake's results entry — which opens the wake's turn (a
// mid-turn results entry records none) — and, from plan 028's PR 2, a
// compaction's. Numbering on from anything less would reuse a turn's number,
// and its call ids and spill files, after a resume (§3.3 item 9).
func (r *turnReader) read(e *store.Entry) (steer, results bool) {
	if e.Turn > 0 {
		r.aware = true
		r.largest = max(r.largest, e.Turn)
	}
	switch {
	case e.Type == store.TypeResume:
		r.aware = true
		return false, false
	case e.Type != store.TypeMessage:
		return false, false
	case e.Message.Role == fantasy.MessageRoleTool:
		r.prev = prevTool
		return false, false
	case e.Message.Role != fantasy.MessageRoleUser:
		r.prev = prevOther
		return false, false
	case e.SubagentResults:
		r.prev = prevOther
		return false, true
	}
	switch {
	case e.Turn > 0: // opens its turn, counted above
	case r.aware:
		steer = true
	case r.prev == prevTool || r.prev == prevSteer:
		steer = true
	default:
		r.inferred++
	}
	r.prev = prevOther
	if steer {
		r.prev = prevSteer
	}
	return steer, false
}

// numbered is the turn the path's last numbered one was: the largest
// recorded, and never fewer than were inferred before turns were recorded
// (the incarnation that started recording them numbered on from those).
func (r *turnReader) numbered() int { return max(r.largest, r.inferred) }

// resumeModel is the model a resumed session continues on (plan 028 §3.3,
// PD6, P8), the first of these that resolves — its provider has a key, and it
// gets the session's tool profile, its header's:
//
//   - Options.Model, when the caller set it: explicit, so one that does not
//     resolve refuses Open, as it does a new session's;
//   - the transcript's last model, by identity (provider, wire model): its
//     own alias when that still names it, then every other alias that does,
//     in sorted order — never an alias that has been re-pointed at another
//     model, which would switch models under the conversation (D-33);
//   - the table's default model.
//
// None is ErrResumeModel, naming why each failed. Landing anywhere but on the
// transcript's own model is reported to Options.Warn.
func (s *Session) resumeModel(opts Options, was pathState) (model, error) {
	table := opts.Table
	if opts.Model != "" {
		return s.eligible(table, opts.Model)
	}
	var candidates []string
	if was.hasModel {
		same := func(alias string) bool {
			m, ok := table.Models[alias]
			return ok && m.Provider == was.model.Provider && m.WireModel == was.model.WireModel
		}
		if same(was.model.Alias) {
			candidates = append(candidates, was.model.Alias)
		}
		for _, alias := range slices.Sorted(maps.Keys(table.Models)) {
			if alias != was.model.Alias && same(alias) {
				candidates = append(candidates, alias)
			}
		}
	}
	own := len(candidates)
	if def := table.DefaultModel; def != "" && !slices.Contains(candidates, def) {
		candidates = append(candidates, def)
	}
	var why []string
	for i, alias := range candidates {
		m, err := s.eligible(table, alias)
		if err != nil {
			why = append(why, err.Error())
			continue
		}
		if was.hasModel && i >= own && s.warn != nil {
			reason := "no alias in the model table names it"
			if own > 0 {
				reason = strings.Join(why[:own], "; ")
			}
			s.warn(s.tools.redactor().String(fmt.Sprintf("resume: session %s's model %s (%s, %s) is not available (%s); continuing on %s",
				opts.Resume, was.model.Alias, was.model.Provider, was.model.WireModel, reason, alias)))
		}
		return m, nil
	}
	reason := "the transcript names no model the table has, and the table has no default"
	if len(why) > 0 {
		reason = strings.Join(why, "; ")
	}
	return model{}, s.tools.redactErr(fmt.Errorf("%w (tool profile %q): %s", ErrResumeModel, s.tools.profile, reason))
}

// eligible builds alias's model when the session may run on it: the alias
// resolves, with its provider's key, and gets the session's tool profile. It
// resolves and checks before it builds the client, so a model the session
// cannot take is never built.
func (s *Session) eligible(table *modeltable.Table, alias string) (model, error) {
	r, err := table.Resolve(alias, s.getenv)
	if err != nil {
		return model{}, fmt.Errorf("harness: %w", err)
	}
	if err := s.tools.check(r); err != nil {
		return model{}, err
	}
	lm, err := s.newModel(r)
	if err != nil {
		return model{}, fmt.Errorf("harness: model %q: %w", alias, err)
	}
	return model{r: r, lm: lm}, nil
}
