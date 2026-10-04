package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
)

// Session is the TUI's backend over the wire (plan 027 §3.14, PR 4): a
// backend.Backend whose every command and read is a round trip to the host
// that owns the engine, and whose stream is the host's attach stream, decoded.
// It is built on a Client and its Stream and changes neither's rules: the
// resend rule, the re-attach table and the byte-bounded queue are theirs.
//
//   - Info is the Session's own copy of the session info document (§3.13): a
//     fallback from the options before any attach reply (GLM 11), then the
//     attach reply's document, replaced by each Ready and Restore as the stream
//     receives them — ahead of Read, as the in-process Info is a live read
//     ahead of the stream. Capabilities are the host's as it sent them, never
//     rebuilt from this binary's provider table (GLM 7, astra 25). It is a
//     cached read: it waits on nothing and costs a copy.
//   - Read hands the stream up as backend items: the first attach's snapshot
//     as a Restore of generation 1, each event decoded with its seq and the
//     current generation, the Ready the host owes an attachment made before
//     readiness, each re-attach's snapshot as a Restore of the next
//     generation, and End.
//   - Start attaches (when the caller has not, Attach) and returns once the
//     session is ready — the attach reply's ready: true, or the ready
//     notification as the stream receives it, whether or not Read is being
//     called — or with the host's start failure, whose Error is the host's
//     start error text (StartError).
//   - The commands send the caller's own command id, bound to the client
//     identity taken at entry — its number (Client.Identity, the backend
//     epoch), which the ctx's epoch must name, and whose client id the
//     caller's engine.Command must name — refused before anything is sent
//     otherwise, never written under another identity after, and never
//     retried by code: the in-process calls never were. The reads are bound
//     the same way. A refusal is an *Error, which reconstructs the engine's
//     sentinel (sentinels.go).
//   - Close is a view close (§3.9): the stream detached, the client closed.
//     The session goes on on its host.
//
// Locks: Session.mu is a leaf. It is taken by the stream's observer under the
// stream's queue lock, so no Session method holds it across a call into the
// client or the stream.
type Session struct {
	c    *Client
	opts SessionOptions

	// closed is closed by Close; wg counts the Session's own goroutines (a
	// broken stream's detach), which Close joins.
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup

	mu sync.Mutex
	// closing says Close has begun: nothing more is attached, and no
	// goroutine more is started.
	closing bool
	// stream is the attach stream once Attach has succeeded; attaching is
	// closed once the attach in flight returns; attached once stream is set.
	stream    *Stream
	attaching chan struct{}
	attached  chan struct{}
	// info is Info's: the fallback, then the host's documents as the stream
	// receives them (observe).
	info backend.SessionInfo
	// ready is closed once the start's outcome is known, startErr its
	// failure; ended once the stream's last item is queued, endErr why.
	ready    chan struct{}
	isReady  bool
	startErr error
	ended    chan struct{}
	isEnded  bool
	endErr   error
	// gen is Read's stream generation: 1 from the first attach's snapshot,
	// one more at each Restore. over says Read has handed up the stream's
	// end: every Read after it is backend.ErrClosed.
	gen  uint64
	over bool
	// stopped says a Stop of this Session's was answered — its receipt, or
	// the session's end while it was outstanding: the session is ending, so
	// Close has no stream to detach.
	stopped bool
}

var _ backend.Backend = (*Session)(nil)

// SessionOptions configure a Session (DialSession).
type SessionOptions struct {
	// Client is the client's own options (Dial).
	Client Options
	// SessionID is the craze session to attach (the registry entry's
	// crazeSessionId); "" is the host's one session, as sessions.list names
	// it. Before any attach reply it is Info's CrazeSessionID.
	SessionID string
	// When is the first attach's (§3.4): "" or ready waits for the host's
	// start; now attaches at once, the start's events arrive live, and a Ready
	// item follows — what the golden matrix attaches with before the host
	// starts its engine (§3.16).
	When protocol.When
	// Budget is the subscription's budget (nil: the host's defaults).
	Budget *protocol.AttachBudget

	// The fallback Info answers before any attach reply (GLM 11: as the
	// in-process path answers from the configured provider before Start):
	// Provider, the registry entry's provider name, and Workspace. Label and
	// Capabilities, when set, are the fallback's own; when not, they are this
	// binary's provider table's for Provider — the in-process path's own read
	// before Start (engineBackend.Info through agent.ProviderInfo), and for
	// the fallback ONLY: once the host has answered, Info is the host's as
	// sent.
	Provider     string
	Workspace    string
	Label        string
	Capabilities *agent.Capabilities

	// ReadsAttachments is the dialler's word that the host reads the
	// attachments directory this client's TUI stores pasted images in (plan
	// 033 P27): a host of the client's own CRAZE_HOME namespace, which the
	// dialler decides from the socket's path (rundir.SocketInNamespace) — the
	// session itself cannot tell. Session.ReadsAttachments answers it to the
	// TUI, which makes image chips only for a session whose host says yes.
	ReadsAttachments bool
}

// ReadsAttachments is the TUI's P27 question (its attachmentsReader, plan 033
// §3.3): SessionOptions.ReadsAttachments, as the session was dialled.
func (s *Session) ReadsAttachments() bool { return s.opts.ReadsAttachments }

// closeBound bounds a view close's detach (§3.9): past it the client's close
// ends the transport, which ends the subscription on the host anyway.
const closeBound = 3 * time.Second

