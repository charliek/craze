package transcript

// Bounds is what one model may retain (plan 024 §3.2 (b)). It is one set for
// every instance — the engine's and every client's — so two models folding the
// same events trim the same entries. It is the one knob: owner decision 3 may
// raise MainBytes to 32 MiB after V6.
type Bounds struct {
	// MainEntries and MainBytes bound the main transcript: the entry count, and
	// the retained bytes (Entry.Bytes summed). Trimming drops from the front,
	// never the last entry, after every append and every chunk. A tool update
	// in place never trims (today's rule), so the byte budget can be exceeded
	// by what such updates add — at most one tool payload per row updated in
	// place — until the next append or chunk enforces it again.
	MainEntries int
	MainBytes   int
	// SubEntries and SubBytes bound each child transcript the same way.
	SubEntries int
	SubBytes   int
	// StreamText caps a streamed entry's text (assistant, thought, a child's
	// user rows): the tail is kept, led by "…". User rows, notes and errors
	// are uncapped (today's rule).
	StreamText int
	// Agents is how many FINISHED roster rows are kept; past it the oldest
	// finish is evicted with its child transcript — the live session's rule
	// (internal/agent/subagents.go evictFinishedLocked). Running rows are never
	// evicted.
	Agents int
}

// DefaultBounds are the bounds every instance uses: the TUI's own caps
// (internal/tui/transcript.go at plan 024's baseline) plus the 8 MiB byte
// budget owner decision 3 gives the main transcript, and the live session's
// 32 finished roster rows.
func DefaultBounds() Bounds {
	return Bounds{
		MainEntries: 5000,
		MainBytes:   8 << 20,
		SubEntries:  1000,
		SubBytes:    1 << 20,
		StreamText:  64 << 10,
		Agents:      32,
	}
}

// minStreamText is the smallest stream cap that keeps a byte of text beside
// the "…" a capped tail starts with.
const minStreamText = len(ellipsis) + 1

// withDefaults fills every non-positive field from DefaultBounds, and raises a
// stream cap too small to hold its own ellipsis.
func (b Bounds) withDefaults() Bounds {
	d := DefaultBounds()
	if b.MainEntries <= 0 {
		b.MainEntries = d.MainEntries
	}
	if b.MainBytes <= 0 {
		b.MainBytes = d.MainBytes
	}
	if b.SubEntries <= 0 {
		b.SubEntries = d.SubEntries
	}
	if b.SubBytes <= 0 {
		b.SubBytes = d.SubBytes
	}
	if b.StreamText <= 0 {
		b.StreamText = d.StreamText
	}
	if b.StreamText < minStreamText {
		b.StreamText = minStreamText
	}
	if b.Agents <= 0 {
		b.Agents = d.Agents
	}
	return b
}
