// ndjson.go implements the one-request-per-dial newline-delimited JSON
// protocol both herdr and roost speak over a UDS control socket (plan 015
// §3.2, §3.3, §3.4): dial, write one line, read replies until the caller is
// satisfied, close. No connection outlives one request.
package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// maxReplyLine bounds one reply line: roost's own cap (plan 015 §3.7), reused
// here so herdr and roost share one limit.
const maxReplyLine = 16 << 20

// errFrameTooLarge is wrapped by readBoundedLine's error so a caller (and a
// test) can identify the size-cap rejection specifically, rather than any
// other read failure (a closed connection, a ctx deadline, ...).
var errFrameTooLarge = errors.New("ndjson: reply frame exceeds the size cap")

// roundTrip dials socket, writes req as one JSON line, then feeds reply lines
// to accept until it reports done or an error. It is used by herdr directly
// and will be used by roost (Commit 4), which skips "event" frames and
// mismatched ids by asking accept for another line.
//
// The connection is bound to ctx: SetDeadline is set from ctx's deadline, if
// any, and ctx.Done additionally closes the connection so a peer that accepts
// and never replies cannot hold the caller past ctx (plan 015 §3.2). Every
// error path checks ctx.Err() last, so a failure caused by ctx expiring wraps
// ctx.Err() rather than the raw I/O error, which a caller matches with
// errors.Is(err, context.DeadlineExceeded) or context.Canceled.
func roundTrip(ctx context.Context, socket string, req any, accept func(line []byte) (done bool, err error)) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return wrapCtx(ctx, false, fmt.Errorf("dial: %w", err))
	}
	defer conn.Close()

	hadDeadline := false
	if dl, ok := ctx.Deadline(); ok {
		hadDeadline = true
		if err := conn.SetDeadline(dl); err != nil {
			return wrapCtx(ctx, hadDeadline, fmt.Errorf("set deadline: %w", err))
		}
	}
	// A ctx cancelled with no deadline (e.g. the Hub's base context on Close)
	// still must not let a non-replying peer hold the connection open, so
	// cancellation itself closes the conn.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ndjson: marshal request: %w", err)
	}
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		return wrapCtx(ctx, hadDeadline, fmt.Errorf("write: %w", err))
	}

	r := bufio.NewReader(conn)
	for {
		reply, err := readBoundedLine(r, maxReplyLine)
		if err != nil {
			return wrapCtx(ctx, hadDeadline, fmt.Errorf("read: %w", err))
		}
		done, err := accept(reply)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// wrapCtx returns err, replacing it with one wrapping ctx.Err() (or, failing
// that, context.DeadlineExceeded) whenever ctx caused the failure: the
// caller cares that its deadline or cancellation is why the call failed, not
// the resulting I/O error text (a closed pipe, a reset connection, ...).
//
// ctx.Err() alone is not reliable here: conn.SetDeadline(dl) and ctx's own
// internal timer are two independent timers set to the same instant, and the
// socket read can return its "i/o timeout" error a hair before ctx's timer
// goroutine has flipped ctx.Done(). So a hadDeadline call additionally
// treats any net.Error with Timeout() true as ctx expiring — there is no
// other source of a timeout error on a connection whose deadline came from
// ctx.
func wrapCtx(ctx context.Context, hadDeadline bool, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("ndjson: %w: %w", cerr, err)
	}
	if hadDeadline && isTimeout(err) {
		return fmt.Errorf("ndjson: %w: %w", context.DeadlineExceeded, err)
	}
	return fmt.Errorf("ndjson: %w", err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readBoundedLine reads one '\n'-terminated line from r, accumulating
// bufio.Reader's ReadSlice chunks. It fails as soon as the accumulated frame
// exceeds max, without reading (or buffering) the rest of an oversized
// frame. EOF reached before a '\n' is an error: a reply that closes the
// connection without a trailing newline is not a complete line.
func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		// ReadSlice's slice is only valid until the next read, so it must be
		// copied out; append does that when line is grown from nil.
		line = append(line, chunk...)
		if len(line) > max {
			return nil, fmt.Errorf("%w: %d bytes read, cap %d", errFrameTooLarge, len(line), max)
		}
		switch err {
		case nil:
			return line, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			return nil, fmt.Errorf("connection closed before newline: %w", io.ErrUnexpectedEOF)
		default:
			return nil, err
		}
	}
}