// StartError is the host's start having failed (§3.4: ready{startFailed}, or
// an attach refused not_accepting, reason start_failed): Error is the host's
// start error text, verbatim — what the in-process Start returned.
type StartError struct {
	Text string
}

func (e *StartError) Error() string { return e.Text }

// CodecError is a host whose event or snapshot codec is not this build's: a
// client that does not know a codec version must not fold that stream (§3.3).
type CodecError struct {
	Host protocol.Codecs
}

func (e *CodecError) Error() string {
	return fmt.Sprintf("remote: the host's codecs (event %d, snapshot %d) are not this build's (event %d, snapshot %d): its stream cannot be folded",
		e.Host.Event, e.Host.Snapshot, agent.EventCodecVersion, transcript.SnapshotVersion)
}

// knownCodecs is nil when this build reads the codecs c names.
func knownCodecs(c protocol.Codecs) error {
	if c.Event != agent.EventCodecVersion || c.Snapshot != transcript.SnapshotVersion {
		return &CodecError{Host: c}
	}
	return nil
}

// DialSession dials the host at path (Dial) and checks its codecs: a version
// this build does not know is a dial error (a *CodecError), and the client is
// closed. Nothing is attached yet: Attach, or Start, attaches.
func DialSession(ctx context.Context, path string, o SessionOptions) (*Session, error) {
	return dialSession(ctx, path, o, hooks{})
}

// dialSession is DialSession with the tests' hooks in place.
func dialSession(ctx context.Context, path string, o SessionOptions, h hooks) (*Session, error) {
	c, err := dial(ctx, path, o.Client, h)
	if err != nil {
		return nil, err
	}
	if err := knownCodecs(c.Hello().Codecs); err != nil {
		_ = c.Close()
		return nil, err
	}
	s := &Session{c: c, opts: o, closed: make(chan struct{}), attached: make(chan struct{}),
		ready: make(chan struct{}), ended: make(chan struct{})}
	s.info = s.fallback(c.Hello().RetryHorizon)
	return s, nil
}

// fallback is Info before any attach reply (GLM 11): the options' provider,
// label and capabilities — the provider table's for the name where the
// options carry none — the options' session id and workspace, and the
// hello's retry horizon.
func (s *Session) fallback(h protocol.RetryHorizon) backend.SessionInfo {
	p := agent.ProviderInfo{Name: s.opts.Provider}
	info := backend.SessionInfo{
		CrazeSessionID: s.opts.SessionID,
		Workspace:      s.opts.Workspace,
		Provider:       s.opts.Provider,
		Label:          s.opts.Label,
		RetryHorizon:   retryHorizon(h),
	}
	if info.Label == "" {
		info.Label = p.Label()
	}
	if c := s.opts.Capabilities; c != nil {
		info.Capabilities = *c
	} else {
		info.Capabilities = p.Capabilities()
	}
	return info
}

// Client is the protocol client the Session is built on.
func (s *Session) Client() *Client { return s.c }

// ------------------------------------------------------------- lifecycle

