package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/version"
)

// Ensure finds the namespace's hub, or starts one (plan 032 §3.8). Every
// step is bounded by its context:
//
//  1. The record (rundir.ReadHubRecord). A non-empty start token that its
//     pid no longer carries is a stale record — no hub — and is never
//     dialled; an empty one, or one that cannot be checked, is a hub whose
//     identity cannot be verified, which is dialled all the same, its hello
//     the identity check for using it. The hello (helloTimeout) must answer
//     endpoint kind hub, the record's hub id, a protocol this build speaks
//     and the capabilities the caller needs — an older hub without them is
//     reported (LacksError), never used and never replaced. EOF or
//     unavailable/closing during the hello is no hub.
//  2. Otherwise `craze hub` is spawned (Command, through
//     hostspawn.StartCmd: re-executed, a session of its own, stdio on
//     /dev/null, the ready pipe on fd 3, HubChildEnv=1, the environment
//     contract (ChildEnv), HOME its working directory) and its ready line
//     read within readyWait: ok in the caller's namespace is dialled as in
//     1; held is a rendezvous — the record polled every rendezvousPoll for a
//     hub that answers as in 1, until the holder is gone, rendezvousWait
//     has passed or the context ends; a hub that does not answer in time is
//     ended (SIGTERM, then SIGKILL) and reaped.
//  3. A wedged hub (P17): two hellos that time out — not refused, not EOF —
//     against a hub whose pid carries its record's non-empty start token:
//     SIGTERM to that pid (its identity checked again just before), up to
//     wedgedTermWait for it to go, SIGKILL if it has not, and then 2. A hub
//     whose identity cannot be verified is never signalled: it is reported
//     (WedgedError).
//  4. A failure is tried once more after retryPause, when the context
//     leaves room for it — never an answer that another try cannot change
//     (ErrNoHub, LacksError, the context's end).
//
// It answers the hub's socket, for the caller to dial. A test binary spawns
// no hub unless the test installs Command: Ensure is ErrNoHub there before it
// reads anything (§3.18's leak guard).
func Ensure(ctx context.Context, env rundir.Env, need protocol.ConnectionCapabilities) (string, error) {
	if Command == nil && testing.Testing() {
		return "", ErrNoHub
	}
	ns, err := rundir.Namespace(env.CrazeDir)
	if err != nil {
		return "", err
	}
	e := &ensurer{env: env, ns: ns, need: need}
	sock, err := e.attempt(ctx)
	if err == nil || final(err) || !room(ctx, retryPause) {
		return sock, err
	}
	t := time.NewTimer(retryPause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.C:
	}
	return e.attempt(ctx)
}

// Command builds the hub's command, `craze hub`, from argv (its first word
// "hub"). nil — production — re-executes os.Executable(); in a test binary nil
// is no hub at all (ErrNoHub), and a test that wants one installs its own (the
// test binary run as craze, never the real craze hub). Ensure sets the rest:
// its environment (from cmd.Env, or this process's own), its working
// directory, its session, stdio and ready pipe.
var Command func(argv []string) (*exec.Cmd, error)

// ErrNoHub is Ensure in a test binary that has not installed Command: a test
// never spawns a hub it did not ask for, so a production path that would is
// told at once and takes its fallback.
var ErrNoHub = errors.New("hub: no hub in a test binary unless the test installs hub.Command")

// Ensure's bounds and seams: variables only so a test can change them (never
// in parallel).
var (
	// helloTimeout bounds one hello: the dial, the request and the answer.
	helloTimeout = time.Second
	// readyWait bounds a spawned hub's ready line.
	readyWait = 5 * time.Second
	// readyTimer is the ready wait's timer, time.After: a test fires it by
	// hand.
	readyTimer = time.After
	// rendezvousWait bounds a held spawn's wait for the holder to answer.
	rendezvousWait = 5 * time.Second
	// rendezvousPoll is how often the rendezvous reads the record.
	rendezvousPoll = 20 * time.Millisecond
	// wedgedTermWait is how long a wedged hub sent SIGTERM has to go before
	// SIGKILL; wedgedKillWait how long the SIGKILL has.
	wedgedTermWait = 2 * time.Second
	wedgedKillWait = 2 * time.Second
	// contenderGrace is how long a spawned hub that has not answered has to
	// exit on SIGTERM before its group is killed (hostspawn.Child.End).
	contenderGrace = 2 * time.Second
	// retryPause is the pause before Ensure's one retry.
	retryPause = 200 * time.Millisecond
	// startToken is rundir.StartToken: a test plays a process whose
	// identity cannot be read.
	startToken = rundir.StartToken
)

