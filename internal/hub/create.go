package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/version"
)

// session.create (plan 032 §3.10, P5): a new session in a directory, started
// in a host the hub spawns, and answered once it has started.
//
// # Params
//
// cwd (absolute, an existing directory), and optionally prompt, provider,
// model, effort, fast, permissionMode and requestId (parseCreate). The
// provider is the configured default when absent (Creates.DefaultProvider,
// read at this create), and none is bad_request; the permission mode is a
// plain launch's, bypass. Never an agent binary (SD-16): the host finds its
// own — CRAZE_AGENT_BIN is not in its environment (ChildEnv), so it is
// `[agents]` or PATH.
//
// # A create, in order (hub.create)
//
//  1. A host is spawned through internal/hostspawn (StartCmd): `craze serve`
//     with --workspace, --provider and the settings asked for, --no-host-status,
//     and --request-id and --request-hash when the create has an id, which the
//     host writes into its registry entry; the environment contract's
//     environment (ChildEnv); its working directory the session's. Its ready
//     line is read (hostspawn.ReadReady). A host that does not start, or does
//     not answer ok, is unavailable, reason spawn_failed — the host
//     terminated and reaped (Settle, Terminate).
//  2. The hub dials it as a client of its own and attaches with when: ready
//     (remote.Session.Start), waiting for the session's start at most
//     createStartWait: a start that fails, or that takes longer, is
//     not_accepting, reason start_failed, data.cause the host's first error
//     line, and the hub stops that host — session.stop, then, its own child,
//     terminated (abandonHost). A host that cannot be dialled, or that goes
//     before its session is up, is spawn_failed, and stopped the same way.
//  3. A prompt, when there is one, is sent as the list's dispatch sends one
//     (internal/tui's runDispatch, over the same remote.Session): queued,
//     with the connection's first command id, waited for at most
//     createPromptWait. Taken is accepted; no answer — the deadline, or the
//     connection lost after it was sent — is unknown; anything else is the
//     session's refusal, refused, and the session runs on, idle.
//  4. The session's row is read from its host (sessions.list), so the
//     result's roster row is fresh — approximate false — and the hub
//     detaches. The created host runs on, as any detached host does: its
//     own idle exit is its end. It persists its provider as the next plain
//     launch's default itself, as every new session's host does (owner
//     decision 1, craze serve's start).
//
// # Idempotency
//
// A requestId is kept with a hash of the create's normalized params
// (createHash) — in memory (createTable: a create in flight never evicted,
// one done for createKeep, at most createKeepMax of them) and in the new
// host's registry entry. A request with the id of one in memory and the same
// hash joins it — its answer, whether it is done or not; another hash is
// bad_request, reason request_conflict. One the memory does not hold — this
// hub restarted since, or forgot it — is looked for in the registry: a live
// host whose entry carries the id and has a session is joined under the same
// contract (hub.join) — its start waited for at most createStartWait from
// the join, which answers at once for a session already started (R3-2): its
// row, its prompt unknown (this hub never saw the answer); a start that
// failed, or that does not end in time, is start_failed, and the hub stops
// that host. Another hash there is request_conflict too. A retry after the
// created session has ended finds nothing, and creates another.
//
// A create runs on a goroutine of its own (creator.run): a waiter that goes
// away does not cancel it. A create in flight keeps the hub from its idle
// exit (lifecycle.creates); more than createsMax at once is unavailable,
// reason busy. The teardown gives the creates in flight its own bound
// (teardownBound) and then cuts them (creator.cut): their waiters see the
// connection close, and a host already started runs on, its requestId
// intact.

// Creates is what session.create needs (Options.Creates): the CLI's view of
// the config file and the providers, so the hub imports neither. nil is a
// hub that creates nothing — its hello says sessionCreate false, and
// session.create is refused unsupported.
type Creates struct {
	// DefaultProvider is the provider a create that names none starts: the
	// config file's, read at each create; "" for none, which refuses it.
	DefaultProvider func() string
	// KnownProvider says a provider id names one this build can start.
	KnownProvider func(string) bool
}

// HostCommand builds a created session's host command, `craze serve …`, from
// argv (its first word "serve"). nil — production — is hostspawn.Command
// (os.Executable() re-executed); in a test binary nil is no host at all (the
// create is refused spawn_failed), and a test that creates installs its own:
// a test binary never runs itself as a host by accident. The create sets the
// rest: its environment (from cmd.Env, or this process's own, through
// ChildEnv), its working directory, its session, stdio and ready pipe.
var HostCommand func(argv []string) (*exec.Cmd, error)

// errNoHostCommand is a create in a test binary that has not installed
// HostCommand.
var errNoHostCommand = errors.New("hub: no session host in a test binary unless the test installs hub.HostCommand")