// Attach attaches the Session to its session (§3.4), with the options' when
// and budget and no cursor — so the first reply carries a snapshot (X21), the
// first Restore Read hands up. It is idempotent: once attached it returns
// nil, and a call while another's attach is in flight waits for it. A refused
// attach is its *Error (a failed start: not_accepting, reason start_failed,
// its cause the host's start error); nothing is attached, and a later call
// tries again. The golden matrix calls it with when: "now" before the host
// starts its engine (§3.16), so the start's events reach Read live.
func (s *Session) Attach(ctx context.Context) error {
	for {
		s.mu.Lock()
		switch {
		case s.stream != nil:
			s.mu.Unlock()
			return nil
		case s.closing:
			s.mu.Unlock()
			return backend.ErrClosed
		case s.attaching != nil:
			ch := s.attaching
			s.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		ch := make(chan struct{})
		s.attaching = ch
		s.mu.Unlock()
		st, err := s.c.Attach(ctx, AttachOptions{SessionID: s.opts.SessionID, When: s.opts.When, Budget: s.opts.Budget, observe: s.observe})
		if h := s.c.hooks.attached; h != nil {
			h()
		}
		s.mu.Lock()
		s.attaching = nil
		close(ch)
		closing := s.closing
		if err == nil {
			// Held even if Close began meanwhile, so the Session's stream is
			// the one thing either of them closes; Close read it under this
			// lock, so it either closes it itself or left it to this.
			s.stream = st
			close(s.attached)
		}
		s.mu.Unlock()
		if err == nil && closing {
			// Close ran while the attach was out, and saw no stream: this
			// detaches the one it made, bounded as Close's own is, and Read
			// hands up nothing of it (backend.ErrClosed).
			cctx, cancel := context.WithTimeout(context.Background(), closeBound)
			_ = st.Close(cctx)
			cancel()
			return backend.ErrClosed
		}
		return err
	}
}

// Start attaches if the Session has not (Attach), and returns once the session
// is ready: at once for an attach reply that said ready: true, or once the
// stream has received the ready notification — whether or not Read is being
// called (the TUI reads from Init while Start runs). A start that failed is a
// *StartError whose Error is the host's start error text, whether the ready
// notification said so or the attach was refused start_failed. A stream that
// ends before readiness ends Start with why; Close, with backend.ErrClosed.
//
// Without a reader, the stream's queue (Options.StreamBytes) holds what comes
// before the ready: a start that publishes more than that before its ready
// waits for a reader, as the stream does (X18 8).
func (s *Session) Start(ctx context.Context) error {
	if err := s.Attach(ctx); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == protocol.CodeNotAccepting && e.Reason == protocol.ReasonStartFailed {
			return &StartError{Text: e.Cause}
		}
		return err
	}
	select {
	case <-s.ready:
		return s.started()
	case <-s.ended:
		return s.started()
	case <-s.closed:
		return backend.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// started is the start's outcome once ready or ended is closed, decided by
// the Session's state, whichever channel woke the caller: the start's
// outcome when the session was ready first — readyLocked never settles one
// after the end — and otherwise the end.
func (s *Session) started() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isReady {
		return s.startErr
	}
	err := s.endErr
	if err == nil {
		err = errors.New("the session ended")
	}
	return fmt.Errorf("remote: the stream ended before the session was ready: %w", err)
}

// Started is a no-op: the host's engine is started by its host, and the host
// told this Session how its start went (Start, the Ready item).
func (s *Session) Started(error) {}

// Close is a view close (§3.9): the stream is detached — bounded by
// closeBound, past which the client's close ends the transport, and the
// subscription with it — and the client closed. It never stops the session,
// which goes on on its host. It is idempotent; it answers nil for an orderly
// close, and a host's refusal of the detach otherwise. Read answers
// backend.ErrClosed afterwards, and every command ErrClosed (one still
// waiting resolves as Client.Close says).
func (s *Session) Close() error { return s.CloseWithin(context.Background()) }

// CloseWithin is Close bounded by ctx as well as by closeBound (plan 030
// C5r): the explicit quit's one deadline covers its stop, the wait for the
// session's end and this close after them (tui's stopQuit), so the detach
// waits only for what is left of it, and a ctx already done — the deadline
// passed, or a second quit — detaches nothing: the transport is closed at
// once, which ends the subscription on the host anyway, and every write
// blocked on it — a stop's behind a host that has stopped reading among them
// (C5r2), which is why the quit closes through here at its deadline. It is
// Close in every other way, and shares its once: whichever is called first
// decides.
func (s *Session) CloseWithin(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		st := s.stream
		if s.stopped {
			// The session is ending on its host (Stop): a detach would only
			// wait on a connection that admits nothing more.
			st = nil
		}
		s.mu.Unlock()
		close(s.closed)
		if st != nil && ctx.Err() == nil {
			cctx, cancel := context.WithTimeout(ctx, closeBound)
			err := st.Close(cctx)
			cancel()
			var e *Error
			if errors.As(err, &e) {
				s.closeErr = e
			}
		}
		_ = s.c.Close()
		s.wg.Wait()
	})
	return s.closeErr
}

// Ended is closed once the stream's last item is queued — the session's end
// on its host, or the transport given up, or an item that could not be read —
// whether or not Read has handed it up yet. It is what the TUI's explicit quit
// waits on after its stop (plan 030 §3.6).
func (s *Session) Ended() <-chan struct{} { return s.ended }

// ClientID is the client id the host bound this client to last, read per call:
// it changes when a reconnect's hello did not resume.
func (s *Session) ClientID() string { return s.c.ClientID() }

// Epoch is the client's identity (Client.Identity): it moves on every hello
// that did not resume — a retired client, a replaced engine, a restarted host
// — before the re-attach's Restore is queued, and never on a same-identity
// Restore (a cursor refused after a slow consumer, an omitted reset). A command
// or read whose ctx carries another epoch is refused before anything is sent.
func (s *Session) Epoch() uint64 { return s.c.Identity() }

// Info is the session's facts as this Session holds them now (see Session):
// the fallback until the first attach reply, then the host's documents as the
// stream receives them. It waits on nothing; the catalogs are the caller's
// own copies.
func (s *Session) Info() backend.SessionInfo {
	s.mu.Lock()
	info := s.info
	s.mu.Unlock()
	info.Models = slices.Clone(info.Models)
	info.Modes = slices.Clone(info.Modes)
	return info
}

// observe is the stream's observer (AttachOptions.observe): each item as the
// stream queues it, in stream order, under the queue's lock. The documents
// replace Info; an attach reply that says ready, or a Ready, settles the
// start; the stream's last item settles its end. Once the Session has ended —
// the stream's End, or an item Read could not decode (broken) — nothing more
// is observed: Read hands up nothing after that end, so no later document
// moves Info and no later Ready settles the start.
func (s *Session) observe(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isEnded {
		return
	}
	switch it.Kind {
	case KindAttached, KindRestore:
		s.info = sessionInfo(&it.Reply.Session)
		if it.Reply.Ready {
			s.readyLocked(nil)
		}
	case KindReady:
		r := it.Ready
		if r.Session.SessionID != "" {
			// A Ready made from a start_failed refusal carries no document.
			s.info = sessionInfo(&r.Session)
		}
		var err error
		if r.StartFailed {
			err = &StartError{Text: r.Err}
		}
		s.readyLocked(err)
	case KindEnd, KindError:
		if !s.isEnded {
			s.isEnded, s.endErr = true, it.Err
			close(s.ended)
		}
	}
}

// readyLocked settles the start, once, and never once the Session has ended:
// a stream that ended before readiness ends Start with its end, whatever the
// host says afterwards. err is the start's failure; s.mu is held.
func (s *Session) readyLocked(err error) {
	if s.isReady || s.isEnded {
		return
	}
	s.isReady, s.startErr = true, err
	close(s.ready)
}

// ----------------------------------------------------------------- stream

