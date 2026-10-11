package cli

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// A resume handle (plan 041 §3.1, P4) names one wait's place: the attach
// incarnation, a seq of that incarnation's stream, and the target the wait
// follows — `<incarnation>.<seq>.<target>`, printable and shell-safe (only
// letters, digits, '.', '_' and '-'), so a script can paste it into `craze
// wait <id> --from <handle>` unquoted. Its seq is the cursor just before the
// target's first event this wait relies on — the queue row's `sent`, or the
// turn's `started` — so a `wait --from` that is replayed from it folds the
// whole turn again rather than the part after the last event the first wait
// saw; a target first met mid-turn (plain `wait`) has the attach's own cutoff.
// A handle is ownership: `wait --from` cancels its target on a signal (§3.1
// "Signals"), plain `wait` never does.
//
// The target is one of:
//
//   - `turn-<n>` — a turn of craze's own: its id, "turn-N", as it is;
//   - `turn-<n>.row-<id>` — a turn of craze's own that came from the queued
//     row <id>, met mid-way (a reconcile's snapshot), so its seq is past the
//     row's link to it: the row is named beside the turn, and a wait from
//     the handle takes it for its own — should the agent's own turn refuse
//     the turn and put the row back (P3), the row's next run is the target;
//   - `row-<id>` — a queued row: "row-" and its id ("q-N");
//   - `foreign-<b64>` — a turn the agent ran on its own (plain `wait` only):
//     the agent's own id for it, base64url without padding, since its shape
//     is the agent's and may hold anything.
type turnHandle struct {
	incarnation string
	seq         uint64
	target      waitTarget
	// row is the queued row a turn target came from, "" for none: set only
	// beside a turn (`turn-<n>.row-<id>`).
	row string
}

// targetKind is what a wait follows.
type targetKind int

const (
	// targetNone: nothing yet — before a submit's reply, or plain wait's
	// look at an idle session.
	targetNone targetKind = iota
	// targetTurn is a turn of craze's own, by its id ("turn-N"), scoped to
	// its incarnation: turn ids restart at turn-1 with every incarnation
	// (P7), so a turn is only ever named with the incarnation beside it.
	targetTurn
	// targetRow is a queued row, by its id ("q-N").
	targetRow
	// targetForeign is a turn the agent ran on its own, by the bracket's id.
	targetForeign
)

// waitTarget is one thing a wait follows: a kind and its id.
type waitTarget struct {
	kind targetKind
	id   string
}

func (t waitTarget) String() string {
	switch t.kind {
	case targetTurn:
		return t.id
	case targetRow:
		return "row " + t.id
	case targetForeign:
		return "the agent's turn " + t.id
	}
	return "nothing"
}

// The target spellings' prefixes.
const (
	handleTurnPrefix    = "turn-"
	handleRowPrefix     = "row-"
	handleForeignPrefix = "foreign-"
)

// String is the handle as craze prints it, and parseHandle reads it back.
func (h turnHandle) String() string {
	var target string
	switch h.target.kind {
	case targetTurn:
		target = handleTurnPrefix + strings.TrimPrefix(h.target.id, handleTurnPrefix)
		if h.row != "" {
			target += "." + handleRowPrefix + h.row
		}
	case targetRow:
		target = handleRowPrefix + h.target.id
	case targetForeign:
		target = handleForeignPrefix + base64.RawURLEncoding.EncodeToString([]byte(h.target.id))
	default:
		return ""
	}
	return h.incarnation + "." + strconv.FormatUint(h.seq, 10) + "." + target
}

// parseHandle reads a handle (turnHandle.String): an incarnation of letters,
// digits and '-', a decimal seq, and a target whose id is shell-safe — a turn
// id of letters, digits and '-' (the '.' after it starts the row it came
// from, when there is one), a row id of letters, digits, '.', '_' and '-', or
// a foreign id's base64url.
// Anything else is an error naming what is wrong, never the text itself
// (args.go's rule: a pasted key may land here).
func parseHandle(s string) (turnHandle, error) {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return turnHandle{}, fmt.Errorf("a handle is <incarnation>.<seq>.<target>")
	}
	inc, seqText, target := parts[0], parts[1], parts[2]
	if inc == "" || !handleChars(inc, false) {
		return turnHandle{}, fmt.Errorf("the handle's incarnation is not one")
	}
	seq, err := strconv.ParseUint(seqText, 10, 64)
	if err != nil || strconv.FormatUint(seq, 10) != seqText {
		return turnHandle{}, fmt.Errorf("the handle's seq is not a decimal number")
	}
	h := turnHandle{incarnation: inc, seq: seq}
	switch {
	case strings.HasPrefix(target, handleTurnPrefix):
		rest, row, withRow := strings.Cut(strings.TrimPrefix(target, handleTurnPrefix), ".")
		if rest == "" || !handleChars(rest, false) {
			return turnHandle{}, fmt.Errorf("the handle's turn is not one")
		}
		h.target = waitTarget{kind: targetTurn, id: handleTurnPrefix + rest}
		if withRow {
			id, ok := strings.CutPrefix(row, handleRowPrefix)
			if !ok || id == "" || !handleChars(id, true) {
				return turnHandle{}, fmt.Errorf("the handle's row is not one")
			}
			h.row = id
		}
	case strings.HasPrefix(target, handleRowPrefix):
		rest := strings.TrimPrefix(target, handleRowPrefix)
		if rest == "" || !handleChars(rest, true) {
			return turnHandle{}, fmt.Errorf("the handle's row is not one")
		}
		h.target = waitTarget{kind: targetRow, id: rest}
	case strings.HasPrefix(target, handleForeignPrefix):
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(target, handleForeignPrefix))
		if err != nil || len(raw) == 0 {
			return turnHandle{}, fmt.Errorf("the handle's foreign turn is not one")
		}
		h.target = waitTarget{kind: targetForeign, id: string(raw)}
	default:
		return turnHandle{}, fmt.Errorf("the handle names no turn or row")
	}
	return h, nil
}

// handleChars reports whether s is all letters, digits, '-' and — with
// dots — '.' and '_': a handle's shell-safe alphabet.
func handleChars(s string, dots bool) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		case dots && (c == '.' || c == '_'):
		default:
			return false
		}
	}
	return true
}
