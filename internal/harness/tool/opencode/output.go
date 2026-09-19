package opencode

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// How bash's Run stays within its bounds whatever the filesystem does.
//
// Fantasy waits for a tool with no timeout, so a Run that does not return
// hangs the turn, then the session's Close, then the TUI's exit. A filesystem
// that does not answer — a stalled network or FUSE mount under the workspace,
// the harness's home or the temporary directory — must therefore never be on
// Run's return path. The invariant, which every goroutine and lock below is
// arranged around:
//
//	No lock taken by Run's return path or by the pipe reader is ever held
//	across a syscall that touches the harness home, the spill file, the
//	workdir or the temporary directory; and every wait on Run's return path
//	is bounded by a timer, or by a signal that fires regardless of the
//	filesystem (ctx done, Env.Closing, the leader's exit, the pipe's close).
//
// The goroutines of one call, and what each may block on:
//
//   - Run itself (bash.go): waits in launch, supervise, collect, finish and
//     spiller.wait, each bounded as the invariant says. It makes no filesystem
//     call: the pipe's read end, which it closes, is not a file.
//   - The starter (bashCall.launch): checks the workdir (stat), makes the
//     temporary directory (mkdirAll) and starts the command (exec). It may
//     stall on any of them; launch waits for it only until ctx is done, the
//     session closes or the call's timeout passes, then abandons it, and it
//     kills whatever it goes on to start.
//   - The watcher (group.watch, process.go): waits on the leader with waitid
//     and wait4, and on the release channel supervise closes. No filesystem.
//   - The reader: io.Copy from the pipe through the redaction writer into
//     output.Write. It blocks on the pipe, which collect closes, and on
//     output.mu and spiller.mu, both held for memory work only. No filesystem.
//   - The progress goroutine (output.report): takes output.mu for a snapshot,
//     releases it, then delivers to the consumer. No filesystem.
//   - The spill writer (spiller.run): the one goroutine that touches the spill
//     file — open, write, close — and it holds no lock while it does. It
//     takes spiller.mu only to move bytes between the backlog and itself.
//
// The locks: output.mu guards the tail and the counters; spiller.mu guards the
// backlog and the ended and abandoned flags. Each is held for a few memory
// operations and released before anything else is called; neither is ever
// held across a syscall, a channel send that could wait, or the other lock in
// the reverse order (output.mu is taken first, then spiller.mu, and only
// there).
//
// What is accepted: a goroutine inside a syscall that never returns cannot be
// interrupted, so the starter or the spill writer may leak until the
// filesystem answers. Each leaks a bounded amount — the starter its command
// and environment, the writer the one chunk it was writing, at most
// spillBacklog — and nothing waits on either once it is abandoned: the
// writer's backlog is dropped on every abandonment path (spiller.abandon), and
// a partial spill file is left where it is, for tool.Sweep, rather than
// removed by name: a remove is another filesystem call, and a name may by then
// belong to another file. The result never names such a file.
//
// How bash keeps a command's output (plan 019 §3.9).
const (
	// tailBytes is how much of a command's output is held in memory: twice
	// what the model is shown, as in opencode (shell.ts:439), so the tail it
	// is shown never starts where memory does.
	tailBytes = 2 * tool.MaxBytes
	// maxSpillBytes caps one call's spill file. Past it the file keeps the
	// first maxSpillBytes of the output and the notice says the rest was not
	// saved; the output is still read to its end, and its tail kept (NOTICE:
	// opencode has no cap, and a runaway command would fill the disk).
	maxSpillBytes = 100 << 20
	// spillBacklog bounds the output waiting for the spill writer. A writer
	// that falls this far behind is abandoned, never waited on.
	spillBacklog = 8 << 20
	// spillWait bounds the wait, once the output has ended, for the spill
	// writer to finish its queue. A file it has not finished by then is
	// abandoned: the result says the output could not be saved.
	spillWait = time.Second
	// flushWait bounds the reading of what is left in the pipe once the
	// command's group is dead: long enough to empty any pipe buffer many
	// times over, since the reader waits on nothing but the pipe.
	flushWait = 500 * time.Millisecond
	// readerWait bounds each wait, past flushWait, for the reader to return.
	readerWait = 100 * time.Millisecond
)