// Read is the stream (backend.Backend.Read): one item at a time, in order,
// from one reader. It waits for the Session to be attached. A ctx done before
// an item is taken takes nothing (Stream.Next's own contract: its queue hands
// out an item or waits, and a wait that ctx ends takes none); an item taken is
// always returned — the stream items that map to no backend item
// (synchronized, an attach whose cursor was honoured) are taken and passed
// over, and only then is ctx looked at again. After End, Read is
// backend.ErrClosed, and so it is once Close has begun, whatever the stream
// still holds.
//
// An event or a snapshot that does not decode — or a Restore from a host
// whose codecs this build does not read (a reconnect reached another host) —
// ends the stream: an End item carrying why, never an item skipped. The
// stream is detached then.
func (s *Session) Read(ctx context.Context) (backend.Item, error) {
	if err := ctx.Err(); err != nil {
		return backend.Item{}, err
	}
	st, err := s.attachedStream(ctx)
	if err != nil {
		return backend.Item{}, err
	}
	for {
		s.mu.Lock()
		over := s.over || s.closing
		s.mu.Unlock()
		if over {
			return backend.Item{}, backend.ErrClosed
		}
		it, err := st.Next(ctx)
		switch {
		case errors.Is(err, ErrStreamClosed):
			s.mu.Lock()
			s.over = true
			s.mu.Unlock()
			return backend.Item{}, backend.ErrClosed
		case err != nil:
			return backend.Item{}, err
		}
		if bi, ok := s.item(st, it); ok {
			return bi, nil
		}
		if err := ctx.Err(); err != nil {
			return backend.Item{}, err
		}
	}
}