// session.create's bounds: variables only so a test can change them (never in
// parallel).
var (
	// createStartWait bounds a session's start, from its attach — the list's
	// dispatch bound — and a joined create's wait, from the join.
	createStartWait = 60 * time.Second
	// createPromptWait bounds the first prompt's answer: the command gate's
	// deadline (internal/tui's gateDeadline).
	createPromptWait = 15 * time.Second
	// createDialWait bounds the dial and hello of a created host.
	createDialWait = 5 * time.Second
	// createStopWait bounds the session.stop sent to a host the hub gives
	// up on, its dial included.
	createStopWait = 2 * time.Second
	// createReadWait bounds the read of a created session's row.
	createReadWait = 2 * time.Second
	// createDetachWait bounds the detach once a create is done.
	createDetachWait = 3 * time.Second
	// createJoinWait bounds the wait for one of a create's own goroutines
	// once its connection is closed (the prompt's, the stream's drain).
	createJoinWait = time.Second
	// createsMax is how many creates may be in flight at once.
	createsMax = 16
	// createKeep is how long a done create's answer is kept for its
	// requestId; createKeepMax how many are.
	createKeep    = 10 * time.Minute
	createKeepMax = 256
)

// createClient is who the hub says it is to a host it created.
var createClient = protocol.ClientInfo{Kind: "hub", Name: "craze hub create", Version: version.Version}

// createCommandID is the command id the first prompt goes with: the first of
// the hub's own connection to the host.
const createCommandID = "1"

// creator is the hub's session.create: its options, its table of creates by
// requestId, and the teardown's cut.
type creator struct {
	h *hub
	o Creates

	// ctx is every create's, cancelled at the teardown's cut; cut is closed
	// with it, for the waiters.
	ctx    context.Context
	cancel context.CancelFunc
	cut    chan struct{}
	once   sync.Once

	mu    sync.Mutex
	calls map[string]*createCall
}

func newCreator(h *hub, o Creates) *creator {
	ctx, cancel := context.WithCancel(context.Background())
	return &creator{h: h, o: o, ctx: ctx, cancel: cancel, cut: make(chan struct{}), calls: map[string]*createCall{}}
}

// stop is the teardown's cut: every create still in flight stops waiting —
// it leaves its host as it is — and every waiter stops waiting for it.
func (cr *creator) stop() {
	cr.once.Do(func() {
		cr.cancel()
		close(cr.cut)
	})
}

// createCall is one create: in flight until done is closed, then its answer.
// Every request carrying its requestId with the same params joins it.
type createCall struct {
	id, hash string
	done     chan struct{}
	// ans is the answer, written before done is closed; at, finished under
	// the creator's lock, when it was.
	ans      createAnswer
	at       time.Time
	finished bool
}

// createAnswer is a create's answer: its result, or its refusal.
type createAnswer struct {
	res *protocol.CreateResult
	err *protocol.Error
}

// wait is a waiter's wait for c: its answer, or false once the teardown has
// cut it — the waiter's connection closes unanswered.
func (c *createCall) wait(cut <-chan struct{}) (createAnswer, bool) {
	select {
	case <-c.done:
		return c.ans, true
	case <-cut:
	}
	select {
	case <-c.done:
		return c.ans, true
	default:
		return createAnswer{}, false
	}
}

// create answers session.create (the file's comment) on the connection's own
// goroutine, which waits for the create's answer — the create itself runs on
// its own (creator.run), so a waiter that goes does not cancel it.
func (c *conn) create(req *request) bool {
	cr := c.s.h.cr
	if cr == nil {
		return c.replyErr(req.id, refused(protocol.CodeUnsupported, protocol.ReasonUnsupported,
			"%s is not served by this hub: it has no way to start a session's host", req.method))
	}
	p, perr := cr.parse(req.params)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	call, perr := cr.admit(p)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	ans, ok := call.wait(cr.cut)
	if !ok {
		return false
	}
	if ans.err != nil {
		return c.replyErr(req.id, ans.err)
	}
	return c.reply(req.id, ans.res)
}

// ------------------------------------------------------------------ params

// createReq is one create's params, normalized: the cwd cleaned, the
// permission mode settled (bypass when absent), the provider as the client
// sent it ("" for none) beside the one it resolves to, and their hash.
type createReq struct {
	p        protocol.CreateParams
	provider string
	hash     string
}

// createMembers is every member session.create's params may carry.
var createMembers = []string{"cwd", "prompt", "provider", "model", "effort", "fast", "permissionMode", "requestId"}