// LacksError is a hub that answered, and cannot do what the caller needs: an
// older craze, which Ensure does not use and does not replace — it exits
// when idle, and the next Ensure after that starts this craze's.
type LacksError struct {
	// Version is the hub's craze version (its hello's endpoint).
	Version string
	// Missing is what it cannot do, in words: "create sessions", …
	Missing []string
}

func (e *LacksError) Error() string {
	return fmt.Sprintf("this hub (craze %s) cannot %s; it exits when idle", e.Version, strings.Join(e.Missing, " or "))
}

// WedgedError is a hub that does not answer and is not replaced (P17): its
// identity cannot be verified, so it is never signalled.
type WedgedError struct {
	PID int
	Why string
}

func (e *WedgedError) Error() string {
	return fmt.Sprintf("the hub (pid %d) does not answer, and %s, so it is not replaced", e.PID, e.Why)
}

// missing is what have lacks of need, in LacksError's words.
func missing(have, need protocol.ConnectionCapabilities) []string {
	var out []string
	check := func(n, h bool, what string) {
		if n && !h {
			out = append(out, what)
		}
	}
	check(need.SessionCreate, have.SessionCreate, "create sessions")
	check(need.RosterSubscribe, have.RosterSubscribe, "subscribe to the session roster")
	check(need.Connect, have.Connect, "connect to a session")
	check(need.Multiplex, have.Multiplex, "multiplex sessions")
	check(need.Snapshot, have.Snapshot, "take a snapshot")
	check(need.AttachWhenNow, have.AttachWhenNow, "attach with when now")
	return out
}