// attachedStream is the stream, waiting for the Session to be attached.
func (s *Session) attachedStream(ctx context.Context) (*Stream, error) {
	for {
		s.mu.Lock()
		st := s.stream
		s.mu.Unlock()
		if st != nil {
			return st, nil
		}
		select {
		case <-s.attached:
			// Read's loop answers backend.ErrClosed for a stream Close beat.
		case <-s.closed:
			return nil, backend.ErrClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// item is it as a backend item, and false for one that maps to none.
func (s *Session) item(st *Stream, it Item) (backend.Item, bool) {
	switch it.Kind {
	case KindAttached, KindRestore:
		r := it.Reply
		if r.Snapshot == nil {
			// A cursor honoured: the stream goes on from it, nothing to
			// restore (a first attach with no cursor always carries one, X21).
			return backend.Item{}, false
		}
		if err := knownCodecs(s.c.Hello().Codecs); err != nil {
			return s.broken(st, err), true
		}
		snap, err := transcript.DecodeSnapshot(r.Snapshot)
		if err != nil {
			return s.broken(st, fmt.Errorf("remote: the snapshot at %s/%d: %w", r.After.Incarnation, r.After.Seq, err)), true
		}
		s.mu.Lock()
		s.gen++
		gen := s.gen
		s.mu.Unlock()
		return backend.Item{Kind: backend.ItemRestore, Info: sessionInfo(&r.Session), Snapshot: snap, Gen: gen}, true
	case KindEvent:
		ev, err := agent.DecodeEvent(string(it.Body))
		if err != nil {
			return s.broken(st, fmt.Errorf("remote: event %d: %w", it.Seq, err)), true
		}
		ev.Seq = it.Seq
		s.mu.Lock()
		gen := s.gen
		s.mu.Unlock()
		return backend.Item{Kind: backend.ItemEvent, Event: ev, Gen: gen}, true
	case KindReady:
		r := it.Ready
		var info backend.SessionInfo
		if r.Session.SessionID != "" {
			info = sessionInfo(&r.Session)
		} else {
			info = s.Info()
		}
		var err error
		if r.StartFailed {
			err = &StartError{Text: r.Err}
		}
		return backend.Item{Kind: backend.ItemReady, Info: info, Err: err}, true
	case KindEnd, KindError:
		s.mu.Lock()
		s.over = true
		s.mu.Unlock()
		return backend.Item{Kind: backend.ItemEnd, Err: it.Err}, true
	case KindPresence:
		// How many clients are attached (plan 032 §3.14), in the stream
		// generation it was read in: a Restore after it is a later
		// attachment's, whose own count follows it.
		s.mu.Lock()
		gen := s.gen
		s.mu.Unlock()
		return backend.Item{Kind: backend.ItemPresence, Attached: it.Attached, Gen: gen}, true
	}
	// synchronized, and a kind this build does not hand up.
	return backend.Item{}, false
}

// broken ends the stream for err (an End item carrying it) and detaches it,
// on a goroutine of the Session's (Close joins it), bounded by closeBound. It
// ends the Session as the stream's own End does: a Start still waiting for the
// session's readiness — which no ready can bring now: the detach drops it —
// returns with err.
func (s *Session) broken(st *Stream, err error) backend.Item {
	s.mu.Lock()
	s.over = true
	if !s.isEnded {
		s.isEnded, s.endErr = true, err
		close(s.ended)
	}
	detach := !s.closing
	if detach {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if detach {
		go func() {
			defer s.wg.Done()
			if h := s.c.hooks.broken; h != nil {
				h()
			}
			ctx, cancel := context.WithTimeout(context.Background(), closeBound)
			defer cancel()
			_ = st.Close(ctx)
		}()
	}
	return backend.Item{Kind: backend.ItemEnd, Err: err}
}

// --------------------------------------------------------------- commands

// command sends one command (§3.6) as the caller's engine.Command c, with its
// own command id, and never retried by code. It takes its binding at entry —
// the identity the client holds and that identity's client id, read together
// — and is refused before anything is sent, as ErrOutcomeUnknown (resume_lost)
// matching backend.ErrStaleEpoch, when the ctx's epoch is not that identity
// or c names another client id; it then goes under that identity's NUMBER
// (CommandOptions.Identity), which registration, the adoption that moves the
// identity and every attempt compare — never the client id's spelling, which
// a replaced engine or a restarted host mints again. A ctx with no epoch
// binds the command to the identity current at entry. params builds the
// method's params for the session id the Session is attached to.
func (s *Session) command(ctx context.Context, c engine.Command, method string, params func(sid string) any, result any) error {
	ident, client := s.c.binding()
	if err := backend.CheckEpoch(ctx, ident); err != nil {
		return staleIdentity(method, c.ID)
	}
	if c.Client == "" || c.ID == "" {
		return fmt.Errorf("remote: %s: %w: a command over the socket names its client and its id (got %q/%q)", method, engine.ErrBadRequest, c.Client, c.ID)
	}
	if c.Client != client {
		return staleIdentity(method, c.ID)
	}
	if h := s.c.hooks.entered; h != nil {
		h(c.ID)
	}
	sid, err := s.sessionID(ctx, ident)
	switch {
	case errors.Is(err, backend.ErrStaleEpoch):
		return staleIdentity(method, c.ID)
	case err != nil:
		return err
	}
	_, err = s.c.Command(ctx, method, params(sid), result, CommandOptions{ID: c.ID, Identity: ident})
	return err
}

// read makes one read (§3.3), bound as a command is: to the identity the
// client holds at entry, which the ctx's epoch must name. It is written only
// on a connection of that identity (Client.callAs) — so a read made for one
// session never goes out to another, not even one that waited for a
// connection across a resume loss — and once the client has left that
// identity it is backend.ErrStaleEpoch, nothing sent, as in process. Its
// answer is judged against the binding once it has come back too: an answer
// read on the old identity's connection that reaches its caller after the
// client has left that identity is backend.ErrStaleEpoch, never the old
// session's state handed to a caller now bound elsewhere. A read whose
// connection went before its reply is made again (reads are safe to repeat),
// under the same binding, while ctx allows.
func (s *Session) read(ctx context.Context, method string, params func(sid string) any, result any) error {
	ident := s.c.Identity()
	if err := backend.CheckEpoch(ctx, ident); err != nil {
		return err
	}
	for {
		sid, err := s.sessionID(ctx, ident)
		if err != nil {
			return err
		}
		err = s.c.callAs(ctx, ident, method, params(sid), result)
		if errors.Is(err, ErrConnectionLost) && ctx.Err() == nil {
			continue
		}
		if s.c.Identity() != ident {
			// Whatever came back came back from a session this read's caller
			// is no longer bound to.
			return backend.ErrStaleEpoch
		}
		return err
	}
}

// sessionID is the craze session the Session's calls name: the stream's (it
// changes only when a reconnect finds the host serving another), before any
// attach the options', and else the host's one session (sessions.list, bound
// to identity ident as the call it is for).
func (s *Session) sessionID(ctx context.Context, ident uint64) (string, error) {
	s.mu.Lock()
	st := s.stream
	s.mu.Unlock()
	if st != nil {
		return st.SessionID(), nil
	}
	if s.opts.SessionID != "" {
		return s.opts.SessionID, nil
	}
	var list protocol.SessionsListResult
	if err := s.c.callAs(ctx, ident, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list); err != nil {
		return "", err
	}
	if len(list.Sessions) != 1 {
		return "", fmt.Errorf("remote: the host serves %d sessions: name one in SessionOptions.SessionID", len(list.Sessions))
	}
	return list.Sessions[0].SessionID, nil
}

// Submit is session.prompt, mode queue or send_now (Control.Submit).
func (s *Session) Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	var r protocol.PromptResult
	err := s.command(ctx, c, protocol.MethodSessionPrompt, func(sid string) any {
		return protocol.PromptParams{SessionID: sid, Text: text, FromRow: fromRow, Mode: protocol.PromptMode(mode)}
	}, &r)
	if err != nil {
		return engine.SubmitResult{}, err
	}
	res := engine.SubmitResult{Turn: r.Turn, Armed: r.Armed}
	if r.Text != nil {
		res.Text = *r.Text
	}
	if len(r.Queued) > 0 {
		q, err := agent.DecodeQueuedPrompt(r.Queued)
		if err != nil {
			return engine.SubmitResult{}, replyError(protocol.MethodSessionPrompt, err)
		}
		res.Queued = &q
	}
	return res, nil
}

// Interject is session.prompt, mode interject (Control.Interject).
func (s *Session) Interject(ctx context.Context, c engine.Command, text string) error {
	return s.command(ctx, c, protocol.MethodSessionPrompt, func(sid string) any {
		return protocol.PromptParams{SessionID: sid, Text: text, Mode: protocol.PromptInterject}
	}, nil)
}

// Answer is asks.answer (Control.Answer).
func (s *Session) Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	return s.command(ctx, c, protocol.MethodAsksAnswer, func(sid string) any {
		return protocol.AsksAnswerParams{SessionID: sid, AskID: id, Answer: protocol.Answer{OptionID: a.OptionID,
			Cancel: a.Cancel, Answers: a.Answers, Skip: a.Skip, Accept: a.Accept, Reject: a.Reject}}
	}, nil)
}

// Unqueue is session.queue.remove (Control.Unqueue): the row removed.
func (s *Session) Unqueue(ctx context.Context, c engine.Command, id string) (agent.QueuedPrompt, error) {
	var r protocol.QueueRemoveResult
	err := s.command(ctx, c, protocol.MethodQueueRemove, func(sid string) any {
		return protocol.QueueRemoveParams{SessionID: sid, RowID: id}
	}, &r)
	if err != nil {
		return agent.QueuedPrompt{}, err
	}
	q, err := agent.DecodeQueuedPrompt(r.Row)
	if err != nil {
		return agent.QueuedPrompt{}, replyError(protocol.MethodQueueRemove, err)
	}
	return q, nil
}

// EditQueued is session.queue.edit (Control.EditQueued), with the version the
// edit was made against when there is one.
func (s *Session) EditQueued(ctx context.Context, c engine.Command, id, text string, expectedVersion *int) error {
	return s.command(ctx, c, protocol.MethodQueueEdit, func(sid string) any {
		return protocol.QueueEditParams{SessionID: sid, RowID: id, Text: text, ExpectedVersion: expectedVersion}
	}, nil)
}

// ClearQueue is session.queue.clear (Control.ClearQueue): every row it
// removed, in queue order — rows another client queued included (C2).
func (s *Session) ClearQueue(ctx context.Context, c engine.Command) ([]agent.QueuedPrompt, error) {
	var r protocol.QueueClearResult
	err := s.command(ctx, c, protocol.MethodQueueClear, func(sid string) any {
		return protocol.QueueClearParams{SessionID: sid}
	}, &r)
	if err != nil {
		return nil, err
	}
	rows := make([]agent.QueuedPrompt, 0, len(r.Removed))
	for _, raw := range r.Removed {
		q, err := agent.DecodeQueuedPrompt(raw)
		if err != nil {
			return nil, replyError(protocol.MethodQueueClear, err)
		}
		rows = append(rows, q)
	}
	return rows, nil
}

// Disarm is session.disarm (Control.Disarm).
func (s *Session) Disarm(ctx context.Context, c engine.Command) error {
	return s.command(ctx, c, protocol.MethodSessionDisarm, func(sid string) any {
		return protocol.DisarmParams{SessionID: sid}
	}, nil)
}

// SetTitle is session.setTitle (Control.SetTitle).
func (s *Session) SetTitle(ctx context.Context, c engine.Command, title string) error {
	return s.command(ctx, c, protocol.MethodSessionSetTitle, func(sid string) any {
		return protocol.SetTitleParams{SessionID: sid, Title: title}
	}, nil)
}

// Set is session.set (Control.Set): the confirmed value and its revision.
func (s *Session) Set(ctx context.Context, c engine.Command, st engine.Setting) (engine.SetResult, error) {
	var r protocol.SetResult
	err := s.command(ctx, c, protocol.MethodSessionSet, func(sid string) any {
		return protocol.SetParams{SessionID: sid, Setting: protocol.Setting{Kind: protocol.SettingKind(st.Kind),
			ID: st.ID, Value: st.Value, ForModel: st.ForModel}}
	}, &r)
	if err != nil {
		return engine.SetResult{}, err
	}
	return engine.SetResult{Value: r.Value, Rev: r.Rev}, nil
}

// Cancel is session.cancel (Control.Cancel). A cancel that ran and failed
// carries its result beside its error (data.result, §3.2) — Reported above
// all — which comes back beside the error, as in process.
func (s *Session) Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error) {
	var r protocol.CancelResult
	err := s.command(ctx, c, protocol.MethodSessionCancel, func(sid string) any {
		return protocol.CancelParams{SessionID: sid, TurnID: turn}
	}, &r)
	var e *Error
	switch {
	case err == nil:
	case errors.As(err, &e) && len(e.Result) > 0:
		r = protocol.CancelResult{}
		if derr := json.Unmarshal(e.Result, &r); derr != nil {
			return engine.CancelResult{}, err
		}
	default:
		return engine.CancelResult{}, err
	}
	return engine.CancelResult{Outcome: engine.CancelOutcome(r.Outcome), Turn: r.Turn, Reported: r.Reported}, err
}

// Stop is session.stop (plan 030 §3.6a; backend.Backend.Stop): the
// caller's own command id, bound as every command is. It returns once the
// host has answered its receipt — the stop taken, not yet done: the
// session's end follows on the stream, its closing records and then the
// stream's End. A host whose capability stop is false answers unsupported,
// reason stop_unsupported, which is an *Error matching
// backend.ErrStopUnsupported (sentinels.go): nothing was stopped, and the
// caller detaches instead.
//
// The session's end while the stop is outstanding is its answer too (plan 030
// C5, X3): a stop that joins one already running, or that arrives once a
// signal or the idle exit has set the end going, may meet the session's end
// before its receipt — the host's connection ends as the session does, and
// admits nothing more — and that end is what the stop asked for, never an
// outcome unknown. The session's own end (reset session_closed, an End with
// no error) answers it; a transport given up does not, and the command's own
// answer stands. A session that had ended before the call is answered at
// once. The command's call is abandoned when the end answers: its context is
// the call's own, cancelled then, so nothing is left waiting on a host that
// has gone.
func (s *Session) Stop(ctx context.Context, c engine.Command) error {
	if s.endedClean() {
		s.markStopped()
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.command(cctx, c, protocol.MethodSessionStop, func(sid string) any {
			return protocol.StopParams{SessionID: sid}
		}, nil)
	}()
	var err error
	select {
	case err = <-done:
	case <-s.ended:
		if !s.endedClean() {
			err = <-done
			break
		}
		cancel()
		<-done
		err = nil
	}
	if err != nil && s.endedClean() && !errors.Is(err, backend.ErrStopUnsupported) {
		// The end came as the call failed for it: the end is the answer.
		err = nil
	}
	if err == nil {
		s.markStopped()
	}
	return err
}

// endedClean reports that the stream has ended with the session's own end:
// no error beside it.
func (s *Session) endedClean() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isEnded && s.endErr == nil
}

