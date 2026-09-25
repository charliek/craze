package control

import (
	"errors"
	"time"

	"github.com/charliek/craze/internal/engine"
)

// The binding table (plan 027 §3.6, astra 12–14, GLM 2).
//
// hello mints a client id per connection (Control.NewClientID) and binds it:
// the table holds, per client id, which connection it is bound to now, a
// generation that every transfer bumps, and the resume token. The table, the
// token index and every conn's bound state are guarded by Server.bindMu, a
// leaf above the receipts table's own: under it the server calls only the
// receipts table's leaf methods — NewClientID, ClaimClient, ReleaseClient —
// so the one lock edge is bindMu → receipts.mu, and bindMu is never held
// across I/O, a handler or any engine call that can block.
//
//   - TRANSFER: hello{resume} whose token this server issued for that client
//     id in the current engine's incarnation runs in ONE bindMu section —
//     ClaimClient(id), bump gen, point the binding at the new connection, mark
//     the old connection superseded — and the old connection is closed outside
//     the lock. The answer is resumed: true and the same client id.
//   - RELEASE: a connection's close releases its client only if the binding
//     still names that connection AND its generation — the compare and the
//     ReleaseClient both under bindMu, so no transfer can slip between them.
//     The engine's ReleaseClient releases whatever id it is given, even one a
//     transfer has just claimed (C2's review), so this compare is the only
//     guard against a stale connection's late cleanup releasing a live,
//     resumed client (TestAStaleConnectionsCleanupReleasesNothing).
//   - Two simultaneous resumes serialise on bindMu: the second transfers the
//     binding from the first and closes it.
//
// # The token index, and what a resume is answered
//
// Client ids restart at c-1 in every engine (receipts.go), and in every host
// process, so a client id alone never says which incarnation a resume is for:
// a stranger's {c-1, token} can name a c-1 that is bound here now. The TOKEN
// is what says it. Server.tokens indexes every token this server has issued
// and still remembers, to the client id it names and the incarnation it was
// issued in, and hello{resume: {clientId, token}} is decided from it alone:
//
//   - the token names this clientId in the current engine's incarnation:
//     TRANSFER (above) — or, if the engine has retired the client
//     (ClaimClient's ErrUnknownClient), its binding and token are dropped and
//     the answer is resumed: false with a fresh id;
//   - the token is known and names a DIFFERENT clientId: bad_request, reason
//     bad_token — a confused or hostile client — and nothing changes. A wrong
//     token never binds;
//   - the token is known from the previous engine: another incarnation's,
//     resumed: false with a fresh id;
//   - the token is unknown to this server — another host process's, an engine
//     older than the one it remembers, or never issued at all: as far as this
//     host can know, another incarnation's (§3.6), so resumed: false with a
//     fresh id, and any binding of that client id here is untouched.
//
// Nothing but a token this server issued, for exactly that id, in this
// incarnation, binds an id; every other answer is a fresh id or a refusal.
//
// The index is bounded as the table is. A current-incarnation token lives
// exactly as long as its binding: added with it at a fresh hello, kept across
// resumes, dropped with it when its client is found retired or its binding has
// gone without a connection for the idle bound ("Lifetime"). When the engine
// is replaced, the table is cleared and the replaced engine's tokens stay as
// the PREVIOUS generation — so its clients meeting a collision with the new
// engine's ids are told resumed: false, and a previous token presented under
// the wrong id is still bad_token — and every older generation is dropped. So
// the index holds the current engine's bindings' tokens plus one replaced
// engine's; a token two replacements old is simply unknown, which is answered
// resumed: false like any stranger's, and never binds either way. (A
// replacement happens when the host swaps its session: the picker, in tests.)
//
// The token is the binding's for its whole life: a resume answers with the
// same one (execution amendment, C6). A token rotated on every resume would
// make the pinned schedule above — the second of two simultaneous resumes
// wins — impossible, since the second would present a token the first had
// just voided; and a client that persists its token (A4's process-restart
// leg) and crashed between a resume's reply and writing the new one would lose
// the id it still held. The trust model is local and same-uid, so a token's
// secrecy is the client's own state's file permissions either way, and the
// index is an ordinary map keyed by it.
//
// # Lifetime (plan 027 X13, astra r5)
//
// A binding lives while a connection holds it, and for a bounded time after:
//
//   - a binding with NO CONNECTION for Server.idle — twice the engine's
//     receipts age bound (RetryHorizon.Age, 10 min), so 20 min — is dropped,
//     with its token. The release time is recorded at the compare-and-release
//     (unbind), on the server's clock (Options.Clock), and every release is
//     appended to Server.released, oldest first. The drop is lazy: each hello
//     (bind) and each unbind pops the releases that have aged out off the
//     front, and drops a binding only if that release is still its latest —
//     no connection since, the same generation, the same binding (a resume in
//     between re-binds it, and a later release appends a fresh entry). So the
//     work is bounded by the releases popped, and the list holds only the
//     releases of the last 20 min;
//   - a binding is also dropped when its client is found retired (a resume
//     whose ClaimClient fails), and every binding when the engine is replaced
//     (SetEngine), the released list with them;
//   - a command admitted under a binding that has since been dropped does not
//     run (movedOn, X15): the drop erased the generation that would say
//     whether a resume had taken the client over, so a missing binding counts
//     as moved on. The command did not run, and the client's later resume is
//     answered resumed: false, so it resolves the command outcome-unknown.
//
// Why 20 min, and why without an engine call: the engine retires a released
// client once it has waited out receiptAge holding no entry, so by twice that
// the client is normally retired and its binding could only answer resumed:
// false anyway. A client with a command that ran longer than that is not yet
// retired, and loses its binding all the same: its later resume is answered
// resumed: false with a fresh id, and the client resolves its outstanding
// commands as outcome-unknown — honest, and never a second execution, since
// the fresh id's commands are new ones. So neither the table nor the index
// grows with the history of clients that never come back: both hold the live
// bindings plus the releases of the last 20 min (and one replaced engine's
// tokens, above).