// final reports whether err is an answer another attempt cannot change.
func final(err error) bool {
	var lacks *LacksError
	var ns *nsError
	return errors.Is(err, ErrNoHub) || errors.As(err, &lacks) || errors.As(err, &ns) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// room reports whether ctx has not ended and leaves more than d.
func room(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > d
}

// nsError is a spawned hub serving another namespace than its spawner's: the
// environment contract handed it another craze directory.
type nsError struct{ got, want string }

func (e *nsError) Error() string {
	return fmt.Sprintf("the hub it started serves namespace %s, not this craze's %s", e.got, e.want)
}

// ensurer is one Ensure call's.
type ensurer struct {
	env  rundir.Env
	ns   string
	need protocol.ConnectionCapabilities
}

// attempt is steps 1–3 once.
func (e *ensurer) attempt(ctx context.Context) (string, error) {
	sock, found, err := e.find(ctx)
	if err != nil || found {
		return sock, err
	}
	return e.spawn(ctx)
}

// identity is what a record's start token says of its pid now.
type identity int

const (
	// verified: the pid carries the record's token — the hub's process.
	verified identity = iota
	// unverifiable: no token, or one that cannot be checked here.
	unverifiable
	// stale: the pid is gone, or another process's now.
	stale
)

// identify tells rec's hub process (step 1).
func identify(rec rundir.HubRecord) identity {
	if rec.StartToken == "" {
		return unverifiable
	}
	tok, err := startToken(rec.PID)
	switch {
	case errors.Is(err, rundir.ErrNoProcess):
		return stale
	case err != nil:
		return unverifiable
	case tok != rec.StartToken:
		return stale
	}
	return verified
}

// carries reports whether rec's pid still carries its token, as checked
// now: the identity a signal to it needs.
func carries(rec rundir.HubRecord) bool {
	if rec.StartToken == "" {
		return false
	}
	tok, err := startToken(rec.PID)
	return err == nil && tok == rec.StartToken
}

// find is steps 1 and 3: the record's hub, answering — found — or no usable
// hub, for a spawn; an error is an answer of its own.
func (e *ensurer) find(ctx context.Context) (string, bool, error) {
	rec, _, err := rundir.ReadHubRecord(e.env)
	if err != nil {
		// No record, or none that names a hub of this namespace: a spawned
		// hub writes its own over it.
		return "", false, nil
	}
	ident := identify(rec)
	if ident == stale {
		return "", false, nil
	}
	for range 2 {
		res, out, err := dialHello(ctx, rec.Socket, helloTimeout)
		switch out {
		case helloOK:
			return e.use(rec.Socket, rec.HubID, res)
		case helloFailed:
			return "", false, err
		case helloNone:
			return "", false, nil
		}
	}
	return "", false, e.replaceWedged(ctx, rec, ident)
}

// use takes a hello's answer from the hub at sock, which must be wantID's:
// found, or not this hub (another one answers at the path: no error, look
// again), or one this caller cannot use.
func (e *ensurer) use(sock, wantID string, res protocol.HubHelloResult) (string, bool, error) {
	if res.Endpoint.HostID != wantID {
		return "", false, nil
	}
	if !slices.Contains(protocol.SupportedProtocols(), res.Protocol) {
		return "", false, fmt.Errorf("the hub (craze %s) chose protocol %d, which this craze does not speak", res.Endpoint.CrazeVersion, res.Protocol)
	}
	if m := missing(res.Capabilities, e.need); len(m) > 0 {
		return "", false, &LacksError{Version: res.Endpoint.CrazeVersion, Missing: m}
	}
	return sock, true, nil
}

// replaceWedged is step 3, for rec's hub whose two hellos timed out: nil once
// it is gone (a spawn follows), or why it is not replaced.
func (e *ensurer) replaceWedged(ctx context.Context, rec rundir.HubRecord, ident identity) error {
	if ident != verified {
		why := "its identity cannot be verified"
		if rec.StartToken == "" {
			why = "its record carries no start token to tell it from another process by"
		}
		return &WedgedError{PID: rec.PID, Why: why}
	}
	// Checked again just before each signal: what stays open is the instant
	// between the check and the kill (plan 030 X22's residual).
	if !carries(rec) {
		return nil
	}
	_ = syscall.Kill(rec.PID, syscall.SIGTERM)
	if gone(ctx, rec, wedgedTermWait) {
		return nil
	}
	if carries(rec) {
		_ = syscall.Kill(rec.PID, syscall.SIGKILL)
	}
	gone(ctx, rec, wedgedKillWait)
	return ctx.Err()
}

// gone waits up to d, polling, for rec's pid to stop carrying its token —
// exited and reaped, or another process's — whether it did.
func gone(ctx context.Context, rec rundir.HubRecord, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for carries(rec) {
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(rendezvousPoll):
		}
	}
	return true
}

// hubCommand is the hub's command (Command's rule).
func hubCommand() (*exec.Cmd, error) {
	if c := Command; c != nil {
		return c([]string{"hub"})
	}
	if testing.Testing() {
		return nil, ErrNoHub
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, "hub"), nil
}

// workDir is the hub's working directory: home when it is a directory, else
// "/" — never the spawner's, which a hub living past it must not pin.
func workDir(home string) string {
	if home != "" {
		if fi, err := os.Stat(home); err == nil && fi.IsDir() {
			return home
		}
	}
	return "/"
}

