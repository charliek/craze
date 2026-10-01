package tui

import (
	"time"

	"github.com/charliek/craze/internal/harness/tool/attach"
	"github.com/charliek/craze/internal/paths"
)

// sweepAttachments runs the attachments directory's sweep (plan 033 §3.2,
// P29): files older than seven days go, then the oldest while the directory
// is over 500 MiB (attach.Sweep). The TUI is the one that writes the
// directory, so it is the one that tidies it, once as it starts
// (sweepAttachmentsAtStart).
//
// Best effort: its error, if any, is dropped. A failed sweep costs disk, not
// correctness — a file it should have removed is one the host can still read,
// and one it did remove is "no longer available" at send time, which the host
// already handles (plan 033 §3.2). Nothing is ever shown for it.
//
// dir is paths.AttachmentsDir(); "" — no craze directory — sweeps nothing.
func sweepAttachments(dir string, now time.Time) {
	if dir == "" {
		return
	}
	_, _ = attach.Sweep(dir, now)
}

// sweepAttachmentsAtStart is Run's call — every TUI's start, a new session's,
// --continue's and craze attach's alike (plan 033 X12): the real directory
// and the real time, on a goroutine of its own, so the sweep neither holds up
// the first frame nor runs on the UI goroutine.
func sweepAttachmentsAtStart() {
	go sweepAttachments(paths.AttachmentsDir(), time.Now())
}
