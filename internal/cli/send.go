package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze send (plan 041 §3.2, §3.4, W2) sends a prompt to a session — queued
// behind any turn it is running — by any id craze ps prints. With --wait it
// waits for that prompt's own turn and prints its result as craze wait does
// (the owned form: a signal removes the prompt's queued row, or cancels its
// turn by id). Without it, it prints where the prompt went — `started
// <turn>`, or `queued <row> (position <n>)` — and the command that follows it
// with its handle, and exits 0.
//
// The session is a live host's when one serves it (matchSession's tiers). A
// session no live host serves — and one whose host refuses the attach because
// it is closing the session, or idling out — is reloaded (§3.4): its id is
// resolved over the session index (the newest row of each craze id, by the
// same tiers over the craze ids; a row with no craze id is not addressable,
// and a provider craze cannot load again has no row), and a host is spawned
// for it as craze new --wait spawns one — directly, with the hub's
// environment, out of the command's process tree — loading it (`craze serve
// --load <crazeId>`) with the row's
// recorded start settings and --unattended (reloadFlags), or, for a row with
// no record — a person's session — as the ordinary interactive host the
// session list would spawn. A host that took the session meanwhile (another
// send's reload) is attached to (spawnHost's rendezvous); one still closing
// it is waited out, within reloadWait. A workspace that is gone, or a load the
// agent refuses, is exit 1 with the host's words.
//
// The prompt (prompt_input.go) is read before anything is resolved, spawned or
// dialled, and is never sent twice: an answer lost with its connection is exit
// 1, `the prompt may not have reached it`, naming craze wait.

// sendWhat names craze send in its lines.
const sendWhat = "craze send"

// reloadWait bounds how long craze send waits for a host closing the session
// to be gone — or to keep it, when its idle exit decides to stay — before it
// gives up; reloadPoll is how often it looks again. Variables only so a test
// can shorten them.
var (
	reloadWait = 10 * time.Second
	reloadPoll = 50 * time.Millisecond
)

// reloadClosing sees each time craze send's reload finds the session's host
// still closing it, before it looks again: a no-op, and the barrier a test
// ends that host at.
var reloadClosing = func() {}

type sendFlags struct {
	wait       bool
	timeout    time.Duration
	maxMessage int
	json       bool
	prompt     promptInput
}

func newSendCmd() *cobra.Command {
	f := &sendFlags{}
	cmd := &cobra.Command{
		Use:   "send <id> [PROMPT...]",
		Short: "Send a prompt to a session, reloading it if it has ended",
		Long: "craze send sends a prompt — PROMPT, --prompt-file, or stdin — to the session <id> names (as craze " +
			"ps shows it), queued behind any turn it is running, and prints where it went: started, or queued " +
			"and its place. A session that has ended is loaded again first, from the session index. With " +
			"--wait it waits for the prompt's own turn and prints its result, as craze wait does.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sigs, stop := waitSignals()
			defer stop()
			return runSend(cmd, f, args, sigs)
		},
	}
	cmd.Flags().BoolVar(&f.wait, "wait", false, "wait for the prompt's turn to end and print its result; a signal then removes or cancels it")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "give up after this long (from the command's start); with --wait the result is running, exit 5, with a handle to follow it (default: no limit)")
	cmd.Flags().IntVar(&f.maxMessage, "max-message", 0, "with --wait: print at most this many characters of the message and the plan together, and write the whole result to a file named in the output (default: no limit)")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the outcome as one JSON object")
	registerPromptInputFlags(cmd, &f.prompt)
	return cmd
}

