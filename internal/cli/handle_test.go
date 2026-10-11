package cli

import (
	"regexp"
	"strings"
	"testing"
)

// TestHandleRoundTrips: every target kind formats to a shell-safe handle and
// parses back to itself — a craze turn by its id (with the row it came from
// beside it, when the handle has one), a row by "row-" and its id, a foreign
// turn's id, whatever its shape, as base64url.
func TestHandleRoundTrips(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	inc := "0199c0de-1111-7000-8000-00000000a001"
	for _, c := range []struct {
		target waitTarget
		row    string
		want   string
	}{
		{waitTarget{kind: targetTurn, id: "turn-3"}, "", inc + ".41.turn-3"},
		{waitTarget{kind: targetTurn, id: "turn-3"}, "q-2", inc + ".41.turn-3.row-q-2"},
		{waitTarget{kind: targetTurn, id: "turn-3"}, "q.2_x-y", inc + ".41.turn-3.row-q.2_x-y"},
		{waitTarget{kind: targetRow, id: "q-2"}, "", inc + ".41.row-q-2"},
		{waitTarget{kind: targetForeign, id: "interject fallback/1 $(rm -rf)"}, "", ""},
	} {
		h := turnHandle{incarnation: inc, seq: 41, target: c.target, row: c.row}
		s := h.String()
		if c.want != "" && s != c.want {
			t.Errorf("%v: %q, want %q", c.target, s, c.want)
		}
		if !safe.MatchString(s) {
			t.Errorf("%v: %q is not shell-safe", c.target, s)
		}
		back, err := parseHandle(s)
		if err != nil || back != h {
			t.Errorf("%q parsed back as %+v, %v; want %+v", s, back, err, h)
		}
	}
	if s := (turnHandle{incarnation: inc}).String(); s != "" {
		t.Errorf("a handle with no target is %q, want none", s)
	}
}

// TestParseHandleRefusesWhatIsNotOne: every malformed handle is refused,
// and the refusal never quotes the text (a pasted key may land in --from).
func TestParseHandleRefusesWhatIsNotOne(t *testing.T) {
	for _, s := range []string{
		"", "abc", "inc.1", ".1.turn-1", "inc..turn-1", "inc.x.turn-1", "inc.01.turn-1", "inc.-1.turn-1",
		"inc.1.", "inc.1.turn-", "inc.1.row-", "inc.1.bogus-1", "inc.1.foreign-", "inc.1.foreign-!!",
		"in c.1.turn-1", "inc;rm.1.turn-1", "inc.1.turn-1;rm", "inc.1.row-q 2", "sk-secret.1.nope",
		"inc.1.turn-1.", "inc.1.turn-1.row-", "inc.1.turn-1.q-2", "inc.1.turn-1.row-q 2", "inc.1.turn-1_2", "inc.1.turn-.row-q-1",
	} {
		h, err := parseHandle(s)
		if err == nil {
			t.Errorf("%q parsed as %+v", s, h)
			continue
		}
		if s != "" && strings.Contains(err.Error(), s) {
			t.Errorf("the refusal of %q quotes it: %v", s, err)
		}
	}
}