// collect stops the reading of the output once the command's group is dead,
// and reports whether the reader returned and whether it read the output to
// its end. Something that left the group, or a leader SIGKILL could not end,
// may hold the pipe open and write on for ever, so the reader gets until
// flushWait has passed to empty the pipe — its reads get that deadline — and
// the pipe is then closed. copyErr is the reader's error, set before copied
// closes: nil when it reached the end of the output.
//
// A reader that has not returned readerWait after that — it cannot block on
// anything but the pipe, so this does not happen — is abandoned: its
// goroutine may leak, and the bytes its redaction writer held back are lost
// with it. Either way the result then says the output may be incomplete.
func collect(r *os.File, copied <-chan struct{}, copyErr *error) (returned, complete bool) {
	_ = r.SetReadDeadline(time.Now().Add(flushWait)) // an error leaves the timer below
	wait := time.NewTimer(flushWait + readerWait)
	defer wait.Stop()
	select {
	case <-copied:
		return true, *copyErr == nil
	case <-wait.C:
	}
	_ = r.Close()
	select {
	case <-copied:
		return true, false
	case <-time.After(readerWait):
		return false, false
	}
}

// spillFile is where the spill writer puts the output: an *os.File from
// tool.OpenSpill, or a test's stand-in.
type spillFile interface {
	io.Writer
	Close() error
	Name() string
}

// spillOpener opens a call's spill file.
type spillOpener func(home, id string) (spillFile, error)

func openSpill(home, id string) (spillFile, error) {
	f, err := tool.OpenSpill(home, id)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// output is a command's merged stdout and stderr, redacted before it
// arrives: the last tailBytes of it in memory, and — once there is more than
// the model is shown — all of it, up to the cap, in a spill file that a
// goroutine of its own writes (spiller). Writing to it never waits on a
// file, so the reader always empties the pipe. The reader writes it and the
// progress goroutine reads it, so everything is under mu.
type output struct {
	home, id string
	open     spillOpener
	cap      int64 // the most the spill file holds

	mu        sync.Mutex
	buf       []byte   // holds at least the last tailBytes written
	total     int      // bytes written
	newlines  int      // newlines written
	writes    int      // Writes so far, so progress sends only what changed
	spill     *spiller // nil until the output passes MaxBytes
	spilled   int64    // bytes given to the spill writer
	spillDone bool     // nothing more goes to it: it is capped, abandoned or ended
	capped    bool     // output past the cap was not given to it
	ended     bool     // finish has run: a late write, from a reader collect abandoned, is dropped
}

// Write takes the next piece of the output. It never fails and never waits
// on anything but mu.
func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return len(p), nil
	}
	if o.spill == nil && o.total+len(p) > tool.MaxBytes {
		// All of the output so far is still in buf: it is at most MaxBytes,
		// half of what buf keeps (shell.ts:500-513).
		o.spill = startSpiller(o.open, o.home, o.id)
		o.toSpill(o.buf)
	}
	if o.spill != nil {
		o.toSpill(p)
	}
	o.buf = append(o.buf, p...)
	if len(o.buf) >= 2*tailBytes {
		o.buf = o.buf[:copy(o.buf, o.buf[len(o.buf)-tailBytes:])]
	}
	o.total += len(p)
	o.newlines += bytes.Count(p, []byte{'\n'})
	o.writes++
	return len(p), nil
}

// toSpill gives b to the spill writer, up to the cap, without waiting: when
// the writer has fallen spillBacklog behind, or failed, the file is
// abandoned. Past the cap the writer is told the output has ended, and the
// file holds the first cap bytes. It is called under mu.
func (o *output) toSpill(b []byte) {
	if len(b) == 0 || o.spillDone {
		return
	}
	if room := o.cap - o.spilled; int64(len(b)) > room {
		b, o.capped = b[:room], true
	}
	if len(b) > 0 {
		if !o.spill.send(b) {
			o.spill.abandon()
			o.spillDone = true
			return
		}
		o.spilled += int64(len(b))
	}
	if o.capped {
		o.spill.end()
		o.spillDone = true
	}
}

