package harness

import (
	"time"

	"github.com/charliek/craze/internal/harness/store"
)

// Event is one thing a running turn reports to Run's sink, as it happens:
// TextDelta, ThoughtDelta, StepDone or Retrying. The set is closed; the
// adapter switches on the concrete type.
type Event interface{ isEvent() }

// TextDelta is the next piece of the answer's text, as the provider streamed
// it. It is the model's raw output: the adapter sanitizes it before a
// terminal sees it (plan 018 §3.8).
type TextDelta struct{ Text string }

// ThoughtDelta is the next piece of the model's reasoning ("thinking"), raw
// like TextDelta.
type ThoughtDelta struct{ Text string }

// StepDone reports a finished model step and what it cost. It comes after
// the step's text and after the step was persisted. An H1 turn has one step.
type StepDone struct{ Usage Usage }

// Retrying reports that the step failed before producing any output and will
// be sent again after Delay (at most once a turn: the runner allows one
// retry). Nothing streamed before it belongs to the answer.
type Retrying struct{ Delay time.Duration }

func (TextDelta) isEvent()    {}
func (ThoughtDelta) isEvent() {}
func (StepDone) isEvent()     {}
func (Retrying) isEvent()     {}

// Usage is a step's or a turn's token counts: input, output, reasoning, and
// the prompt-cache reads and writes, which show whether a provider's prefix
// cache hit. It is the transcript's own shape (store.Usage), so what a turn
// reports and what it records cannot drift apart.
type Usage = store.Usage
