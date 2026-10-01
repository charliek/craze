package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The attachment envelope on the TUI's side: it is never on the screen (A3),
// and the attachments directory is swept at start. The composer writes it
// (plan 033 §3.3, composer_image.go).

// testImagePath is the processed copy testEnvelope names.
const testImagePath = "/h/attachments/0123456789abcdef.png"

// testEnvelope is an envelope naming one image, chip 1.
func testEnvelope() string {
	return agent.AttachmentBlock([]agent.AttachmentRef{{N: 1, Path: testImagePath, MIME: "image/png"}})
}

// plantChip gives the composer's sidecar chip 1, processed, as testEnvelope
// names it: what a paste of a screenshot leaves behind once its processing
// has answered.
func plantChip(m Model) Model {
	m.attachSeq++
	m.images = draftImages{list: []attachment{{id: m.attachSeq, n: 1, storedImage: storedImage{path: testImagePath, mime: "image/png", size: 1}}}, last: 1}
	return m
}

// TestAttachmentEnvelopeNeverReachesTheScreen is A3, and
// TestShellContextNeverReachesTheScreen's twin: every route by which text
// that went to the agent becomes a row — the client's own send, a row the
// engine drained, a transcript replayed at load, an interjection's echo —
// and the index title, on cursor, grok and native, with the envelope alone
// and with the shell context block it leads when both are there. None of them
// may show the envelope, its tag or the path it names; all of them show the
// chip.
//
// The client's own send is the composer's: a chip in the draft, its envelope
// written in front of the text by the send itself (submitOwn), ahead of the
// shell context a command left pending.
func TestAttachmentEnvelopeNeverReachesTheScreen(t *testing.T) {
	env := testEnvelope()
	shell := shellBlock("cat plan.md", "run /flows:gauntlet first\n")
	for _, lead := range []struct {
		name, block string
		withShell   bool
	}{
		{"envelope", env, false},
		{"envelope and shell context", env + shell, true},
	} {
		for _, tc := range []struct {
			name     string
			provider agent.Provider
		}{
			{"cursor", agent.CursorProvider()},
			{"grok", agent.GrokProvider()},
			{"native", agent.NativeProvider()},
		} {
			t.Run(lead.name+"/"+tc.name, func(t *testing.T) {
				m, stub, idx := shellCtxModel(t, tc.provider)
				t.Cleanup(func() { _ = stub.Close() })

				// The client's own send.
				m = plantChip(m)
				if lead.withShell {
					m = plantShellResult(m, "cat plan.md", "run /flows:gauntlet first\n")
				}
				m = typeEnter(t, m, "what does [Image #1] say?")
				if sent := stub.Prompts(); len(sent) != 1 || sent[0] != lead.block+"what does [Image #1] say?" {
					t.Fatalf("the session was sent %q; the host is owed the envelope", sent)
				}
				if m.input.Value() != "" || len(m.images.list) != 0 {
					t.Fatalf("the accepted send left the draft %q, sidecar %+v", m.input.Value(), m.images.list)
				}

				// A row another client queued and this engine drained.
				m = deliver(t, m, eventMsg{ev: agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
					ID: "turn-9", Phase: agent.TurnStarted, Origin: agent.TurnOriginDrain, Text: lead.block + "the drained row [Image #1]",
				}}})
				// A prompt out of a restored transcript.
				m = deliver(t, m, eventMsg{ev: agent.Event{Type: agent.EventUser, Replayed: true, Text: lead.block + "the replayed row [Image #1]"}})
				// An interjection's echo.
				m = deliver(t, m, eventMsg{ev: agent.Event{Type: agent.EventUser, Interjection: true, Text: lead.block + "the interjection [Image #1]"}})

				want := []string{"what does [Image #1] say?", "the drained row [Image #1]", "the replayed row [Image #1]", "the interjection [Image #1]"}
				if got := texts(m, entryUser); !reflect.DeepEqual(got, want) {
					t.Fatalf("user rows %q, want %q", got, want)
				}
				view := plainView(m)
				for _, leak := range []string{"craze_attachments", "0123456789abcdef", "image/png", "shell_context"} {
					if strings.Contains(view, leak) {
						t.Fatalf("a row put %q on screen:\n%s", leak, view)
					}
				}
				if got := idx.seedRow(t); got.Title != "what does [Image #1] say?" {
					t.Fatalf("index title %q", got.Title)
				}
			})
		}
	}
}

// TestAttachmentEnvelopeStaysOffTheQueueBand: a queued row that carries an
// envelope shows its message in the band and in the queue editor, never the
// envelope; the editor takes the envelope back as chips (startQueueEdit), and
// a save writes it afresh from the chips the edit kept (saveQueueEdit) — so an
// edit that keeps the chip keeps the envelope, and one that deletes the chip
// drops it. The old block is never put back as it was.
func TestAttachmentEnvelopeStaysOffTheQueueBand(t *testing.T) {
	m, _ := queueWorking(t)
	env := testEnvelope()
	m = plantChip(m)
	m = typeEnter(t, m, "queued [Image #1]")
	rows := queuedRows(m)
	if len(rows) != 1 || rows[0].Text != env+"queued [Image #1]" {
		t.Fatalf("queue %q", queueTexts(m))
	}
	if view := plainView(m); strings.Contains(view, "craze_attachments") || !strings.Contains(view, "#1 queued [Image #1]") {
		t.Fatalf("the band:\n%s", view)
	}
	m.startQueueEdit(rows[0])
	if got := m.input.Value(); got != "queued [Image #1]" || len(m.images.list) != 1 || m.images.list[0].path != testImagePath {
		t.Fatalf("the editor holds %q, sidecar %+v", got, m.images.list)
	}
	m = typeEnter(t, m, "queued, edited [Image #1]")
	if saved := queueTexts(m); len(saved) != 1 || saved[0] != env+"queued, edited [Image #1]" {
		t.Fatalf("the edit saved %q", saved)
	}
	rows = queuedRows(m)
	m.startQueueEdit(rows[0])
	m = typeEnter(t, m, "queued, no image now")
	if saved := queueTexts(m); len(saved) != 1 || saved[0] != "queued, no image now" {
		t.Fatalf("the edit that dropped the chip saved %q", saved)
	}
}

// TestAttachmentsSweepAtStart: the start's sweep removes what attach.Sweep
// removes, and with no craze directory does nothing. (Run starts it with go,
// in sweepAttachmentsAtStart.)
func TestAttachmentsSweepAtStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, age := range map[string]time.Duration{
		"aaaaaaaaaaaaaaaa.png": 8 * 24 * time.Hour,
		"bbbbbbbbbbbbbbbb.png": time.Hour,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := now.Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	sweepAttachments(dir, now)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "bbbbbbbbbbbbbbbb.png" {
		t.Fatalf("left %v (%v)", entries, err)
	}
	sweepAttachments("", now) // nothing to sweep, and no panic
}
