package main

import (
	"encoding/json"
	"os"

	"github.com/charliek/craze/internal/acp"
)

// Two knobs for the start settings' ordering (craze plan 032 §3.11, P6): a
// session's --effort and --fast are set inside its Start, so no prompt can
// reach the agent ahead of them. A test proves it with a schedule it forces:
// CRAZE_FAKE_SET_GATE holds every session/set_config_option's answer until the
// test releases it — while the read loop goes on reading, so a prompt craze
// wrote meanwhile would be read, and recorded, ahead of the set's answer — and
// CRAZE_FAKE_DUMP_CALLS is the record: every request and notification the
// fake reads, in the order it read them, which is the order craze wrote them.

// dumpCall appends msg to CRAZE_FAKE_DUMP_CALLS, one line per message read:
// its method, and for session/set_config_option a space and "<configId>=
// <value>". It runs on the read loop, ahead of the message's handler.
func dumpCall(msg *acp.Message) {
	p := os.Getenv("CRAZE_FAKE_DUMP_CALLS")
	if p == "" {
		return
	}
	line := msg.Method
	if msg.Method == acp.MethodSessionSetConfig {
		var params acp.SetConfigParams
		_ = json.Unmarshal(msg.Params, &params)
		line += " " + params.ConfigID + "=" + params.Value
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}

// withCalls is handler with every message it is given recorded first
// (dumpCall).
func withCalls(handler func(*acp.Message)) func(*acp.Message) {
	return func(msg *acp.Message) {
		dumpCall(msg)
		handler(msg)
	}
}

// setGated runs handle for a session/set_config_option: at once, on the read
// loop, or — with CRAZE_FAKE_SET_GATE naming a FIFO — on a goroutine of its
// own once one byte has been read from that FIFO, the read loop reading on
// meanwhile. One byte answers one set. A set of the option
// CRAZE_FAKE_SET_REFUSE names is refused instead, when its turn comes: -32602,
// as cursor refuses a set it will not take.
func (s *server) setGated(msg *acp.Message, handle func(*acp.Message)) {
	run := func() {
		var p acp.SetConfigParams
		_ = json.Unmarshal(msg.Params, &p)
		if refuse := os.Getenv("CRAZE_FAKE_SET_REFUSE"); refuse != "" && refuse == p.ConfigID {
			_ = s.conn.ReplyErr(msg.ID, pmInvalidParams("Refused config option: "+p.ConfigID))
			return
		}
		handle(msg)
	}
	gate := os.Getenv("CRAZE_FAKE_SET_GATE")
	if gate == "" {
		run()
		return
	}
	go func() {
		awaitSetGate(gate)
		run()
	}()
}

// awaitSetGate is one set's release: a byte read from the FIFO at path.
//
// The FIFO is opened for reading and writing, so the wait holds a write end of
// its own until its byte (plan 035 C3). Its read never ends on an end of file:
// a set whose gate opens while the test's writer for the set before it is
// still open would otherwise read that writer's close as its own release, and
// answer before the test let it go (plan 032 X69: seen on macOS CI). Nor does
// the pipe run out of ends while the set waits: X69's gate opened it afresh
// after such an end of file, and the test's next writer could open, write and
// close before the old open was closed — whose close then dropped the byte
// with the pipe, as gate's doc tells for CRAZE_FAKE_GATE, and held the set for
// ever. A FIFO that will not open read-write falls back to that reopening
// read.
func awaitSetGate(path string) {
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		defer f.Close()
		var b [1]byte
		_, _ = f.Read(b[:])
		return
	}
	for {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		var b [1]byte
		n, _ := f.Read(b[:])
		_ = f.Close()
		if n == 1 {
			return
		}
	}
}