// tail is the last tailBytes of the output. It is called under mu.
func (o *output) tail() []byte { return o.buf[max(len(o.buf)-tailBytes, 0):] }

func (o *output) snapshot() (tail string, writes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.tail()), o.writes
}

// finish ends the output and returns what the model is shown of it —
// opencode's tail of it (shell.ts:568-573) — whether that is less than all
// of it, the measures but for the spill file, whether the spill file stops
// at the cap, and the spill writer, or nil when there is none. A cut output
// has one: when the output was too long in lines but not in bytes, finish
// starts it with the whole output, which is still in memory
// (shell.ts:571-573). Once finish has run, nothing more is taken.
func (o *output) finish() (kept string, cut bool, trunc tool.Truncation, capped bool, spill *spiller) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ended = true
	kept, cut = tailText(string(o.tail()), tool.MaxLines, tool.MaxBytes)
	if cut && o.spill == nil {
		o.spill = startSpiller(o.open, o.home, o.id)
		o.toSpill(o.buf)
	}
	if o.spill != nil {
		if !cut {
			o.spill.abandon() // cannot happen: a spill file means the output was cut
		}
		o.spill.end()
		o.spillDone = true
	}
	trunc = tool.Truncation{
		KeptBytes: len(kept), TotalBytes: o.total,
		KeptLines: strings.Count(kept, "\n") + 1, TotalLines: o.newlines + 1,
	}
	return kept, cut, trunc, o.capped, o.spill
}

// spiller writes a call's spill file on a goroutine of its own, from a
// backlog that output appends to without waiting; each write takes all of
// the backlog, so however small the pipe's reads, a writer that keeps up
// writes large pieces. It opens the file there too: creating the spill
// directory and the file touches the filesystem as much as writing does. That
// goroutine is the only one that touches the file, and it holds no lock while
// it does (see the top of this file): mu covers the backlog and the flags,
// for memory work only. The path is the writer's alone until done closes,
// which publishes it.
//
// The file is offered to the model only when it is whole — all of the output,
// or its first cap bytes — and was finished in time. One that is not is left
// where it is for tool.Sweep, unnamed in the result, never removed: a stalled
// remove would be one more filesystem call to wait on, and the name may by
// then belong to another file. A writer stalled on the filesystem leaks until
// the filesystem answers, with the one chunk it was writing, at most
// spillBacklog; nothing waits on it longer than spillWait, and its backlog is
// dropped the moment it is abandoned, on every path (abandon).
type spiller struct {
	wake chan struct{} // one token: there is something new in the backlog
	done chan struct{} // closed when the writer has finished
	path string        // the whole file, set by the writer before done closes; "" when there is none

	mu        sync.Mutex // never held across a syscall, or anything that waits
	backlog   []byte     // output not yet taken by the writer
	ended     bool       // no more output is coming
	abandoned bool       // write no more of the file, and offer it to nobody
}

func startSpiller(open spillOpener, home, id string) *spiller {
	s := &spiller{wake: make(chan struct{}, 1), done: make(chan struct{})}
	go s.run(open, home, id)
	return s
}

// run is the writer: open the file, write the backlog as it comes until the
// output has ended or the file is abandoned, close the file, and publish its
// path when it is whole. Every filesystem call here is made with no lock held.
func (s *spiller) run(open spillOpener, home, id string) {
	defer close(s.done)
	f, err := open(home, id)
	if err != nil {
		s.abandon()
		return
	}
	whole := false
	for {
		b, ended, abandoned := s.take()
		if abandoned {
			break
		}
		if len(b) > 0 {
			if _, err := f.Write(b); err != nil {
				s.abandon()
				break
			}
		}
		if ended {
			whole = true
			break
		}
		<-s.wake
	}
	if err := f.Close(); err == nil && whole {
		s.path = f.Name()
	}
}

// take empties the backlog and says whether the output has ended — so what
// it took is the last of it — or the file has been abandoned.
func (s *spiller) take() (b []byte, ended, abandoned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, s.backlog = s.backlog, nil
	return b, s.ended, s.abandoned
}

// poke wakes the writer. It is called under mu.
func (s *spiller) poke() {
	select {
	case s.wake <- struct{}{}:
	default: // a token is waiting already
	}
}

