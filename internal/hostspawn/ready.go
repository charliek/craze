package hostspawn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/charliek/craze/internal/rundir"
)

// The ready handshake's spawner end (plan 030 §3.4): the line a spawned host
// writes on its fd 3 (ReadyLine), and its reading (ReadReady, ParseReady).
// The host end — taking the pipe, writing the line — is craze serve's.

// The environment a spawner hands its host (Start): the ready pipe's
// descriptor, and the mark of a host spawned detached. The host removes both
// from its own environment the moment it starts, so neither reaches its agent.
const (
	ReadyFDEnv   = "CRAZE_READY_FD"
	HostChildEnv = "CRAZE_HOST_CHILD"
)

// ReadyLineMax bounds the ready line, its newline included: the spawner reads
// no further (ParseReady), and a longer line is a host it terminates.
const ReadyLineMax = 64 << 10

// ReadyLine is the one line a spawned host writes on its ready pipe. OK names
// the host (HostID — the id the spawner minted and passed, --host-id), its
// socket, its session's craze id and the craze that serves it (the host may be
// a newer binary than its launcher, when craze was upgraded on disk between
// the two). Not OK carries the host's refusal (Error) and, for a session
// another host holds, Held. Refused says the refusal is of what the host was
// asked to run (craze serve's refusedChoice) — no such session, a row that
// cannot run where it ran, a flag its provider cannot take, a claim to try
// again — which choosing again may change; without it, and without Held, the
// host could not come up (its socket would not bind, its claim could not be
// taken at all, …), and the launcher says so with the host's log and the
// opt-out. Absent means false: a host that does not say is taken as one that
// could not come up, which names its log. The spawner takes a line only when
// it carries everything its branch needs (ReadyLine.invalid), and ignores a
// member it does not know.
type ReadyLine struct {
	OK             bool       `json:"ok"`
	HostID         string     `json:"hostId,omitempty"`
	Socket         string     `json:"socket,omitempty"`
	CrazeSessionID string     `json:"crazeSessionId,omitempty"`
	CrazeVersion   string     `json:"crazeVersion,omitempty"`
	Error          string     `json:"error,omitempty"`
	Held           *ReadyHeld `json:"held,omitempty"`
	Refused        bool       `json:"refused,omitempty"`
}

// ReadyHeld is who holds the session a host could not claim, as its lock
// names it: the holder's host id and its pid — each zero when the lock names
// no holder yet (the instant between a claim's flock and its line, "pid ?") —
// and the session. The spawner waits for that host's registry entry to carry
// the session (Rendezvous), and the pid tells it at once when the holder has
// died meanwhile rather than after its whole wait (plan 030 §3.4's "a holder
// that crashes or releases meanwhile").
type ReadyHeld struct {
	HostID         string `json:"hostId"`
	PID            int    `json:"pid,omitempty"`
	CrazeSessionID string `json:"crazeSessionId"`
}

// HeldError is the claim's refusal a held answer reports, as the host's own
// claim had it: what a caller words, and finds with errors.As.
func (h *ReadyHeld) HeldError() *rundir.HeldError {
	return &rundir.HeldError{CrazeID: h.CrazeSessionID, Holder: rundir.Holder{PID: h.PID, HostID: h.HostID}}
}

// Failure is why no ready line was read (ReadReady, ParseReady); 0 is a line
// read.
type Failure int

const (
	// Exited: EOF with no line — the host exited, or closed its pipe
	// unwritten, before it was ready.
	Exited Failure = iota + 1
	// TimedOut: no line within ReadyWait.
	TimedOut
	// Malformed: a line that is not a ready line.
	Malformed
	// Oversized: no line within ReadyLineMax bytes.
	Oversized
	// Cancelled: the caller's context ended first.
	Cancelled
)

// ReadReady reads the host's ready line from r: the line, or why there is
// none — a failure and what it was — within ReadyWait, or until ctx ends. The
// read runs on a goroutine of its own, which the caller's close of r ends. A
// line already read when the wait ends — the timer and the read, or ctx's end
// and the read, ready at once — is the answer (plan 032 r43 3): a host that
// has announced itself is never taken for one that has not.
func ReadReady(ctx context.Context, r *os.File) (ReadyLine, Failure, string) {
	type result struct {
		line    ReadyLine
		failure Failure
		why     string
	}
	got := make(chan result, 1)
	go func() {
		line, failure, why := ParseReady(r)
		got <- result{line, failure, why}
		readyRead()
	}()
	readySelecting()
	var failure Failure
	select {
	case res := <-got:
		return res.line, res.failure, res.why
	case <-ReadyTimer(ReadyWait):
		failure = TimedOut
	case <-ctx.Done():
		failure = Cancelled
	}
	select {
	case res := <-got:
		return res.line, res.failure, res.why
	default:
		return ReadyLine{}, failure, ""
	}
}