// runSend is craze send (the file's comment).
func runSend(cmd *cobra.Command, f *sendFlags, args []string, sigs <-chan os.Signal) error {
	started := time.Now()
	id, err := sessionID(cmd, args)
	if err != nil {
		return err
	}
	words := args[1:]
	switch {
	case f.timeout < 0:
		return usagef("craze send: --timeout must not be negative")
	case f.maxMessage < 0:
		return usagef("craze send: --max-message must not be negative")
	case cmd.Flags().Changed("max-message") && !f.wait:
		return usagef("craze send: --max-message needs --wait")
	case !rundir.ValidToken(id):
		// Refused before a registry read builds a path (the bridge's rule).
		return usagef("craze send: that is not a session id (craze ps lists them)")
	}
	if err := f.prompt.check(cmd, sendWhat, words); err != nil {
		return err
	}
	// A stdout whose reader has gone fails its write rather than kill the
	// command before its stderr line (craze new's rule).
	ignoreSIGPIPE()
	r := newPromptJob(cmd, sendWhat, "", f.wait, started, f.timeout, f.json, f.maxMessage, sigs)
	defer r.close()
	// Under --timeout and the signals, as the rest of the command (C3 review
	// r1, 6), and before anything is resolved or spawned.
	text, err := f.prompt.read(r.ctx, cmd, sendWhat, words, r.signalled)
	if err != nil {
		return failJSON(cmd.OutOrStdout(), r.out, err)
	}
	r.text = text
	conn, err := r.connect(id)
	if err != nil {
		return r.failBefore(err)
	}
	defer conn.close(r.relayed)
	r.session = resultSession{id: conn.sessionID, provider: conn.entry.Provider}
	if f.wait {
		return r.waitFor(conn)
	}
	return r.sendOnly(conn)
}

// connect is the session id names, attached (the file's comment): a live
// host's, or — none serving it, or its host closing it — the index's,
// reloaded.
func (r *promptJob) connect(id string) (*waitConn, error) {
	entries, err := rundir.Hosts(r.env)
	if err != nil {
		return nil, exitf(1, "%s: %s", sendWhat, sanitizeLine(err.Error()))
	}
	var row sessions.Row
	switch matches := matchSession(entries, id); len(matches) {
	case 1:
		sid, err := entrySession(sendWhat, matches[0])
		if err != nil {
			return nil, err
		}
		r.session = resultSession{id: sid, provider: matches[0].Provider}
		conn, err := r.dialFound(matches[0], sid)
		if err == nil || !retryable(err) || r.ctx.Err() != nil {
			return conn, err
		}
		// The host is closing the session, or it has ended there, or the
		// host went between the registry's answer and the dial: the index's
		// to reload, by the session's own craze id. A session the index has
		// no row for (never prompted, or a provider craze cannot load)
		// cannot be: the attach's refusal says why.
		var ierr error
		if row, ierr = sendIndexRow(sid, sid); ierr != nil {
			return nil, err
		}
	case 0:
		if row, err = sendIndexRow(id, ""); err != nil {
			return nil, err
		}
	default:
		return nil, exitf(1, "%s: %d sessions match %s: %s", sendWhat, len(matches), id, sanitizeLine(formatEntries(matches)))
	}
	return r.reload(row)
}

// reloadable says an attach's refusal is a session to load again (§3.4): its
// host closing it, or idling out (closing), or the session ended there — and
// not one whose start failed, which a reload would only fail again.
func reloadable(err error) bool {
	var re *remote.Error
	if errors.As(err, &re) && re.Reason == protocol.ReasonStartFailed {
		return false
	}
	return errors.Is(err, engine.ErrClosing) || errors.Is(err, engine.ErrNotAccepting) || errors.Is(err, remote.ErrSessionEnded)
}

// hostGone says a dial to a host the registry listed failed with no answer
// from it at all — its socket gone (ENOENT), refused, the handshake cut off:
// the host is going, or has gone, between the registry's answer and the dial
// (C3 review r1, 2). A refusal the host answered says nothing of the kind,
// and nor does the command's own deadline or signal (dialWait's exits that
// carry no cause).
func hostGone(err error) bool {
	var ee *exitError
	if !errors.As(err, &ee) || ee.cause == nil {
		return false
	}
	var re *remote.Error
	return !errors.As(err, &re)
}

// retryable says a dial to a listed host is worth making again, or the
// session worth loading in a host of its own: the host closing it, ending
// it, or gone (reloadable, hostGone).
func retryable(err error) bool { return reloadable(err) || hostGone(err) }

// dialFound is dialWait for a host the registry listed — found serving the
// session, or a spawn's held answer — which may be going by now.
func (r *promptJob) dialFound(e rundir.Entry, sessionID string) (*waitConn, error) {
	sendDialling(e)
	return dialWait(r.ctx, e, sessionID, sendWhat, nil)
}

// sendDialling sees each listed host craze send is about to dial: a no-op, and
// the barrier a test removes that host at, between the lookup and the dial.
var sendDialling = func(rundir.Entry) {}