// send adds b to the backlog without waiting, and reports whether it did:
// not when the writer is spillBacklog behind, or has failed.
func (s *spiller) send(b []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.abandoned || s.ended || len(s.backlog)+len(b) > spillBacklog {
		return false
	}
	s.backlog = append(s.backlog, b...)
	s.poke()
	return true
}

// end tells the writer the output has ended: the file is complete once the
// backlog is written.
func (s *spiller) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
	s.poke()
}

// abandon gives the file up, from either side — the writer's, when it could
// not be opened or written; output's, when the writer fell too far behind or
// was not finished in time — and drops the backlog at once, so nothing more
// is held for a writer that may never come back. The writer, if it still
// runs, stops at its next take. Nothing is removed (see spiller).
func (s *spiller) abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abandoned, s.ended, s.backlog = true, true, nil
	s.poke()
}

// wait waits at most d, once the output has ended, for the writer to finish
// — and no longer once closing, the session's close signal, closes — and
// returns the whole file's path, or "" when there is none: it could not be
// opened or written, it was abandoned, or it was not finished in time. One
// not finished in time is abandoned then: its backlog is dropped, and the
// result will have said the output was not saved. The path is read only once
// done has closed, which is what publishes it.
func (s *spiller) wait(d time.Duration, closing <-chan struct{}) string {
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
		case <-closing:
		}
	}
	select {
	case <-s.done:
		return s.path
	default:
		s.abandon()
		return ""
	}
}

// tailText is opencode's tail (shell.ts:225-255): text itself when it has at
// most maxLines lines and maxBytes bytes; otherwise the most whole lines
// from its end that fit in both or, when even its last line does not fit,
// that line's last maxBytes bytes, from the first character boundary. cut
// says anything was left out.
func tailText(text string, maxLines, maxBytes int) (kept string, cut bool) {
	if strings.Count(text, "\n") < maxLines && len(text) <= maxBytes {
		return text, false
	}
	lines, size := 0, 0
	rest := text
	for lines < maxLines {
		i := strings.LastIndexByte(rest, '\n')
		line := rest[i+1:]
		n := len(line)
		if lines > 0 {
			n++ // the newline joining it to the lines kept after it
		}
		if size+n > maxBytes {
			if lines == 0 {
				start := len(line) - maxBytes
				for start < len(line) && !utf8.RuneStart(line[start]) {
					start++
				}
				return line[start:], true
			}
			break
		}
		lines, size = lines+1, size+n
		if i < 0 {
			break
		}
		rest = rest[:i]
	}
	return text[len(text)-size:], true
}

// report sends progress snapshots — the output's tail — at most every
// progressInterval, and only when the output has changed, from a goroutine
// of its own, so a consumer that is slow or blocked never holds up the
// command or the copy of its output (plan 019 §3.5). The func it returns
// stops it: no snapshot starts once that has returned, and it waits for the
// goroutine unless a snapshot is being delivered — a blocked consumer must
// not hold up Run either — in which case that one delivery may end after
// Run has returned, as Progress allows. A nil progress sends nothing.
func (o *output) report(progress tool.Progress) (stop func()) {
	if progress == nil {
		return func() {}
	}
	var (
		mu            sync.Mutex
		stopped, busy bool
	)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(progressInterval)
		defer tick.Stop()
		sent := 0
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
			}
			snap, writes := o.snapshot()
			if writes == sent {
				continue
			}
			mu.Lock()
			if stopped {
				mu.Unlock()
				return
			}
			busy = true
			mu.Unlock()
			deliver(progress, snap)
			mu.Lock()
			busy = false
			mu.Unlock()
			sent = writes
		}
	}()
	return func() {
		mu.Lock()
		stopped = true
		wait := !busy
		mu.Unlock()
		close(quit)
		if wait {
			<-done
		}
	}
}

// deliver hands one snapshot to progress. A consumer that panics loses that
// snapshot, which the contract allows; nothing else could recover a panic
// on this goroutine.
func deliver(progress tool.Progress, snapshot string) {
	defer func() { _ = recover() }()
	progress(snapshot)
}