// spawn is step 2.
func (e *ensurer) spawn(ctx context.Context) (string, error) {
	cmd, err := hubCommand()
	if err != nil {
		return "", err
	}
	environ := cmd.Env
	if environ == nil {
		environ = os.Environ()
	}
	cwd, _ := os.Getwd()
	cmd.Env = ChildEnv(environ, cwd)
	cmd.Dir = workDir(e.env.Home)
	child, r, err := hostspawn.StartCmd(cmd, HubChildEnv, "", "")
	if err != nil {
		return "", fmt.Errorf("start craze hub: %w", err)
	}
	line, failure, why := readReady(ctx, r)
	_ = r.Close()
	switch failure {
	case 0:
	case hostspawn.Cancelled:
		// SIGTERM at once; the rest of its end is the reaper's.
		go child.End(contenderGrace)
		return "", ctx.Err()
	case hostspawn.Exited:
		return "", fmt.Errorf("the hub it started (pid %d) exited before it answered", child.PID())
	case hostspawn.TimedOut:
		child.End(contenderGrace)
		return "", fmt.Errorf("the hub it started (pid %d) did not answer within %v, and was ended", child.PID(), readyWait)
	default:
		child.End(contenderGrace)
		return "", fmt.Errorf("the hub it started (pid %d) answered no ready line (%s), and was ended", child.PID(), why)
	}
	switch {
	case line.OK && line.NS != e.ns:
		return "", &nsError{got: line.NS, want: e.ns}
	case line.OK:
		res, out, err := dialHello(ctx, line.Socket, helloTimeout)
		switch out {
		case helloOK:
			sock, found, err := e.use(line.Socket, line.HubID, res)
			if found || err != nil {
				return sock, err
			}
			return "", fmt.Errorf("another hub than the one it started (%s) answers at %s", line.HubID, line.Socket)
		case helloFailed:
			return "", err
		}
		return "", fmt.Errorf("the hub it started (pid %d) does not answer at %s", child.PID(), line.Socket)
	case line.Held != nil:
		return e.rendezvous(ctx, *line.Held)
	}
	return "", fmt.Errorf("craze hub could not start: %s", line.Error)
}

// errHolderGone is a held spawn whose holder exited before it answered: the
// next attempt may take the lock itself.
var errHolderGone = errors.New("the hub that held the lock exited before it answered")