// parse holds raw to session.create's params (the schema's rules, and the
// hub's own: cwd an existing directory, the provider one this build knows,
// no control character in a value that becomes an argument) and resolves the
// provider: bad_request otherwise, reason unknown_field for a member it does
// not define.
func (cr *creator) parse(raw json.RawMessage) (createReq, *protocol.Error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || string(b) == "null" {
		return createReq{}, badParams("params.cwd is required: the session's directory")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return createReq{}, badParams("params must be an object")
	}
	var unknown []string
	for k := range m {
		if !slices.Contains(createMembers, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return createReq{}, &protocol.Error{Code: protocol.RPCInvalidParams, Message: "unknown field params." + unknown[0],
			Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonUnknownField}}
	}
	var q protocol.CreateParams
	str := func(key string, dst *string, maxChars int, required bool) *protocol.Error {
		v, ok := m[key]
		if !ok {
			if required {
				return badParams("params.%s is required", key)
			}
			return nil
		}
		if string(bytes.TrimSpace(v)) == "null" || json.Unmarshal(v, dst) != nil {
			return badParams("params.%s must be a string", key)
		}
		switch {
		case *dst == "":
			return badParams("params.%s must not be empty", key)
		case maxChars > 0 && utf8.RuneCountInString(*dst) > maxChars:
			return badParams("params.%s is longer than %d characters", key, maxChars)
		}
		return nil
	}
	for _, f := range []struct {
		key      string
		dst      *string
		max      int
		required bool
	}{
		{"cwd", &q.Cwd, 4096, true},
		{"prompt", &q.Prompt, 0, false},
		{"provider", &q.Provider, 64, false},
		{"model", &q.Model, 256, false},
		{"effort", &q.Effort, 64, false},
		{"requestId", &q.RequestID, 0, false},
	} {
		if perr := str(f.key, f.dst, f.max, f.required); perr != nil {
			return createReq{}, perr
		}
	}
	if v, ok := m["fast"]; ok {
		var fast bool
		if string(bytes.TrimSpace(v)) == "null" || json.Unmarshal(v, &fast) != nil {
			return createReq{}, badParams("params.fast must be true or false")
		}
		q.Fast = &fast
	}
	if v, ok := m["permissionMode"]; ok {
		var mode string
		if json.Unmarshal(v, &mode) != nil || !slices.Contains(protocol.PermissionModes(), protocol.PermissionMode(mode)) {
			return createReq{}, badParams("params.permissionMode must be bypass or prompt")
		}
		q.PermissionMode = protocol.PermissionMode(mode)
	}
	if q.PermissionMode == "" {
		q.PermissionMode = protocol.PermissionBypass
	}
	if q.RequestID != "" && !protocol.ValidRequestID(q.RequestID) {
		return createReq{}, badParams("params.requestId must be 1-%d of [A-Za-z0-9._-]", protocol.RequestIDMax)
	}
	if !filepath.IsAbs(q.Cwd) {
		return createReq{}, badParams("params.cwd %q is not an absolute path", q.Cwd)
	}
	q.Cwd = filepath.Clean(q.Cwd)
	if strings.TrimSpace(q.Prompt) == "" && q.Prompt != "" {
		return createReq{}, badParams("params.prompt is blank: send none instead")
	}
	for _, f := range []struct{ key, v string }{{"cwd", q.Cwd}, {"provider", q.Provider}, {"model", q.Model}, {"effort", q.Effort}} {
		if strings.ContainsFunc(f.v, unicode.IsControl) {
			return createReq{}, badParams("params.%s holds a control character", f.key)
		}
	}
	if st, err := os.Stat(q.Cwd); err != nil || !st.IsDir() {
		why := "is not a directory"
		if errors.Is(err, fs.ErrNotExist) {
			why = "does not exist"
		}
		return createReq{}, badParams("params.cwd %s %s", q.Cwd, why)
	}
	provider := q.Provider
	if provider == "" {
		provider = strings.TrimSpace(cr.o.DefaultProvider())
		if provider == "" {
			return createReq{}, badParams("params.provider is required: this hub has no default provider (config.toml's provider)")
		}
		if !cr.o.KnownProvider(provider) {
			return createReq{}, badParams("the default provider %q (config.toml's provider) is not one this craze can start", provider)
		}
	} else if !cr.o.KnownProvider(provider) {
		return createReq{}, badParams("params.provider %q is not a provider this craze can start", provider)
	}
	return createReq{p: q, provider: provider, hash: createHash(q)}, nil
}