// markStopped records a stop answered (Session.stopped).
func (s *Session) markStopped() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

// CancelSubagent is session.subagent.cancel (Control.CancelSubagent).
func (s *Session) CancelSubagent(ctx context.Context, c engine.Command, id string) error {
	return s.command(ctx, c, protocol.MethodSubagentCancel, func(sid string) any {
		return protocol.SubagentCancelParams{SessionID: sid, AgentID: id}
	}, nil)
}

// RefreshModels is session.models.refresh (plan 034 §3.4, Q17;
// backend.Backend.RefreshModels): a read, bound as every read is to the
// identity the client holds at entry, and resent across a lost connection —
// the method is idempotent, so asking again is all a resend does. Its reply
// follows the catalog delta an applied refresh published, so the stream has
// already queued the list the answer names.
//
// A session whose info document does not say modelsRefresh — an ACP session,
// a host from before the method, or no attach reply yet — is answered
// agent.ModelsUnsupported here and nothing is sent (A25: no call). A host
// that refuses the method anyway, code unsupported (models_refresh_unsupported,
// or unknown_method from an older host the document came from before a
// reconnect reached it), is answered the same: what a client does with
// either is the in-process answer's. A status this build does not know is
// handed up as it came: the caller acts on the ones it knows.
func (s *Session) RefreshModels(ctx context.Context, nativeDir string) (agent.ModelsRefresh, error) {
	if err := backend.CheckEpoch(ctx, s.c.Identity()); err != nil {
		return agent.ModelsRefresh{}, err
	}
	if !s.Info().ModelsRefresh {
		return agent.ModelsRefresh{Status: agent.ModelsUnsupported}, nil
	}
	var r protocol.ModelsRefreshResult
	err := s.read(ctx, protocol.MethodModelsRefresh, func(sid string) any {
		return protocol.ModelsRefreshParams{SessionID: sid, NativeDir: nativeDir}
	}, &r)
	var e *Error
	switch {
	case errors.As(err, &e) && e.Code == protocol.CodeUnsupported:
		return agent.ModelsRefresh{Status: agent.ModelsUnsupported}, nil
	case err != nil:
		return agent.ModelsRefresh{}, err
	}
	return agent.ModelsRefresh{Status: agent.ModelsStatus(r.Status), Revision: r.Revision, SameDir: r.SameDir}, nil
}