// sendIndexRow is the session index's row id names (§3.4): the newest row of
// each craze id (sessions.Store.ByCrazeID) of a provider craze can load
// again, by matchSession's tiers over the craze ids — exact, a suffix, a
// prefix, each past the first needing four characters. exact, when set, is
// the one craze id to take (a live host's session). None is `no session
// <id>`; several are exit 1, naming them.
func sendIndexRow(id, exact string) (sessions.Row, error) {
	rows, err := (&sessions.Store{KnownProvider: knownProvider}).ByCrazeID()
	if err != nil {
		return sessions.Row{}, exitf(1, "%s: the session index: %s", sendWhat, sanitizeLine(err.Error()))
	}
	var ids []string
	for crazeID, row := range rows {
		if knownProvider(row.Provider) && (exact == "" || crazeID == exact) {
			ids = append(ids, crazeID)
		}
	}
	slices.Sort(ids)
	matches := matchCrazeID(ids, id)
	switch len(matches) {
	case 0:
		return sessions.Row{}, exitf(1, "%s: no session %s", sendWhat, id)
	case 1:
		return rows[matches[0]], nil
	}
	named := make([]string, len(matches))
	for i, m := range matches {
		named[i] = fmt.Sprintf("%s (%s, %s)", psShort(m), rows[m].Provider, rows[m].CWD)
	}
	return sessions.Row{}, exitf(1, "%s: %d ended sessions match %s: %s", sendWhat, len(matches), id, sanitizeLine(strings.Join(named, ", ")))
}

// reload is row's session loaded again and attached (§3.4): a host that
// serves it already — another send's reload, or one still closing it — is
// attached to; with none, a host is spawned to load it (reloadFlags), which
// may find the session held after all, and attaches to its holder. A holder
// closing it, or one gone between the lookup and the dial (hostGone), is
// looked for again every reloadPoll, within reloadWait; a host this run
// spawned that cannot be attached to is the failure. The host a reload
// spawned is never stopped by this run, whatever becomes of its prompt (C3
// review r1, 1): another send may have found it and prompted it already.
func (r *promptJob) reload(row sessions.Row) (*waitConn, error) {
	opts := spawnOptions{env: r.env, flags: reloadFlags(row), load: loadArg(row), unattended: row.Unattended != nil, shared: true, hubEnv: true, reparent: true}
	r.session = resultSession{id: row.CrazeID, provider: row.Provider}
	until := time.Now().Add(reloadWait)
	for {
		var (
			conn *waitConn
			err  error
		)
		if e, ok := hostServing(r.env, row.CrazeID); ok {
			conn, err = r.dialFound(e, row.CrazeID)
		} else {
			ref, serr := r.spawn(opts, false)
			if serr != nil {
				return nil, spawnExit(sendWhat, serr)
			}
			if ref.held {
				conn, err = r.dialFound(ref.entry, ref.entry.CrazeSessionID)
			} else if conn, err = dialWait(r.ctx, ref.entry, ref.entry.CrazeSessionID, sendWhat, nil); err != nil {
				return nil, err
			}
		}
		switch {
		case err == nil:
			return conn, nil
		case !retryable(err) || r.ctx.Err() != nil:
			return nil, err
		case !time.Now().Before(until) && reloadable(err):
			return nil, &exitError{code: 1, msg: sendWhat + ": session " + psShort(row.CrazeID) + " is still closing; try again once it has ended", cause: err}
		case !time.Now().Before(until):
			return nil, err
		}
		reloadClosing()
		select {
		case <-r.ctx.Done():
			return nil, err
		case <-time.After(reloadPoll):
		}
	}
}

// --------------------------------------------------------- without --wait

// errNoAnswer is a prompt whose answer did not come within cleanupWait of a
// signal or --timeout's end.
var errNoAnswer = errors.New("no answer")

// sendReceipt is craze send's outcome without --wait, as --json prints it:
// where the prompt went — `started` and its turn, or `queued`, its row and
// its place in the queue (1 is next) — and the handle that follows it.
type sendReceipt struct {
	Status   string `json:"status"`
	Session  string `json:"session"`
	Short    string `json:"short"`
	Provider string `json:"provider"`
	Turn     string `json:"turn,omitempty"`
	Row      string `json:"row,omitempty"`
	Position int    `json:"position,omitempty"`
	Handle   string `json:"handle,omitempty"`
	Attach   string `json:"attach"`
}