// createHash is a create's identity for its requestId: a hash of its
// normalized params — what the client asked for, so the provider as it was
// sent ("" for the default, whatever the config says by the time it is
// retried) — as "sha256:<hex>".
func createHash(q protocol.CreateParams) string {
	key := struct {
		Cwd            string                  `json:"cwd"`
		Prompt         string                  `json:"prompt"`
		Provider       string                  `json:"provider"`
		Model          string                  `json:"model"`
		Effort         string                  `json:"effort"`
		Fast           *bool                   `json:"fast"`
		PermissionMode protocol.PermissionMode `json:"permissionMode"`
	}{q.Cwd, q.Prompt, q.Provider, q.Model, q.Effort, q.Fast, q.PermissionMode}
	b, _ := json.Marshal(key) // nothing in it fails to encode
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --------------------------------------------------------------- the table

// admit finds p's create or starts it (the file's comment, "Idempotency"):
// the call to wait on, or the refusal — request_conflict, busy, closing. A
// new create — or a join of a host a previous hub created — is counted in
// flight (lifecycle.beginCreate) and run on a goroutine of its own.
func (cr *creator) admit(p createReq) (*createCall, *protocol.Error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.evictLocked(time.Now())
	id := p.p.RequestID
	if id != "" {
		if c := cr.calls[id]; c != nil {
			if c.hash != p.hash {
				return nil, conflict(id)
			}
			cr.h.logf("session.create %s: joined the create in flight or done", id)
			return c, nil
		}
		if e, ok := cr.registered(id); ok {
			if e.RequestHash != p.hash {
				return nil, conflict(id)
			}
			if perr := cr.h.life.beginCreate(); perr != nil {
				return nil, perr
			}
			c := cr.newCallLocked(id, p.hash)
			cr.h.logf("session.create %s: joining host %s (pid %d), which a create of that id started", id, e.HostID, e.PID)
			go cr.run(c, func(ctx context.Context) createAnswer { return cr.h.join(ctx, id, e) })
			return c, nil
		}
	}
	if perr := cr.h.life.beginCreate(); perr != nil {
		return nil, perr
	}
	c := cr.newCallLocked(id, p.hash)
	go cr.run(c, func(ctx context.Context) createAnswer { return cr.h.create(ctx, p) })
	return c, nil
}

// conflict is a requestId reused with other params.
func conflict(id string) *protocol.Error {
	return refused(protocol.CodeBadRequest, protocol.ReasonRequestConflict,
		"requestId %s was used already for a create with other params", id)
}

// registered is the live host a create of requestId id started, from the
// registry (rundir.Hosts, which sweeps the dead): one whose entry carries the
// id and names its session — a host that names none yet is between its bind
// and its identity, and its spawner gone (this hub's memory has no create of
// that id), it stops itself at its ready line. The newest when, against
// every spawner's rule, there are several. A registry that cannot be read
// has none.
func (cr *creator) registered(id string) (rundir.Entry, bool) {
	entries, err := hostsRead(cr.h.o.Env)
	if err != nil {
		cr.h.logf("session.create %s: the registry cannot be read (%v); creating", id, err)
		return rundir.Entry{}, false
	}
	var found rundir.Entry
	ok := false
	for _, e := range entries {
		if e.RequestID != id || e.CrazeSessionID == "" {
			continue
		}
		if !ok || e.StartedAt.After(found.StartedAt) {
			found, ok = e, true
		}
	}
	return found, ok
}

// newCallLocked is a create in flight, kept under its requestId when it has
// one.
func (cr *creator) newCallLocked(id, hash string) *createCall {
	c := &createCall{id: id, hash: hash, done: make(chan struct{})}
	if id != "" {
		cr.calls[id] = c
	}
	return c
}

// evictLocked forgets the done creates kept past createKeep, then the oldest
// of those past createKeepMax. A create in flight is never forgotten.
func (cr *creator) evictLocked(now time.Time) {
	var done []*createCall
	for id, c := range cr.calls {
		switch {
		case !c.finished:
		case now.Sub(c.at) > createKeep:
			delete(cr.calls, id)
		default:
			done = append(done, c)
		}
	}
	if len(done) <= createKeepMax {
		return
	}
	slices.SortFunc(done, func(a, b *createCall) int { return a.at.Compare(b.at) })
	for _, c := range done[:len(done)-createKeepMax] {
		delete(cr.calls, c.id)
	}
}

// run runs one create (fn) as in flight, and answers its waiters — once it
// is no longer counted in flight, so a waiter that has its answer can be
// sure the create's slot is free again (busy).
func (cr *creator) run(c *createCall, fn func(context.Context) createAnswer) {
	ans := fn(cr.ctx)
	c.ans = ans
	cr.mu.Lock()
	c.at, c.finished = time.Now(), true
	cr.mu.Unlock()
	cr.h.life.endCreate()
	close(c.done)
}

// ------------------------------------------------------------- the create

// create is one create, from the spawn to the answer (the file's comment).
func (h *hub) create(ctx context.Context, p createReq) createAnswer {
	q := p.p
	tag := "session.create"
	if q.RequestID != "" {
		tag += " " + q.RequestID
	}
	dir, err := rundir.HostLogDir(h.o.Env)
	if err != nil {
		h.logf("%s: the host logs' directory: %v", tag, err)
		return createRefusal(spawnFailed("the session host cannot be started: the host logs' directory cannot be used"))
	}
	hostID := rundir.NewHostID()
	logPath := filepath.Join(dir, hostID+".log")
	spec := hostspawn.Spec{HostID: hostID, Log: logPath, Workspace: q.Cwd, Provider: p.provider, Model: q.Model,
		Effort: q.Effort, Fast: q.Fast, NoForce: q.PermissionMode == protocol.PermissionPrompt, NoHostStatus: true}
	if q.RequestID != "" {
		spec.RequestID, spec.RequestHash = q.RequestID, p.hash
	}
	cmd, err := hostCommand(hostspawn.Args(spec))
	if err != nil {
		h.logf("%s: %v", tag, err)
		return createRefusal(spawnFailed("the session host cannot be started: %v", err))
	}
	environ := cmd.Env
	if environ == nil {
		environ = os.Environ()
	}
	wd, _ := os.Getwd()
	cmd.Env = ChildEnv(environ, wd)
	cmd.Dir = q.Cwd
	child, r, err := hostspawn.StartCmd(cmd, hostspawn.HostChildEnv, filepath.Join(dir, hostspawn.AgentGroupsName(hostID)), logPath)
	if err != nil {
		// Its error names the executable's path: the hub's log's alone.
		h.logf("%s: start the session host: %v", tag, err)
		return createRefusal(spawnFailed("the session host cannot be started"))
	}
	h.logf("%s: host %s (pid %d) started for a %s session in %s; its log: %s", tag, hostID, child.PID(), p.provider, q.Cwd, logPath)
	line, failure, why := hostspawn.ReadReady(ctx, r)
	_ = r.Close()
	if failure != 0 {
		return h.notReady(tag, child, failure, why)
	}
	if !line.OK {
		// The host said why itself and is exiting: the grace to, and ended if
		// it takes longer. A new session is never another host's (held).
		child.Settle()
		why := strings.TrimPrefix(strings.TrimPrefix(line.Error, "craze serve: "), "craze: ")
		h.logf("%s: host %s could not start: %s", tag, hostID, line.Error)
		return createRefusal(spawnFailed("the session host could not start: %s", firstLine(why)))
	}
	if line.HostID != hostID {
		child.Terminate()
		h.logf("%s: host %s's ready line names host %q", tag, hostID, line.HostID)
		return createRefusal(spawnFailed("the session host answered for another host"))
	}
	sess, err := dialCreated(ctx, line.Socket, line.CrazeSessionID)
	if err != nil {
		h.logf("%s: host %s cannot be dialled: %v", tag, hostID, err)
		if ctx.Err() != nil {
			return createRefusal(closingErr())
		}
		h.abandonHost(child, line.Socket, line.CrazeSessionID)
		return createRefusal(spawnFailed("the session host cannot be reached"))
	}
	at := h.attend(ctx, sess, q.Prompt, time.Now().Add(createStartWait))
	switch at.kind {
	case attendCut:
		return createRefusal(closingErr())
	case attendStartFailed, attendStartTimeout:
		h.logf("%s: host %s: the session did not start: %s; stopping it", tag, hostID, at.cause)
		h.abandonHost(child, line.Socket, line.CrazeSessionID)
		return createRefusal(startFailed(at.cause))
	case attendLost:
		h.logf("%s: host %s went before its session started: %v; stopping it", tag, hostID, at.err)
		h.abandonHost(child, line.Socket, line.CrazeSessionID)
		return createRefusal(spawnFailed("the session host went before its session started"))
	}
	row := h.createdRow(hostID, line, child.PID(), at)
	h.logf("%s: host %s serving session %s, started; prompt %s", tag, hostID, line.CrazeSessionID, at.prompt)
	return createAnswer{res: &protocol.CreateResult{Session: row, Prompt: at.prompt, PromptError: at.promptErr}}
}

// join is a create of requestId id that a previous hub started (the file's
// comment, "Idempotency"), its host e, from the registry: its start waited
// for at most createStartWait from now — at once for a session that has
// started already — and answered as the create would have, its prompt
// unknown. A start that failed, or that does not end in time, stops the
// host: session.stop — it is not this hub's child, so nothing more.
func (h *hub) join(ctx context.Context, id string, e rundir.Entry) createAnswer {
	tag := "session.create " + id
	sess, err := dialCreated(ctx, e.Socket, e.CrazeSessionID)
	if err != nil {
		h.logf("%s: host %s cannot be dialled: %v", tag, e.HostID, err)
		if ctx.Err() != nil {
			return createRefusal(closingErr())
		}
		return createRefusal(refused(protocol.CodeUnavailable, protocol.ReasonHostUnreachable,
			"the session a create of requestId %s started cannot be reached", id))
	}
	at := h.attend(ctx, sess, "", time.Now().Add(createStartWait))
	switch at.kind {
	case attendCut:
		return createRefusal(closingErr())
	case attendStartFailed, attendStartTimeout:
		h.logf("%s: host %s: the session did not start: %s; stopping it", tag, e.HostID, at.cause)
		if err := stopCreated(e.Socket, e.CrazeSessionID); err != nil {
			h.logf("%s: host %s: session.stop: %v", tag, e.HostID, err)
		}
		return createRefusal(startFailed(at.cause))
	case attendLost:
		h.logf("%s: host %s went before its session started: %v", tag, e.HostID, at.err)
		return createRefusal(refused(protocol.CodeUnavailable, protocol.ReasonHostUnreachable,
			"the session a create of requestId %s started went before it started", id))
	}
	line := hostspawn.ReadyLine{OK: true, HostID: e.HostID, Socket: e.Socket, CrazeSessionID: e.CrazeSessionID}
	row := h.createdRow(e.HostID, line, e.PID, at)
	h.logf("%s: host %s serving session %s, started; joined", tag, e.HostID, e.CrazeSessionID)
	return createAnswer{res: &protocol.CreateResult{Session: row, Prompt: protocol.CreatePromptUnknown,
		PromptError: "the create was answered by another hub, which this one never heard from"}}
}

// notReady is a spawned host that gave no usable ready line (failure, why):
// ended, and a refusal — closing for a create the teardown cut, whose host is
// left to stop itself (its ready line has nobody to take it).
func (h *hub) notReady(tag string, child *hostspawn.Child, failure hostspawn.Failure, why string) createAnswer {
	var msg string
	switch failure {
	case hostspawn.Cancelled:
		h.logf("%s: host pid %d: cut by the teardown before it was ready", tag, child.PID())
		return createRefusal(closingErr())
	case hostspawn.Exited:
		child.Settle()
		msg = "the session host exited before it was ready"
		if err := child.Err(); err != nil {
			msg += " (" + err.Error() + ")"
		}
	case hostspawn.TimedOut:
		child.Terminate()
		msg = "the session host was not ready within " + hostspawn.ReadyWait.String()
	case hostspawn.Oversized:
		child.Terminate()
		msg = fmt.Sprintf("the session host's ready line is longer than %d bytes", hostspawn.ReadyLineMax)
	default:
		child.Terminate()
		msg = "the session host's ready line cannot be read: " + why
	}
	h.logf("%s: host pid %d: %s", tag, child.PID(), msg)
	return createRefusal(spawnFailed("%s", msg))
}

// hostCommand is a created host's command (HostCommand's rule).
func hostCommand(argv []string) (*exec.Cmd, error) {
	if c := HostCommand; c != nil {
		return c(argv)
	}
	if testing.Testing() {
		return nil, errNoHostCommand
	}
	return hostspawn.Command(argv)
}

// createdSession is what a create uses of its connection to a created host
// (attend): remote.Session's methods, and two reads of its own — behind an
// interface only so that attend's every outcome can be driven over a session
// a test holds.
type createdSession interface {
	Start(context.Context) error
	Read(context.Context) (backend.Item, error)
	Submit(context.Context, engine.Command, string, engine.SubmitMode, string) (engine.SubmitResult, error)
	ClientID() string
	CloseWithin(context.Context) error
	// hostVersion is the host's craze version, from its hello.
	hostVersion() string
	// sessionRow is the session's own sessions.list row, as its host wrote
	// it.
	sessionRow(context.Context) (json.RawMessage, error)
}

// remoteCreated is a created host's connection: a remote.Session.
type remoteCreated struct{ *remote.Session }

func (r remoteCreated) hostVersion() string { return r.Client().Hello().Endpoint.CrazeVersion }

// sessionRow is the host's sessions.list row naming the session — its one
// row, when none names it.
func (r remoteCreated) sessionRow(ctx context.Context) (json.RawMessage, error) {
	var list struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := r.Client().Call(ctx, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list); err != nil {
		return nil, err
	}
	sid := r.Info().CrazeSessionID
	for _, raw := range list.Sessions {
		var probe struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.SessionID == sid {
			return raw, nil
		}
	}
	if len(list.Sessions) == 1 {
		return list.Sessions[0], nil
	}
	return nil, fmt.Errorf("the host lists no row of session %s", sid)
}

// dialCreated dials the host at socket as the hub's own client of session
// sid, its first attach with when: ready, within createDialWait.
func dialCreated(ctx context.Context, socket, sid string) (createdSession, error) {
	dctx, cancel := context.WithTimeout(ctx, createDialWait)
	defer cancel()
	s, err := remote.DialSession(dctx, socket, remote.SessionOptions{
		Client:    remote.Options{Client: createClient, PeerCheck: rundir.DialCheck(os.Geteuid())},
		SessionID: sid,
		When:      protocol.WhenReady,
	})
	if err != nil {
		return nil, err
	}
	return remoteCreated{s}, nil
}

// abandonHost stops a host this hub spawned that it gives up on (plan 030
// §3.4's rule, internal/cli's hostRef.abandon): session.stop over a
// connection of its own, bounded by createStopWait, then
// hostspawn.TermGrace for the host's stop sequence; a stop that cannot be
// sent, or a host still there after the grace, is terminated. It returns once
// the host is gone and reaped.
func (h *hub) abandonHost(child *hostspawn.Child, socket, sid string) {
	if child.Exited() {
		child.KillAgents()
		return
	}
	if stopCreated(socket, sid) == nil && child.WaitExit(hostspawn.TermGrace) {
		child.KillAgents()
		return
	}
	child.Terminate()
}

// stopCreated sends session.stop for session sid to the host at socket, over
// a connection of its own, within createStopWait: nil once the host has
// taken it.
func stopCreated(socket, sid string) error {
	ctx, cancel := context.WithTimeout(context.Background(), createStopWait)
	defer cancel()
	c, err := remote.Dial(ctx, socket, remote.Options{Client: createClient, PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = c.Command(ctx, protocol.MethodSessionStop, protocol.StopParams{SessionID: sid}, nil, remote.CommandOptions{})
	return err
}

// ---------------------------------------------------------- the attending

// attendKind is what attending a created session came to.
type attendKind int

const (
	// attendStarted: the session started; the prompt's outcome and its row
	// are in the attendance.
	attendStarted attendKind = iota + 1
	// attendStartFailed: the session's start failed (cause).
	attendStartFailed
	// attendStartTimeout: the start did not end by its deadline (cause).
	attendStartTimeout
	// attendLost: the connection went, or could not attach, before the
	// session was up (err).
	attendLost
	// attendCut: the teardown cut the create.
	attendCut
)

// attendance is what attend came to.
type attendance struct {
	kind attendKind
	// cause is a start that failed's first error line.
	cause string
	err   error
	// prompt and promptErr are the first prompt's outcome.
	prompt    protocol.CreatePrompt
	promptErr string
	// row is the session's sessions.list row as its host wrote it
	// (compacted), nil when it could not be read; readAt when it was;
	// version the host's craze, from its hello.
	row     json.RawMessage
	readAt  time.Time
	version string
}

// attend is a create's life on its host, over sess — the hub's own
// connection, which it closes — once the host is up (the file's comment,
// steps 2–4): the start waited for until deadline; then the prompt, when
// there is one; then the session's row read; then the detach. The stream is
// drained throughout, as the list's dispatch drains it: a start that
// publishes more than the stream's queue holds before its ready would
// otherwise wait for a reader.
func (h *hub) attend(ctx context.Context, sess createdSession, prompt string, deadline time.Time) attendance {
	dctx, stopDrain := context.WithCancel(context.Background())
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := sess.Read(dctx); err != nil {
				return
			}
		}
	}()
	// closeNow closes the connection at once — no detach — and joins the
	// drain; closeView detaches first, bounded.
	closeWith := func(cctx context.Context) {
		_ = sess.CloseWithin(cctx)
		stopDrain()
		joinWithin(drained, createJoinWait)
	}
	closeNow := func() {
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		closeWith(cctx)
	}
	sctx, cancel := context.WithDeadline(ctx, deadline)
	err := sess.Start(sctx)
	timedOut := errors.Is(sctx.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil {
		closeNow()
		var se *remote.StartError
		switch {
		case errors.As(err, &se):
			return attendance{kind: attendStartFailed, cause: firstLine(se.Text)}
		case ctx.Err() != nil:
			return attendance{kind: attendCut}
		case timedOut:
			return attendance{kind: attendStartTimeout,
				cause: fmt.Sprintf("the session did not start within %s", createStartWait)}
		}
		return attendance{kind: attendLost, err: err}
	}
	at := attendance{kind: attendStarted, prompt: protocol.CreatePromptNone}
	if prompt != "" {
		var answered bool
		at.prompt, at.promptErr, answered = sendPrompt(ctx, sess, prompt)
		if !answered {
			// Its write may be blocked on a host that stopped reading, where
			// no context reaches it: the connection closed at once ends it.
			at.version = sess.hostVersion()
			closeNow()
			return at
		}
	}
	at.version = sess.hostVersion()
	at.row, at.readAt = readRow(ctx, sess)
	cctx, ccancel := context.WithTimeout(ctx, createDetachWait)
	closeWith(cctx)
	ccancel()
	return at
}

// sendPrompt sends prompt as the list's dispatch does (internal/tui's
// runDispatch): queued, with the connection's first command id, its answer
// waited for at most createPromptWait whether or not the call itself can see
// that deadline (X54: its write does not). answered is false when no answer
// came in time — the call is still out, and the caller closes the connection.
func sendPrompt(ctx context.Context, sess createdSession, prompt string) (protocol.CreatePrompt, string, bool) {
	pctx, cancel := context.WithTimeout(ctx, createPromptWait)
	defer cancel()
	submitted := make(chan error, 1)
	go func() {
		_, err := sess.Submit(pctx, engine.Command{Client: sess.ClientID(), ID: createCommandID}, prompt, engine.SubmitQueue, "")
		submitted <- err
	}()
	var err error
	select {
	case err = <-submitted:
	case <-pctx.Done():
		select {
		case err = <-submitted:
		default:
			return protocol.CreatePromptUnknown, fmt.Sprintf("no answer to the prompt within %s", createPromptWait), false
		}
	}
	switch {
	case err == nil:
		return protocol.CreatePromptAccepted, "", true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), errors.Is(err, backend.ErrOutcomeUnknown):
		return protocol.CreatePromptUnknown, firstLine(err.Error()), true
	}
	return protocol.CreatePromptRefused, firstLine(err.Error()), true
}

// readRow is the session's own sessions.list row, read over sess within
// createReadWait, compacted, and when it was read; nil when it could not be.
func readRow(ctx context.Context, sess createdSession) (json.RawMessage, time.Time) {
	rctx, cancel := context.WithTimeout(ctx, createReadWait)
	defer cancel()
	raw, err := sess.sessionRow(rctx)
	if err != nil {
		return nil, time.Time{}
	}
	at := time.Now()
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil {
		return nil, time.Time{}
	}
	return out.Bytes(), at
}

// createdRow is a created session's roster row, as the hub's roster would
// list it read now (RosterRow): its host as the registry names it — or, not
// listed (yet), as its ready line did — ready, since its session has
// started; reachable; its row the one just read. A row that could not be read
// leaves it approximate.
func (h *hub) createdRow(hostID string, line hostspawn.ReadyLine, pid int, at attendance) protocol.RosterRow {
	e := rundir.Entry{Protocol: protocol.ProtocolVersion, HostID: hostID, PID: pid, Socket: line.Socket,
		CrazeSessionID: line.CrazeSessionID}
	if entries, err := hostsRead(h.o.Env); err == nil {
		for _, x := range entries {
			if x.HostID == hostID {
				e = x
				break
			}
		}
	}
	host := roster.HostOf(e)
	host.Ready = true
	if host.CrazeSessionID == "" {
		host.CrazeSessionID = line.CrazeSessionID
	}
	now := time.Now()
	in := roster.Row{Host: host, Status: roster.Reachable, Version: at.version, Raw: at.row, ReadAt: at.readAt, Polled: true}
	row, ok := RosterRow(in, now)
	if !ok {
		// A host listed in a form the roster omits (its ids out of shape):
		// its ids as they stand, nothing more.
		row = protocol.RosterRow{HostID: hostID, SessionID: line.CrazeSessionID, Status: protocol.RosterReachable, Approximate: true,
			Host: protocol.RosterHost{PID: pid, Protocol: protocol.ProtocolVersion, Ready: true}}
	}
	return row
}

// ---------------------------------------------------------------- answers

func createRefusal(e *protocol.Error) createAnswer { return createAnswer{err: e} }

// spawnFailed is a host that failed to spawn or to become ready.
func spawnFailed(format string, args ...any) *protocol.Error {
	return refused(protocol.CodeUnavailable, protocol.ReasonSpawnFailed, format, args...)
}

// startFailed is a session whose start failed, or took too long: cause is
// the host's first error line, data.cause.
func startFailed(cause string) *protocol.Error {
	e := refused(protocol.CodeNotAccepting, protocol.ReasonStartFailed, "the session did not start: %s", cause)
	e.Data.Cause = cause
	return e
}

// closingErr is a create the teardown cut, or refused.
func closingErr() *protocol.Error {
	return refused(protocol.CodeUnavailable, protocol.ReasonClosing, "the hub is closing")
}

// firstLine is s's first non-blank line, trimmed.
func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return strings.TrimSpace(s)
}