// Ask is asks.get: the ask id names, and whether the session knows it —
// unknown_ask is (zero, false, nil), as the in-process registry's miss. The
// record carries what the wire does (X6): the status, the outcome, who
// resolved it and how, and the opening, its strings capped at the snapshot's
// ItemCap; the turn token, the provider-delivery fields and the incarnation
// stay on the host.
func (s *Session) Ask(ctx context.Context, id string) (agent.AskRecord, bool, error) {
	var r protocol.AsksGetResult
	err := s.read(ctx, protocol.MethodAsksGet, func(sid string) any {
		return protocol.AsksGetParams{SessionID: sid, AskID: id}
	}, &r)
	var e *Error
	switch {
	case errors.As(err, &e) && e.Code == protocol.CodeUnknownAsk:
		return agent.AskRecord{}, false, nil
	case err != nil:
		return agent.AskRecord{}, false, err
	}
	rec, err := askRecord(&r.Ask)
	if err != nil {
		return agent.AskRecord{}, false, replyError(protocol.MethodAsksGet, err)
	}
	return rec, true, nil
}

// Settings is session.state's settings (§3.12): the host's live model, mode
// and config, what a multi-Set chain judges its next step on.
func (s *Session) Settings(ctx context.Context) (backend.Settings, error) {
	var r protocol.StateResult
	err := s.read(ctx, protocol.MethodSessionState, func(sid string) any {
		return protocol.StateParams{SessionID: sid}
	}, &r)
	if err != nil {
		return backend.Settings{}, err
	}
	cfg, err := agent.DecodeConfigState(r.Settings.Config)
	if err != nil {
		return backend.Settings{}, replyError(protocol.MethodSessionState, err)
	}
	out := backend.Settings{Model: r.Settings.Model, Mode: r.Settings.Mode}
	if cfg != nil {
		out.Config = cfg.Options
	}
	return out, nil
}

// LastTurn is session.state's lastTurn (plan 030 §3.7, SF-57): how the
// session's last turn ended, nil while a turn runs, before any has ended, and
// from a host from before plan 030, which sends none. It is bound as every
// read is (read): to the identity the client holds at entry, so an answer
// from a session this client has since left is backend.ErrStaleEpoch, never
// that session's ending. An outcome this build does not know is handed up as
// it came: the caller acts on the three it knows and on nothing else.
func (s *Session) LastTurn(ctx context.Context) (*engine.LastTurn, error) {
	var r protocol.StateResult
	err := s.read(ctx, protocol.MethodSessionState, func(sid string) any {
		return protocol.StateParams{SessionID: sid}
	}, &r)
	if err != nil || r.LastTurn == nil {
		return nil, err
	}
	lt := r.LastTurn
	return &engine.LastTurn{Outcome: engine.TurnOutcome(lt.Outcome), Err: lt.Err, EndedAt: lt.EndedAt, TurnID: lt.TurnID}, nil
}

