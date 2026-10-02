package hostspawn

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestParseReadyLine is the spawner's reading of the line itself: one line of
// at most ReadyLineMax bytes, its newline included, decoded; EOF before a byte
// is an exit, within a line a malformed one; a line past the bound is
// oversized however it ends. And what makes a line a ready line (astra r5-c3
// 4): ok needs its host id in a host id's form, an absolute socket, a craze
// session id and a craze version, and no refusal — no reason, no holder, not
// refused; not ok needs a reason, and its holder, when it names one, the
// session and a host id — or, a lock that names nobody yet, no host and no
// pid. A member the spawner does not know is ignored on either branch.
func TestParseReadyLine(t *testing.T) {
	ok := `{"ok":true,"hostId":"0123456789ab","socket":"/s","crazeSessionId":"c","crazeVersion":"v","future":1}`
	okWithout := func(member, value string) string {
		m := map[string]any{"ok": true, "hostId": "0123456789ab", "socket": "/s", "crazeSessionId": "c", "crazeVersion": "v"}
		if value == "" {
			delete(m, member)
		} else {
			m[member] = json.RawMessage(value)
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	notOK := func(held string) string {
		return `{"ok":false,"error":"e","held":` + held + `}` + "\n"
	}
	pad := func(n int) string { return `{"ok":false,"error":"` + strings.Repeat("x", n) + `"}` }
	for _, tc := range []struct {
		name string
		in   string
		want Failure
	}{
		{"ok, an unknown member ignored", ok + "\n", 0},
		{"ok, and more after it", ok + "\nmore", 0},
		{"exactly the bound", pad(ReadyLineMax-len(pad(0))-1) + "\n", 0},
		{"one past the bound", pad(ReadyLineMax-len(pad(0))) + "\n", Oversized},
		{"no newline, past the bound", pad(ReadyLineMax), Oversized},
		{"nothing", "", Exited},
		{"no newline", ok, Malformed},
		{"not JSON", "hello\n", Malformed},
		{"not ok, no reason", `{"ok":false}` + "\n", Malformed},
		{"not ok, an unknown member ignored", `{"ok":false,"error":"e","future":{"x":1}}` + "\n", 0},

		{"ok, no host id", okWithout("hostId", ""), Malformed},
		{"ok, a host id not in a host id's form", okWithout("hostId", `"0123456789AB"`), Malformed},
		{"ok, no socket", okWithout("socket", ""), Malformed},
		{"ok, a relative socket", okWithout("socket", `"s"`), Malformed},
		{"ok, no session", okWithout("crazeSessionId", ""), Malformed},
		{"ok, a session that is no token", okWithout("crazeSessionId", `"a/b"`), Malformed},
		{"ok, no craze version", okWithout("crazeVersion", ""), Malformed},
		{"ok, and a refusal", okWithout("error", `"e"`), Malformed},
		{"ok, and a holder", okWithout("held", `{"hostId":"0123456789ab","crazeSessionId":"c"}`), Malformed},
		{"ok, and refused", okWithout("refused", `true`), Malformed},
		{"not ok, the choice refused", `{"ok":false,"error":"e","refused":true}` + "\n", 0},

		{"held", notOK(`{"hostId":"0123456789ab","pid":12,"crazeSessionId":"c"}`), 0},
		{"held by a lock that names nobody yet", notOK(`{"hostId":"","crazeSessionId":"c"}`), 0},
		{"held, an unknown member ignored", notOK(`{"hostId":"0123456789ab","crazeSessionId":"c","future":1}`), 0},
		{"held, no reason", `{"ok":false,"held":{"hostId":"0123456789ab","crazeSessionId":"c"}}` + "\n", Malformed},
		{"held by nobody, no reason", `{"ok":false,"held":{}}` + "\n", Malformed},
		{"held by nobody", notOK(`{}`), Malformed},
		{"held, no session", notOK(`{"hostId":"0123456789ab","pid":12}`), Malformed},
		{"held, a session that is no token", notOK(`{"hostId":"0123456789ab","crazeSessionId":".."}`), Malformed},
		{"held, a host id not in a host id's form", notOK(`{"hostId":"h","crazeSessionId":"c"}`), Malformed},
		{"held, a pid and no host", notOK(`{"hostId":"","pid":12,"crazeSessionId":"c"}`), Malformed},
		{"held, a negative pid", notOK(`{"hostId":"0123456789ab","pid":-1,"crazeSessionId":"c"}`), Malformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, why := ParseReady(strings.NewReader(tc.in))
			if got != tc.want {
				t.Fatalf("failure %d (%s), want %d", got, why, tc.want)
			}
		})
	}
}

// TestReadReadyPrefersALineAlreadyRead (plan 032 r43 3): with the ready line
// read and the caller's context ended both before ReadReady waits — either
// the select could take — the line is the answer, every time: a host that
// has announced itself is never taken for one cut before it did.
func TestReadReadyPrefersALineAlreadyRead(t *testing.T) {
	prevRead, prevSelecting := readyRead, readySelecting
	t.Cleanup(func() { readyRead, readySelecting = prevRead, prevSelecting })
	line := `{"ok":true,"hostId":"0123456789ab","socket":"/run/h.sock","crazeSessionId":"s-1","crazeVersion":"1.0.0"}` + "\n"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 64 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString(line); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		read := make(chan struct{})
		readyRead = func() { close(read) }
		readySelecting = func() { <-read }
		got, failure, why := ReadReady(ctx, r)
		_ = r.Close()
		if failure != 0 || !got.OK || got.HostID != "0123456789ab" {
			t.Fatalf("attempt %d: ReadReady = %+v, failure %d (%s); want the line read", i, got, failure, why)
		}
	}
}