// ReadReady's test seams (never in parallel), no-ops in production:
// readyRead runs on its read's goroutine once the outcome is there to take,
// and readySelecting just before ReadReady waits on its outcomes — so a test
// can have the line read and the context ended both before the wait.
var (
	readyRead      = func() {}
	readySelecting = func() {}
)

// ParseReady reads one ready line from r: every byte up to its newline, at
// most ReadyLineMax with it, decoded. EOF before any byte is a host that
// exited, or closed the pipe unwritten, before it was ready; EOF within a line
// is a malformed one.
func ParseReady(r io.Reader) (ReadyLine, Failure, string) {
	raw, failure, why := ReadLine(r)
	if failure != 0 {
		return ReadyLine{}, failure, why
	}
	var line ReadyLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return ReadyLine{}, Malformed, err.Error()
	}
	if why := line.invalid(); why != "" {
		return ReadyLine{}, Malformed, why
	}
	return line, 0, ""
}

// ReadLine reads one line from a ready pipe r — a host's, or the hub's (plan
// 032 §3.5), whose line is its own — without its newline: every byte up to
// the newline, at most ReadyLineMax with it. EOF before any byte is Exited;
// EOF within a line is Malformed, as is a read that fails; no newline within
// ReadyLineMax bytes is Oversized. What the line says is its reader's.
func ReadLine(r io.Reader) ([]byte, Failure, string) {
	br := bufio.NewReaderSize(io.LimitReader(r, ReadyLineMax+1), 4096)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > ReadyLineMax {
			return nil, Oversized, ""
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(buf) == 0 && errors.Is(err, io.EOF) {
			return nil, Exited, ""
		}
		if errors.Is(err, io.EOF) {
			return nil, Malformed, "the line has no end"
		}
		return nil, Malformed, err.Error()
	}
	return bytes.TrimSuffix(buf, []byte("\n")), 0, ""
}

// invalid says why a decoded line is not a ready line, "" when it is one
// (astra r5-c3 4): each branch must carry everything its reader acts on, and
// nothing of the other's. ok names the host in a host id's form, the socket
// as an absolute path, the session by a craze id (a token: it names a lock),
// and the craze serving it. Not ok says why, and a holder, when there is one,
// names the session by its craze id — a held line naming none would have the
// rendezvous match any registry entry that carries no session yet — and its
// host by a host id's form, or by nothing at all with no pid either: a lock
// that names nobody yet (plan 030 X19, X21: any live host serving the session
// is then the holder). A member the line has that is not one of these is
// ignored — a host may be a newer craze than its launcher.
func (l ReadyLine) invalid() string {
	if l.OK {
		switch {
		case l.Error != "" || l.Held != nil || l.Refused:
			return "ok, and a refusal too"
		case !rundir.ValidHostID(l.HostID):
			return fmt.Sprintf("ok names no host id (%q)", l.HostID)
		case !filepath.IsAbs(l.Socket):
			return fmt.Sprintf("ok names no socket (%q)", l.Socket)
		case !rundir.ValidToken(l.CrazeSessionID):
			return fmt.Sprintf("ok names no session (%q)", l.CrazeSessionID)
		case l.CrazeVersion == "":
			return "ok names no craze version"
		}
		return ""
	}
	h := l.Held
	switch {
	case l.Error == "":
		return "not ok, and no reason"
	case h == nil:
		return ""
	case !rundir.ValidToken(h.CrazeSessionID):
		return fmt.Sprintf("held names no session (%q)", h.CrazeSessionID)
	case h.PID < 0 || h.PID > math.MaxInt32:
		return fmt.Sprintf("held names pid %d", h.PID)
	case h.HostID == "" && h.PID != 0:
		return fmt.Sprintf("held names pid %d and no host", h.PID)
	case h.HostID != "" && !rundir.ValidHostID(h.HostID):
		return fmt.Sprintf("held names no host id (%q)", h.HostID)
	}
	return ""
}