// joinWithin waits for done to close, or d to pass.
func joinWithin(done <-chan struct{}, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// ------------------------------------------------------------ the client

// ErrUnanswered is a hub request whose connection ended before its answer
// (Create): the request may have been taken. A create retried under the same
// requestId answers the session it started, if it did.
var ErrUnanswered = errors.New("hub: the connection ended before the answer")

// Create asks the hub at socket for a new session — hello as who, then
// session.create with p — and answers the result as the hub wrote it, not
// re-encoded (a protocol.CreateResult), within ctx, as List does. A refusal
// is an error wrapping the hub's *protocol.Error; a connection that ends
// before the answer, one wrapping ErrUnanswered; a hub whose hello says it
// creates no session, a *LacksError.
func Create(ctx context.Context, socket string, who protocol.ClientInfo, p protocol.CreateParams) (json.RawMessage, error) {
	hc, err := dialHub(ctx, socket, who)
	if err != nil {
		return nil, err
	}
	defer hc.close()
	if !hc.hello.Capabilities.SessionCreate {
		return nil, &LacksError{Version: hc.hello.Endpoint.CrazeVersion, Missing: []string{"create sessions"}}
	}
	res, err := hc.call(protocol.MethodSessionCreate, p)
	if err != nil {
		return nil, err
	}
	var check protocol.CreateResult
	if err := json.Unmarshal(res, &check); err != nil {
		return nil, fmt.Errorf("hub: %s's result: %w", protocol.MethodSessionCreate, err)
	}
	return res, nil
}
