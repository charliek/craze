package tui

import (
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// What a command the composer ran tells the agent (plan 022 §3.6).
//
// A command is run so that somebody can ask about what it printed, so its
// output travels with the next thing they send: the block agent.ShellContextBlock
// renders, in front of the text, once, and then gone. It is not a conversation
// craze is having with the model — nothing is sent when a command finishes, and
// a session that ends without another message never sees any of it.
//
// The bounds below are what keeps "the next message" from becoming "every
// message, and all of them at once".

const (
	// shellCtxResults is how many finished commands one message may carry.
	// Three is a session's worth of "look, then look again, then ask" and
	// still small enough that the oldest of them is plausibly what the
	// question is about.
	shellCtxResults = 3
	// shellCtxBytes bounds the results together. It is one full command's
	// output (shellOutputCap), because that is the size the runner already
	// decided a single answer is worth; several commands share it rather than
	// each being given their own.
	shellCtxBytes = shellOutputCap
)

// keepShellResult records a finished command as context for the next message,
// and drops whatever no longer fits in front of it.
//
// A command that never started keeps nothing: there is no output to ask about,
// and the row already says why. One that was killed or timed out does keep what
// it printed — it ran, the user watched it run, and "what did it say before you
// stopped it" is a fair question. Its exit is the -1 the runner reports, which
// is the same thing the row draws as "killed".
//
// The oldest go first, and the newest is never dropped for size: it is the
// answer to what was just asked for, and the runner has already capped it at
// shellOutputCap. So the block is at most three results and at most
// shellCtxBytes of them, unless one result alone is larger, in which case it is
// the only one.
func (m *Model) keepShellResult(cmd string, res shellResult) {
	if res.start != nil {
		return
	}
	// A fresh slice rather than an append in place: every Update copies the
	// model, so growing the one the last copy handed over could write into
	// the array an older copy is still pointing at.
	kept := make([]agent.ShellResult, 0, len(m.shellCtx)+1)
	kept = append(kept, m.shellCtx...)
	kept = append(kept, agent.ShellResult{Command: cmd, Exit: res.exit, Output: res.out})
	for len(kept) > 1 && (len(kept) > shellCtxResults || shellCtxSize(kept) > shellCtxBytes) {
		kept = kept[1:]
	}
	m.shellCtx = kept
}

// shellCtxSize is what the results weigh: the bytes that came from the command
// and its output, which is everything the block's size depends on that is not
// the block's own fixed framing.
func shellCtxSize(results []agent.ShellResult) int {
	n := 0
	for _, r := range results {
		n += len(r.Command) + len(r.Output)
	}
	return n
}

// withShellContext is text on its way to the agent, with whatever the composer
// has run in front of it. Nothing pending is the text itself.
func (m Model) withShellContext(text string) string {
	return agent.ShellContextBlock(m.shellCtx) + text
}

// dropShellContext forgets the pending results. It is called where the text
// they were attached to was accepted, and where the commands they describe stop
// being part of the conversation the user is in: /clear, and a session change.
func (m *Model) dropShellContext() { m.shellCtx = nil }

// shellContextTaken reports that a submission really took the text handed to
// it, and with it the block in front of that text. A refusal — a queue that is
// full, a message the queue judges too long with the block counted in, a send
// that arrived before the session was ready — took neither, so the context
// stays where the draft does: with the user, for the next attempt.
func shellContextTaken(res engine.SubmitResult, err error) bool {
	return err == nil && (res.Turn != "" || res.Queued != nil || res.Armed)
}
