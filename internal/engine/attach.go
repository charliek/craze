package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// In-process attach (plan 024 §3.6): a snapshot of the engine's transcript
// model and a subscription that carries on from exactly where it was cut, or a
// subscription that carries on from the client's own cursor. It is
// session.attach (session control 05) without the socket: S2 wraps Attachment
// in its notifications and adds the connection's own barrier.
//
// # The single cutoff
//
// The model is folded by the log's observer, inside the publishing boundary,
// once per committed event and in Seq order, so it is always a complete folded
// prefix of the committed sequence, equal to it once the observer returns
// (engine.go's Engine.model). A snapshot at Seq M is therefore exact at M, and
// a subscription after {incarnation, M} replays (M, N] from the ring and goes
// live at N+1: nothing is done under the boundary for a snapshot, and nothing
// is missed or folded twice.
//
// # The rule the order rests on
//
// The model's mutex is released before Subscribe is called, and Attach holds
// no lock of its own when it enters Subscribe. Subscribe waits for the
// boundary, and the boundary's holder folds — takes the model's mutex — before
// it lets go; an Attach that held the mutex across that wait would wait for a
// publisher waiting for it. Model.Snapshot returns a value and has released
// the mutex by then, which is the whole of the rule here, and
// TestAttachWhileAPublisherHoldsTheBoundary is the schedule that would
// deadlock without it.

// ErrAttachRaced is Attach giving up: every fresh snapshot's cursor was
// refused, the first and attachRetries more. A snapshot's cursor names an event
// the model has folded, so the log can refuse it only because the ring moved
// past it between the cut and the subscription — by count or by bytes, a burst
// of commits (a --continue replay, a run of large tool reports) outpacing the
// attach — or because the backlog since the cut outgrew the subscription's
// MaxBytes. Nothing was registered. Its code is "unavailable", the same answer
// ErrUnavailable's backed-up log gets: nothing happened, and the attach may
// simply be asked for again.
var ErrAttachRaced = errors.New("engine: attach raced the event log: it moved past every fresh snapshot")

// attachRetries is how many fresh snapshots Attach takes after the first one's
// cursor is refused, before ErrAttachRaced (plan 024 §3.6).
const attachRetries = 3

// AttachOptions shape an attachment.
type AttachOptions struct {
	// Cursor is where the client already is. nil asks for a snapshot. A cursor
	// the log cannot resume from synchronously — another incarnation, a future
	// seq, a backlog over MaxBytes, a head that has left the ring with no
	// journal to serve it, a gap the journal recorded — is answered with a
	// snapshot instead, and Attachment.Reset says why.
	Cursor *agent.Cursor
	// MaxItems and MaxBytes are the subscription's budget
	// (agent.SubscribeOptions): 0 is the log's default, 1,024 records and
	// 8 MiB. A subscriber that falls further behind is ended with
	// agent.ErrSlowConsumer, and re-attaches.
	MaxItems, MaxBytes int
	// SnapshotBytes is the snapshot's budget on its encoding
	// (transcript.Model.Snapshot): 0 is transcript.DefaultSnapshotBytes, 4 MiB.
	SnapshotBytes int
}

// Attachment is a client's way in: the model as it stood at a cut, and the
// events from the one after it.
type Attachment struct {
	// Snapshot is the model at its Seq, nil when the cursor was honoured. The
	// client Restores it and folds every record Sub delivers after it.
	Snapshot *transcript.Snapshot
	// Reset is why the cursor was not honoured — the first refusal's reason —
	// and "" when it was, or when none was given. A snapshot cut for a later
	// attempt after the ring moved does not change it: the client asked for a
	// cursor, and this is what became of that cursor.
	Reset agent.CursorReason
	// Sub delivers every record after Snapshot.Seq, or after Cursor.Seq when the
	// cursor was honoured, in order and with nothing skipped. What the client
	// owes it (plan 024 §3.6):
	//
	//   - a record whose Omitted is set is one no client can fold (an event
	//     over MaxRecordBytes, which the engine's model folded whole): the client
	//     discards what it folded, closes Sub and attaches again with NO cursor —
	//     resuming from its own would be handed the same omission. The fresh
	//     snapshot is at or beyond the omitted record once the commit that
	//     offered it has finished; a client that reached for it first, before
	//     that commit's fold, is handed the omission once more by a Sub whose
	//     cutoff waited for that commit, so the next attach is past it;
	//   - Records closing with Err non-nil — agent.ErrSlowConsumer, or an
	//     agent.ErrCursorUnresolvable from the journal file's leg of a replay,
	//     which may come after a prefix was delivered — voids everything folded
	//     since the snapshot (since the cursor, prefix included): the client
	//     discards it and attaches again with no cursor.
	Sub *agent.Subscription
}