// replyError is a reply whose result this build could not read.
func replyError(method string, err error) error {
	return fmt.Errorf("remote: %s's reply: %w", method, err)
}

// ------------------------------------------------------------ conversions

// sessionInfo is the wire's info document in the Backend's types: the host's
// facts as it sent them — its capabilities above all, never this binary's
// provider table's (astra 25).
func sessionInfo(p *protocol.SessionInfo) backend.SessionInfo {
	info := backend.SessionInfo{
		CrazeSessionID:    p.SessionID,
		ProviderSessionID: p.ProviderSessionID,
		Incarnation:       p.Incarnation,
		Workspace:         p.Workspace,
		Provider:          p.Provider.Name,
		Label:             p.Provider.Label,
		Capabilities:      capabilities(p.Capabilities),
		CatalogRevision:   p.Catalogs.Revision,
		ModelsRefresh:     p.Capabilities.ModelsRefresh,
		RetryHorizon:      retryHorizon(p.RetryHorizon),
		PermissionMode:    permissionMode(p.PermissionMode),
		StartedAt:         p.StartedAt,
	}
	for _, m := range p.Catalogs.Models {
		info.Models = append(info.Models, agent.ModelInfo{ID: m.ID, Name: m.Name, Recent: m.Recent})
	}
	for _, m := range p.Catalogs.Modes {
		info.Modes = append(info.Modes, agent.ModeInfo{ID: m.ID, Name: m.Name, Description: m.Description})
	}
	return info
}

// permissionMode is the info document's permissionMode in the Backend's
// words (plan 030 §3.7, SF-60): bypass or prompt, and anything else — absent,
// from a host from before plan 030, or a mode this build does not know — the
// host not saying, so the client shows its own config's.
func permissionMode(m protocol.PermissionMode) backend.PermissionMode {
	switch m {
	case protocol.PermissionBypass:
		return backend.PermissionBypass
	case protocol.PermissionPrompt:
		return backend.PermissionPrompt
	}
	return backend.PermissionUnsaid
}

// capabilities is the session capability set on the wire as agent's: every
// agent.Capabilities field from its wire name (the server's sessionCapabilities
// inverted; TestEveryWireCapabilityComesBack holds the two to a bijection).
// cancel, approvals, historyCursor and stop are the protocol's own, stated for
// every host, and rowFacts the host's (what its sessions.list row carries,
// plan 030 §3.8), each with no agent field; modelsRefresh is the session's,
// and goes to backend.SessionInfo.ModelsRefresh instead (sessionInfo, plan
// 034 §3.4).
func capabilities(c protocol.SessionCapabilities) agent.Capabilities {
	return agent.Capabilities{
		Interject:           c.Interject,
		SubagentCancel:      c.SubagentCancel,
		SubagentBackground:  c.SubagentBackground,
		Modes:               c.Modes,
		Effort:              c.Effort,
		FastToggle:          c.FastToggle,
		SubagentRows:        c.SubagentRows,
		SubagentTranscript:  c.SubagentTranscript,
		Todos:               c.Todos,
		AskCards:            c.AskCards,
		PlanCards:           c.PlanCards,
		ParameterizedPicker: c.ParameterizedPicker,
	}
}

// retryHorizon is the wire's retry horizon as the engine's.
func retryHorizon(h protocol.RetryHorizon) engine.RetryHorizon {
	return engine.RetryHorizon{Commands: h.Commands, Age: time.Duration(h.AgeMs) * time.Millisecond}
}

// wireAskBody is an ask's opening on the wire (the server's encodeAskBody):
// each payload as the event codec's leaf wrapper writes it.
type wireAskBody struct {
	Permission json.RawMessage `json:"permission,omitempty"`
	Question   json.RawMessage `json:"question,omitempty"`
	Plan       json.RawMessage `json:"plan,omitempty"`
}

// askRecord is asks.get's record as agent's.
func askRecord(r *protocol.AskRecord) (agent.AskRecord, error) {
	rec := agent.AskRecord{
		ID: r.ID, Kind: agent.AskKind(r.Kind), Status: agent.AskStatus(r.Status),
		Outcome: agent.AskOutcome(r.Outcome), By: r.By, OpenedAt: r.OpenedAt, ResolvedAt: r.ResolvedAt,
	}
	if a := r.Answer; a != nil {
		rec.Answer = agent.AskAnswer{OptionID: a.OptionID, Cancel: a.Cancel, Answers: a.Answers,
			Skip: a.Skip, Accept: a.Accept, Reject: a.Reject}
	}
	if len(r.Body) == 0 {
		return rec, nil
	}
	var w wireAskBody
	if err := json.Unmarshal(r.Body, &w); err != nil {
		return agent.AskRecord{}, err
	}
	var err error
	if rec.Body.Permission, err = agent.DecodePermissionEvent(w.Permission); err != nil {
		return agent.AskRecord{}, err
	}
	if rec.Body.Question, err = agent.DecodeQuestionEvent(w.Question); err != nil {
		return agent.AskRecord{}, err
	}
	if rec.Body.Plan, err = agent.DecodePlanEvent(w.Plan); err != nil {
		return agent.AskRecord{}, err
	}
	return rec, nil
}
