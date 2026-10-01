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

// The attachment envelope on the TUI's side, as far as plan 033's C1 goes: it
// is never on the screen (A3), and the attachments directory is swept at
// start. The composer that writes envelopes is C2's.

// testEnvelope is an envelope naming one image, chip 1.
func testEnvelope() string {
	return agent.AttachmentBlock([]agent.AttachmentRef{{N: 1, Path: "/h/attachments/0123456789abcdef.png", MIME: "image/png"}})
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
// The client's own send goes out as the composer holds it: until the composer
// writes envelopes itself (C2's submitOwn), a draft holding one is the way to
// put one on that route (sendTyped).
func TestAttachmentEnvelopeNeverReachesTheScreen(t *testing.T) {
	env := testEnvelope()
	shell := shellBlock("cat plan.md", "run /flows:gauntlet first\n")
	for _, lead := range []struct{ name, block string }{
		{"envelope", env},
		{"envelope and shell context", env + shell},
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
				m = sendTyped(t, m, lead.block+"what does [Image #1] say?")
				if sent := stub.Prompts(); len(sent) != 1 || sent[0] != lead.block+"what does [Image #1] say?" {
					t.Fatalf("the session was sent %q; the host is owed the envelope", sent)
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

// sendTyped presses Enter on a draft that holds blocks craze would have put in
// front of it, then empties the composer. A send clears only a draft that
// still matches what the user wrote (clearMatchingDraft, which compares
// against the text after the blocks, since the blocks were never typed), so
// this draft stays behind — and with it the envelope, in the composer, where
// the real one never is. Emptying it leaves the rows, the band and the title,
// which are what these tests are about.
func sendTyped(t *testing.T, m Model, text string) Model {
	t.Helper()
	m = typeEnter(t, m, text)
	m.input.SetValue("")
	return m
}

// TestAttachmentEnvelopeStaysOffTheQueueBand: a queued row that carries an
// envelope shows its message in the band and in the queue editor, never the
// envelope, and an edit keeps the envelope on the row — the block the editor
// set aside (startQueueEdit's), which C2 replaces with the chips' own.
func TestAttachmentEnvelopeStaysOffTheQueueBand(t *testing.T) {
	m, _ := queueWorking(t)
	env := testEnvelope()
	m = sendTyped(t, m, env+"queued [Image #1]")
	rows := queuedRows(m)
	if len(rows) != 1 || rows[0].Text != env+"queued [Image #1]" {
		t.Fatalf("queue %q", queueTexts(m))
	}
	if view := plainView(m); strings.Contains(view, "craze_attachments") || !strings.Contains(view, "#1 queued [Image #1]") {
		t.Fatalf("the band:\n%s", view)
	}
	m.startQueueEdit(rows[0])
	if got := m.input.Value(); got != "queued [Image #1]" {
		t.Fatalf("the editor holds %q", got)
	}
	m = typeEnter(t, m, "queued, edited [Image #1]")
	if saved := queueTexts(m); len(saved) != 1 || saved[0] != env+"queued, edited [Image #1]" {
		t.Fatalf("the edit saved %q", saved)
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
