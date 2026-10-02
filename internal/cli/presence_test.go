package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/rundir"
)

// Presence on the two craze hosts (plan 032 §3.14, R2-7, R3-1): every host
// counts its attached clients for them, and the TUI-hosted one counts its own
// TUI among them and keeps the count where that TUI reads it
// (tui.Config.LocalPresence).

// attachedRow is the raw connection's sessions.list row's capabilities.presence
// and attached (0 when absent), request id id.
func attachedRow(t *testing.T, c *rawConn, id string) (bool, float64) {
	t.Helper()
	c.send(t, `{"jsonrpc":"2.0","id":"`+id+`","method":"sessions.list","params":{}}`)
	res := c.reply(t, id)
	rows, _ := res["sessions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("sessions.list answered %v", res)
	}
	row, _ := rows[0].(map[string]any)
	caps, _ := row["capabilities"].(map[string]any)
	presence, _ := caps["presence"].(bool)
	n, _ := row["attached"].(float64)
	return presence, n
}

// nextCount is the next count the host TUI's channel gives, within a step.
func nextCount(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case n, ok := <-ch:
		if !ok {
			t.Fatal("the host TUI's count channel closed")
		}
		return n
	case <-time.After(serveStep):
		t.Fatalf("the host TUI heard no count in %s", serveStep)
		return 0
	}
}

// TestTheTUIHostedServerCountsItsTUI: one client attached to a TUI-hosted
// session is counted with the host TUI — the host TUI hears 2, the client's
// row says attached: 2 — and its leaving brings the host TUI's count back to
// 1; the host's teardown closes the channel. The negative control: a detached
// host (craze serve's bind) counts the same client alone, keeps no channel,
// and still says presence.
func TestTheTUIHostedServerCountsItsTUI(t *testing.T) {
	env := serveEnv(t)
	rh, hostID := servingHost(t, env, io.Discard)
	counts := rh.ctl.localPresence()
	if counts == nil {
		t.Fatal("the TUI-hosted server keeps no count for its TUI")
	}
	eng := grokStubEngine(t)
	rh.onEngine(eng)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := waitEntry(t, entryPath(env, hostID), func(e rundir.Entry) bool { return e.Ready })
	c := dialRaw(t, entry.Socket)
	c.send(t, `{"jsonrpc":"2.0","id":"1","method":"hello","params":{"protocols":[1],"client":{"kind":"test","name":"presence"}}}`)
	c.reply(t, "1")
	c.send(t, `{"jsonrpc":"2.0","id":"2","method":"session.attach","params":{"sessionId":"`+eng.State().CrazeSessionID+`"}}`)
	c.reply(t, "2")
	if n := nextCount(t, counts); n != 2 {
		t.Fatalf("the host TUI heard %d, want 2: itself and the client", n)
	}
	if presence, n := attachedRow(t, c, "3"); !presence || n != 2 {
		t.Fatalf("the client's row: presence %v, attached %v; want true and 2", presence, n)
	}
	_ = c.c.Close()
	if n := nextCount(t, counts); n != 1 {
		t.Fatalf("the host TUI heard %d once the client left, want 1", n)
	}
	rh.close()
	if _, ok := <-counts; ok {
		t.Fatal("the count channel is still open after the host's teardown")
	}

	denv := serveEnv(t)
	dID := rundir.NewHostID()
	d, err := bindControl(denv, dID, "/ws", true, hostRequest{}, func(control.StopRequest) {}, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.close)
	if d.localPresence() != nil {
		t.Fatal("a detached host keeps a count for a TUI it does not have")
	}
	deng := grokStubEngine(t)
	d.server.SetEngine(deng)
	if err := deng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	dc := dialRaw(t, d.host.Socket())
	dc.send(t, `{"jsonrpc":"2.0","id":"1","method":"hello","params":{"protocols":[1],"client":{"kind":"test","name":"presence"}}}`)
	dc.reply(t, "1")
	dc.send(t, `{"jsonrpc":"2.0","id":"2","method":"session.attach","params":{"sessionId":"`+deng.State().CrazeSessionID+`"}}`)
	dc.reply(t, "2")
	if presence, n := attachedRow(t, dc, "3"); !presence || n != 1 {
		t.Fatalf("the detached host's row: presence %v, attached %v; want true and 1", presence, n)
	}
}