// binding is one client id's place in the table.
type binding struct {
	// conn is the connection the id is bound to, nil once released.
	conn  *conn
	gen   uint64
	token string
}

// releaseRecord is one compare-and-release, as Server.released keeps it: the
// binding released, at which generation, when (the server's clock), and the
// id it is filed under. A binding's generation is released at most once, so
// {b, gen} with no connection since says the release is still its latest.
type releaseRecord struct {
	client string
	b      *binding
	gen    uint64
	at     time.Time
}

// tokenOwner is what an issued token names: its client id, and the
// incarnation of the engine that minted the id.
type tokenOwner struct {
	client      string
	incarnation string
}

// bound is a connection's hello: the client id it holds, the generation it
// was bound at, and the engine it serves — captured once, so a handler keeps
// its engine even across a replacement.
type bound struct {
	client  string
	gen     uint64
	token   string
	eng     *engine.Engine
	crazeID string
}

// The binding section's refusals.
var (
	errNotReady    = errors.New("control: no session is being served yet")
	errBadToken    = errors.New("control: that resume token does not match")
	errConnClosing = errors.New("control: the connection is closing")
	errTokenReused = errors.New("control: the token source repeated a token")
)

// bind is hello's binding section: a transfer of resume's client to c when its
// token says so, and otherwise a fresh client (the rules above). It reports the
// binding, whether it was a resume, and the connection the transfer
// superseded — which the caller closes, outside every lock.
func (s *Server) bind(c *conn, resume *resumeReq) (b *bound, resumed bool, old *conn, err error) {
	// The token a fresh binding gets is read before the section: the source
	// is I/O, and bindMu is never held across I/O.
	token, err := s.newToken()
	if err != nil {
		return nil, false, nil, err
	}
	if h := s.hooks.beforeBind; h != nil {
		h()
	}
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	eng := s.eng
	if eng == nil {
		return nil, false, nil, errNotReady
	}
	// First, so a resume presenting a binding that has idled out finds it
	// gone, as it would had anything else run the drop first.
	s.dropIdleLocked()
	// A connection already closing gets nothing: its cleanup may have run,
	// and a binding made now would name a connection nobody will release.
	// close sets closing before its cleanup takes bindMu, and this is read
	// under bindMu, so the two are ordered either way.
	if c.closing.Load() {
		return nil, false, nil, errConnClosing
	}
	if resume != nil {
		owner, known := s.tokens[resume.token]
		switch {
		case !known:
			// Another host's, an older engine's, or never issued: another
			// incarnation's as far as this host can know. A fresh id below.
		case owner.client != resume.clientID:
			return nil, false, nil, errBadToken
		case owner.incarnation != s.incarnation:
			// The previous engine's client. A fresh id below.
		default:
			if cur := s.binds[resume.clientID]; cur != nil && eng.ClaimClient(resume.clientID) == nil {
				old = cur.conn
				cur.gen++
				cur.conn = c
				b = &bound{client: resume.clientID, gen: cur.gen, token: cur.token, eng: eng, crazeID: s.crazeID}
				c.bound = b
				if old != nil {
					old.superseded.Store(true)
				}
				return b, true, old, nil
			}
			// Retired (ErrUnknownClient): nothing is left to take back, and
			// its binding and token go with it.
			delete(s.binds, resume.clientID)
			delete(s.tokens, resume.token)
		}
	}
	if _, reused := s.tokens[token]; reused {
		return nil, false, nil, errTokenReused
	}
	id := eng.NewClientID()
	s.binds[id] = &binding{conn: c, gen: 1, token: token}
	s.tokens[token] = tokenOwner{client: id, incarnation: s.incarnation}
	b = &bound{client: id, gen: 1, token: token, eng: eng, crazeID: s.crazeID}
	c.bound = b
	return b, false, nil, nil
}