// rendezvous waits for the hub that holds the lock (held) to answer as in
// step 1: the record polled every rendezvousPoll, a record not tried yet (by
// its (dev, ino)) — or one whose hello timed out — said hello to, until a hub
// answers, the holder's pid is gone, rendezvousWait passes or ctx ends.
func (e *ensurer) rendezvous(ctx context.Context, held ReadyHeld) (string, error) {
	deadline := time.Now().Add(rendezvousWait)
	var tried rundir.FileID
	again := false
	for {
		rec, id, err := rundir.ReadHubRecord(e.env)
		if err == nil && identify(rec) != stale && (id != tried || again) {
			tried, again = id, false
			res, out, err := dialHello(ctx, rec.Socket, helloTimeout)
			switch out {
			case helloOK:
				if sock, found, err := e.use(rec.Socket, rec.HubID, res); found || err != nil {
					return sock, err
				}
			case helloFailed:
				return "", err
			case helloTimedOut:
				again = true
			}
		}
		if held.PID > 0 && !processAlive(held.PID) {
			return "", errHolderGone
		}
		if !time.Now().Before(deadline) {
			who := "a hub that has not written its line yet"
			if held.PID > 0 {
				who = fmt.Sprintf("pid %d, hub %s", held.PID, held.HubID)
			}
			return "", fmt.Errorf("the hub holding the namespace's lock (%s) did not answer within %v", who, rendezvousWait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(rendezvousPoll):
		}
	}
}

// processAlive reports whether pid names a process (a zombie counts).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// readReady reads a spawned hub's ready line within readyWait, or until ctx
// ends. The read runs on a goroutine of its own, which the caller's close of
// r ends.
func readReady(ctx context.Context, r *os.File) (ReadyLine, hostspawn.Failure, string) {
	type result struct {
		line    ReadyLine
		failure hostspawn.Failure
		why     string
	}
	got := make(chan result, 1)
	go func() {
		raw, failure, why := hostspawn.ReadLine(r)
		if failure != 0 {
			got <- result{failure: failure, why: why}
			return
		}
		line, why := parseReady(raw)
		if why != "" {
			got <- result{failure: hostspawn.Malformed, why: why}
			return
		}
		got <- result{line: line}
	}()
	select {
	case res := <-got:
		return res.line, res.failure, res.why
	case <-readyTimer(readyWait):
		return ReadyLine{}, hostspawn.TimedOut, ""
	case <-ctx.Done():
		return ReadyLine{}, hostspawn.Cancelled, ""
	}
}

// helloOutcome is what one hello to a hub came to.
type helloOutcome int

const (
	// helloOK: a hub answered (its result is the hello's).
	helloOK helloOutcome = iota
	// helloNone: no hub — refused, absent, EOF, a reset, or a hub closing.
	helloNone
	// helloTimedOut: nothing within the bound — not refused, not EOF: a hub
	// that is not answering (P17 counts these).
	helloTimedOut
	// helloFailed: an answer that is an error of its own (the error), or the
	// caller's context ending.
	helloFailed
)

// ensureClient is who Ensure says it is.
var ensureClient = protocol.ClientInfo{Kind: "cli", Name: "craze hub ensure", Version: version.Version}

// dialHello dials the hub at socket and says hello, within wait and ctx: the
// hub's answer, or what the attempt came to.
func dialHello(ctx context.Context, socket string, wait time.Duration) (protocol.HubHelloResult, helloOutcome, error) {
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(dctx, "unix", socket)
	if err != nil {
		return protocol.HubHelloResult{}, classify(ctx, err), ctx.Err()
	}
	defer nc.Close()
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		return protocol.HubHelloResult{}, helloFailed, errors.New("not a unix socket connection")
	}
	if err := rundir.DialCheck(os.Geteuid())(uc); err != nil {
		return protocol.HubHelloResult{}, helloFailed, fmt.Errorf("the hub's socket %s: %w", socket, err)
	}
	// The caller's context's end closes the connection; the hello's own
	// bound is the deadline, so its passing reads as a timeout, never as a
	// close.
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	_ = nc.SetDeadline(deadline)
	params, err := json.Marshal(protocol.HelloParams{Protocols: protocol.SupportedProtocols(), Client: ensureClient})
	if err != nil {
		return protocol.HubHelloResult{}, helloFailed, err
	}
	if err := protocol.WriteLine(nc, protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage("1"),
		Method: protocol.MethodHello, Params: params}); err != nil {
		return protocol.HubHelloResult{}, classify(ctx, err), ctx.Err()
	}
	line, err := protocol.NewLineReader(nc, protocol.OutboundLineMax).ReadLine()
	if err != nil {
		return protocol.HubHelloResult{}, classify(ctx, err), ctx.Err()
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return protocol.HubHelloResult{}, helloFailed, fmt.Errorf("the hub's hello answer: %w", err)
	}
	if resp.Error != nil {
		if resp.Error.Data.Code == protocol.CodeUnavailable && resp.Error.Data.Reason == protocol.ReasonClosing {
			return protocol.HubHelloResult{}, helloNone, nil
		}
		return protocol.HubHelloResult{}, helloFailed, fmt.Errorf("the hub refused hello: %w", resp.Error)
	}
	var res protocol.HubHelloResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return protocol.HubHelloResult{}, helloFailed, fmt.Errorf("the hub's hello result: %w", err)
	}
	if res.Endpoint.Kind != protocol.EndpointHub {
		return protocol.HubHelloResult{}, helloFailed, fmt.Errorf("an endpoint of kind %q answers at the hub's socket %s", res.Endpoint.Kind, socket)
	}
	return res, helloOK, nil
}

// classify is a failed dial, write or read: the caller's context ending is
// failed (its error is the caller's), a timeout — or a listener whose
// backlog is full, which is a hub not accepting — is timed out, and anything
// else (refused, absent, EOF, a reset, a broken pipe) is no hub.
func classify(ctx context.Context, err error) helloOutcome {
	var ne net.Error
	switch {
	case ctx.Err() != nil:
		return helloFailed
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &ne) && ne.Timeout(), errors.Is(err, syscall.EAGAIN):
		return helloTimedOut
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ENOENT),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return helloNone
	}
	return helloNone
}