// sendOnly is craze send without --wait, over conn, attached: the prompt sent
// once the attachment's incarnation is proven — through a gate, so a signal
// or --timeout's end before it went sends nothing ever — and its answer
// printed with the handle a wait --from follows it by. The handle's place is
// just before the target's first event, as the waiter's is (X1): the turn's
// `started` (which reaches the stream before the prompt's answer), or the
// row's `queued`, whose event says its place too; found within readBound. A
// turn whose start the stream did not show is named without a handle — a
// place before it could not tell its events from another turn's — and a row
// from the attachment's own place, which a queued row's wait can start at.
func (r *promptJob) sendOnly(conn *waitConn) error {
	short := psShort(conn.sessionID)
	it, err := conn.st.Next(r.ctx)
	if err == nil && (it.Kind != remote.KindAttached || it.Reply == nil) {
		err = errors.New("its attachment did not answer first")
	}
	if err != nil {
		return r.failBefore(exitf(1, "%s: session %s: %s", sendWhat, short, sanitizeLine(err.Error())))
	}
	after := it.Reply.After
	pctx, cancel := context.WithTimeout(r.ctx, readBound)
	ident, err := proveIdentity(pctx, conn.c, conn.sessionID, after.Incarnation)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, errNotThisHost):
		return r.failBefore(exitf(1, "%s: the session's host changed before the prompt was sent; it was not sent", sendWhat))
	default:
		return r.failBefore(exitf(1, "%s: the session did not answer before the prompt was sent; it was not sent: %s", sendWhat, sanitizeLine(err.Error())))
	}
	g := &submitGate{submit: submitPrompt(conn, r.text)}
	answers := make(chan submitAnswer, 1)
	go func() {
		t, err := g.run(context.Background(), ident)
		answers <- submitAnswer{target: t, ident: ident, err: err}
	}()
	a := r.answer(answers)
	sendFate(g)
	// The fate and the answer that names its target are read together, from
	// the gate (C3 review r1, 3): an answer that came after this command gave
	// up waiting for it is still the prompt's.
	fate, target := g.close()
	follow := "; craze wait " + short + " follows the session"
	switch fate {
	case promptUnsent, promptRefused:
		// Nothing reached the session; a reload's host is not this
		// command's to stop (spawned is craze new --wait's alone).
		undone := r.undo(false)
		msg := "interrupted before the prompt was sent"
		switch {
		case fate == promptRefused:
			msg = a.err.Error()
		case r.timedOut():
			msg = "timed out before the prompt was sent"
		}
		return r.fail(&waitFailure{msg: msg + undone.note()})
	case promptPending:
		msg := "interrupted before the prompt's answer; it may have reached the session" + follow
		if r.timedOut() {
			msg = "timed out before the prompt's answer; it may have reached the session" + follow
		}
		if a.err != nil && !errors.Is(a.err, errNoAnswer) {
			msg = a.err.Error()
			if !strings.Contains(msg, "craze wait ") {
				msg += follow
			}
		}
		return r.fail(&waitFailure{msg: msg})
	}
	h := turnHandle{incarnation: after.Incarnation, seq: after.Seq, target: target}
	place, found := sendPlace(r, conn, target)
	if found {
		h.seq = place.seq
	}
	rec := sendReceipt{Session: conn.sessionID, Short: short, Provider: conn.entry.Provider, Attach: "craze attach --session " + short}
	var line string
	switch target.kind {
	case targetTurn:
		rec.Status, rec.Turn = "started", target.id
		line = "started " + psLine(target.id)
		if found {
			rec.Handle = h.String()
		}
	case targetRow:
		rec.Status, rec.Row, rec.Handle = "queued", target.id, h.String()
		line = "queued " + psLine(target.id)
		pos, known := place.pos+1, found
		if !known {
			// Its queued event was not seen (a restore, the stream's end,
			// the deadline): the queue as the session holds it now, read
			// under the attachment's own incarnation (C3 review r1, 8).
			pos, known = r.queuePosition(conn, ident, target.id)
		}
		if known {
			rec.Position = pos
			line += fmt.Sprintf(" (position %d)", pos)
		}
	default:
		// Taken, its target unknown: said as what it is, and followed from
		// the session's current turn (X24's no-handle form).
		rec.Status, line = "sent", "sent"
	}
	out := r.cmd.OutOrStdout()
	if r.out.json {
		b, err := json.Marshal(rec)
		if err != nil {
			return exitf(1, "%s: the outcome: %v", sendWhat, err)
		}
		_, err = fmt.Fprintf(out, "%s\n", b)
		return err
	}
	_, err = fmt.Fprintf(out, "%s\nnext: %s\n", line, waitCommand(short, rec.Handle))
	return err
}