// unbind is a closing connection's compare-and-release: the client is
// released only if its binding still names this connection at this
// generation. A superseded connection, one whose engine was replaced (the
// table was cleared), and one that never said hello release nothing.
func (s *Server) unbind(c *conn) {
	if h := s.hooks.beforeUnbind; h != nil {
		s.bindMu.Lock()
		client := ""
		if c.bound != nil {
			client = c.bound.client
		}
		s.bindMu.Unlock()
		h(client)
	}
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	defer s.dropIdleLocked()
	b := c.bound
	if b == nil {
		return
	}
	cur := s.binds[b.client]
	if cur == nil || cur.conn != c || cur.gen != b.gen {
		return
	}
	cur.conn = nil
	s.released = append(s.released, releaseRecord{client: b.client, b: cur, gen: cur.gen, at: s.opts.Clock()})
	b.eng.ReleaseClient(b.client)
}

// dropIdleLocked drops every binding, and its token, that has had no
// connection for the idle bound ("Lifetime"): it pops the aged releases off
// the front of the released list and drops each binding whose latest release
// that still is. Its work is the releases it pops. bindMu is held.
func (s *Server) dropIdleLocked() {
	if len(s.released) == 0 {
		return
	}
	now := s.opts.Clock()
	n := 0
	for _, r := range s.released {
		if now.Sub(r.at) < s.idle {
			break
		}
		n++
		if cur := s.binds[r.client]; cur == r.b && cur.conn == nil && cur.gen == r.gen {
			delete(s.binds, r.client)
			delete(s.tokens, cur.token)
		}
	}
	if n > 0 {
		clear(s.released[:n])
		s.released = s.released[n:]
	}
}

// movedOn reports whether b's binding has moved on since a handler was
// admitted under it: a resume transferred the client (the binding's
// generation is newer than b's), the binding is gone from the table on the
// same engine (dropped: idle for the bound, "Lifetime", or its client found
// retired), or the engine b was bound to is no longer the one served (a
// replacement cleared the table). A mutating command checks it just before its
// engine call (conn.command) and does not run if so (astra r5 3).
//
// A missing binding counts as moved on (plan 027 X15, astra r6 1): the drop
// erases the generation that would say whether a resume had taken the client
// over, and no handler pauses for the idle bound between its admission and its
// engine call, so refusing is the conservative, honest answer — the command
// did not run, and a later resume is answered resumed: false, so the client
// resolves it as outcome-unknown. A plain release — the connection closed,
// the binding still in the table, its connection cleared, its generation
// unchanged — is NOT moving on: losing the connection does not cancel an
// admitted command (§3.6, 03 §7), so the command runs and its receipt answers
// the client's resend after a resume.
func (s *Server) movedOn(b *bound) bool {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	if s.eng != b.eng {
		return true
	}
	cur := s.binds[b.client]
	return cur == nil || cur.gen != b.gen
}
