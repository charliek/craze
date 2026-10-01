package engine_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/paths"
)

// An attachment envelope over the socket, in each of session.prompt's modes
// (plan 033 §3.4, P7, A3b): a raw client can send the envelope with any mode,
// where the TUI sends it only with a prompt. Through a real host
// (internal/control), engine and live grok session over the fake agent:
//
//   - mode interject reaches the agent as [Image #1: <path>] text — the
//     session's Interject holds every client to "interjections are text";
//   - mode queue (a row behind a running turn) and mode send_now (armed over a
//     running turn) keep the envelope all the way to the host's read, and the
//     agent gets the image block behind block 1.
//
// What the agent read is the fake's own record (CRAZE_FAKE_DUMP_PROMPTS), in
// arrival order.

// dumpLine is one line of the fake's prompt record.
type dumpLine struct {
	Prompt []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		MimeType string `json:"mimeType"`
		Bytes    int    `json:"bytes"`
	} `json:"prompt"`
	Interject *string `json:"interject"`
}

func readDump(t *testing.T, path string) ([]dumpLine, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []dumpLine
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var l dumpLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("a dump line is not JSON: %v: %s", err, sc.Bytes())
		}
		out = append(out, l)
	}
	return out, string(raw)
}

// event is the reader's event at index i.
func (p *primaryReader) event(i int) agent.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evs[i]
}

func TestAnEnvelopeOverTheSocketInEachMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAZE_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	// Turn 1 (a plain prompt) and turn 2 (the queued row) hold long enough to
	// interject into and to send over; turn 3 (the send-now) runs through.
	t.Setenv("CRAZE_FAKE_STEP", "1500ms,1500ms,1ms")
	dump := filepath.Join(t.TempDir(), "prompts.jsonl")
	t.Setenv("CRAZE_FAKE_DUMP_PROMPTS", dump)

	var img bytes.Buffer
	if err := png.Encode(&img, image.NewNRGBA(image.Rect(0, 0, 32, 24))); err != nil {
		t.Fatal(err)
	}
	// Stored as the TUI's store names one (internal/harness/tool/attach, which
	// an engine test may not import): the first 16 hex digits of its SHA-256,
	// 0600, in a 0700 attachments directory.
	dir := paths.AttachmentsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(img.Bytes())
	stored := filepath.Join(dir, hex.EncodeToString(sum[:8])+".png")
	if err := os.WriteFile(stored, img.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	envelope := agent.AttachmentBlock([]agent.AttachmentRef{{N: 1, Path: stored, MIME: "image/png"}})
	pathText := "[Image #1: " + stored + "]\nalso [Image #1]"

	ctx, cancel := context.WithTimeout(context.Background(), 6*watchdog)
	defer cancel()
	grok := agent.GrokProvider()
	sess := agent.New(agent.Options{
		Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=grok-long-turn"},
		Workspace: t.TempDir(), Stderr: io.Discard, Provider: &grok,
	})
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	p := readPrimary(t, e, false)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c := dialWire(t, ctx, serveWire(t, e))
	n := 0
	cmd := func() engine.Command {
		n++
		return engine.Command{Client: c.s.ClientID(), ID: strconv.Itoa(n)}
	}
	isTool := func(ev agent.Event) bool { return ev.Type == agent.EventTool }

	// A plain prompt holds the session for the two busy modes.
	r1, err := c.s.Submit(ctx, cmd(), "go", engine.SubmitQueue, "")
	if err != nil || r1.Turn == "" {
		t.Fatalf("submit: %+v, %v", r1, err)
	}
	at := p.waitFor(t, p.waitFor(t, 0, startedTurn(r1.Turn)), isTool)

	// mode interject: path text.
	if err := c.s.Interject(ctx, cmd(), envelope+"also [Image #1]"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	// mode queue, behind the running turn: a row, the envelope kept in it.
	r2, err := c.s.Submit(ctx, cmd(), envelope+"queued [Image #1]", engine.SubmitQueue, "")
	if err != nil || r2.Queued == nil {
		t.Fatalf("queue: %+v, %v; want a row", r2, err)
	}
	end1 := p.waitFor(t, at, endedTurn(r1.Turn))
	// The row drains as the next turn; mode send_now replaces it.
	start2 := p.waitFor(t, end1, func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnStarted && ev.Turn.Origin == agent.TurnOriginDrain
	})
	p.waitFor(t, start2, isTool)
	r3, err := c.s.Submit(ctx, cmd(), envelope+"now [Image #1]", engine.SubmitSendNow, "")
	if err != nil || !r3.Armed {
		t.Fatalf("send_now: %+v, %v; want armed", r3, err)
	}
	start3 := p.waitFor(t, start2, func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnStarted && ev.Turn.Origin == agent.TurnOriginSendNow
	})
	p.waitFor(t, start3, endedTurn(p.event(start3).Turn.ID))

	lines, raw := readDump(t, dump)
	if strings.Contains(raw, "craze_attachments") {
		t.Fatalf("an envelope reached the agent:\n%s", raw)
	}
	if len(lines) != 4 {
		t.Fatalf("the agent read %d prompts and interjections, want 4:\n%s", len(lines), raw)
	}
	if len(lines[0].Prompt) != 1 || lines[0].Prompt[0].Text != "go" {
		t.Fatalf("line 1 %+v, want the plain prompt", lines[0])
	}
	if lines[1].Interject == nil || *lines[1].Interject != pathText {
		t.Fatalf("line 2 %+v, want the interjection as %q", lines[1], pathText)
	}
	for i, want := range []string{"queued [Image #1]", "now [Image #1]"} {
		got := lines[2+i].Prompt
		if len(got) != 2 || got[0].Text != want || got[1].Type != "image" || got[1].MimeType != "image/png" || got[1].Bytes != img.Len() {
			t.Fatalf("line %d %+v, want %q and the image block", 3+i, got, want)
		}
	}
	// The interjection's echo is what was sent.
	p.mu.Lock()
	evs := append([]agent.Event(nil), p.evs...)
	p.mu.Unlock()
	echoed := false
	for _, ev := range evs {
		if ev.Type == agent.EventUser && ev.Interjection {
			if ev.Text != pathText {
				t.Fatalf("the interjection echoed as %q", ev.Text)
			}
			echoed = true
		}
	}
	if !echoed {
		t.Fatal("no interjection echo")
	}
}