// sendFate sees sendOnly about to read its gate's fate: a no-op, and the
// barrier a test lands the prompt's answer at, after the command gave up
// waiting for it.
var sendFate = func(*submitGate) {}

// sendPlace is placeOf, a seam: a test makes the stream show nothing.
var sendPlace = func(r *promptJob, conn *waitConn, target waitTarget) (streamPlace, bool) {
	return r.placeOf(conn, target)
}

// queuePosition is row's place in the session's queue (1 is next), read from
// session.state under ident — the identity proven to be the attachment's
// incarnation's — and taken only if the client still holds it after the
// read: a reconnect to another incarnation answers about another queue. Not
// found, or not readable, is not known.
func (r *promptJob) queuePosition(conn *waitConn, ident uint64, row string) (int, bool) {
	ctx, cancel := context.WithTimeout(r.ctx, readBound)
	defer cancel()
	if conn.c.Identity() != ident {
		return 0, false
	}
	var st protocol.StateResult
	err := conn.c.Call(ctx, protocol.MethodSessionState, protocol.StateParams{SessionID: conn.sessionID}, &st)
	if err != nil || conn.c.Identity() != ident {
		return 0, false
	}
	for i, raw := range st.Queue {
		if q, err := agent.DecodeQueuedPrompt(raw); err == nil && q.ID == row {
			return i + 1, true
		}
	}
	return 0, false
}

// answer is the prompt's answer from answers — or, once a signal or --timeout's
// end has come with it still out, the answer within cleanupWait (§3.1: a
// reply still out is waited for), a second signal ending that wait at once:
// errNoAnswer then.
func (r *promptJob) answer(answers <-chan submitAnswer) submitAnswer {
	select {
	case a := <-answers:
		return a
	case <-r.ctx.Done():
	}
	if r.signalled() {
		// The first signal is the one that ended the work: taken, so only a
		// second one ends the wait.
		select {
		case <-r.relayed:
		default:
		}
	}
	t := time.NewTimer(cleanupWait)
	defer t.Stop()
	select {
	case a := <-answers:
		return a
	case <-t.C:
	case <-r.relayed:
	}
	return submitAnswer{err: errNoAnswer}
}

// streamPlace is where a prompt's target first shows on the stream: the seq
// just before its first event, and — a queued row's — its place in the queue
// then (0 is next).
type streamPlace struct {
	seq uint64
	pos int
}

// placeOf reads conn's stream, within readBound and the run's own work, for
// target's first event: its turn's `started`, or its row's `queued` (the
// sendOnly comment). A restore, the stream's end or loss, or an event it
// cannot read ends the look: not found.
func (r *promptJob) placeOf(conn *waitConn, target waitTarget) (streamPlace, bool) {
	ctx, cancel := context.WithTimeout(r.ctx, readBound)
	defer cancel()
	for {
		it, err := conn.st.Next(ctx)
		if err != nil {
			return streamPlace{}, false
		}
		switch it.Kind {
		case remote.KindEvent:
		case remote.KindAttached, remote.KindSynchronized, remote.KindReady, remote.KindPresence:
			continue
		default:
			return streamPlace{}, false
		}
		ev, err := agent.DecodeEvent(string(it.Body))
		if err != nil {
			return streamPlace{}, false
		}
		switch {
		case target.kind == targetTurn && ev.Type == agent.EventTurn && ev.Turn != nil &&
			ev.Turn.Phase == agent.TurnStarted && ev.Turn.ID == target.id:
			return streamPlace{seq: it.Seq - 1}, true
		case target.kind == targetRow && ev.Type == agent.EventQueue && ev.Queue != nil &&
			ev.Queue.ID == target.id && ev.QueueChange == agent.QueueQueued:
			return streamPlace{seq: it.Seq - 1, pos: ev.QueuePos}, true
		}
	}
}