// Attach attaches a client (plan 024 §3.6). With a cursor it subscribes from
// the cursor, and a cursor honoured is the whole answer. Otherwise — no cursor,
// or one refused synchronously for any reason — it cuts a snapshot of the
// engine's model (under the model's own mutex, never the boundary) and
// subscribes after the snapshot's Seq. That cursor names an event the model has
// folded, so any synchronous refusal of it is the log having moved since the cut,
// and it is retried with a fresh snapshot, attachRetries times, before
// ErrAttachRaced.
//
// Every other error Subscribe returns is returned at once, as it is:
// agent.ErrClosed for a log that has closed, and ctx's error for an attach
// abandoned while it waited for the boundary — neither is a race a fresh
// snapshot could win. A snapshot the budget cannot hold
// (transcript.ErrSnapshotTooLarge: the mandatory state alone, or it and the main
// transcript's newest entry, over SnapshotBytes) fails the attach with that
// error, wrapped; nothing is dropped to make it fit.
//
// It blocks: Subscribe waits for the publishing boundary, which a publisher
// holds while it waits for room in the primary. So it belongs on a goroutine
// that is not the primary's reader (Control's contract), and ctx bounds every
// wait it makes — the boundary through agent.SubscribeOptions.Ctx, and the
// attempts between. A cancelled attach registers nothing and leaves no goroutine
// behind. The model's mutex is released before Subscribe is called, and Attach
// holds no lock of its own when it enters it (the rule above).
func (e *Engine) Attach(ctx context.Context, o AttachOptions) (*Attachment, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	subscribe := func(after agent.Cursor) (*agent.Subscription, error) {
		return e.log.Subscribe(agent.SubscribeOptions{
			After: &after, MaxItems: o.MaxItems, MaxBytes: o.MaxBytes, Ctx: ctx,
		})
	}
	var reset agent.CursorReason
	if o.Cursor != nil {
		sub, err := subscribe(*o.Cursor)
		if err == nil {
			return &Attachment{Sub: sub}, nil
		}
		var refused agent.ErrCursorUnresolvable
		if !errors.As(err, &refused) {
			return nil, err
		}
		reset = refused.Reason
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// The model's mutex is taken and released inside this call: what comes
		// back is a value, and nothing of the model's is held from here on.
		snap, err := e.model.Snapshot(o.SnapshotBytes)
		if err != nil {
			return nil, fmt.Errorf("engine: attach: %w", err)
		}
		if h := e.hooks; h != nil && h.attachSnapshotted != nil {
			h.attachSnapshotted(snap.Seq, attempt)
		}
		sub, err := subscribe(agent.Cursor{Incarnation: snap.Incarnation, Seq: snap.Seq})
		if err == nil {
			return &Attachment{Snapshot: snap, Reset: reset, Sub: sub}, nil
		}
		var refused agent.ErrCursorUnresolvable
		if !errors.As(err, &refused) {
			return nil, err
		}
		if attempt == attachRetries {
			return nil, fmt.Errorf("%w: %d snapshots refused, the last at seq %d (%s)",
				ErrAttachRaced, attempt+1, snap.Seq, refused.Reason)
		}
	}
}
