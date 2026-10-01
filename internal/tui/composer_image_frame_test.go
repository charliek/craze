package tui

import (
	"strings"
	"testing"
	"time"
)

// TestFrameGoldenImageChips is A1 and A2 through the real program, on both
// transports (plan 033 §3.3): a bracketed paste of a path to a real PNG is the
// chip [Image #1], a second paste [Image #2], one Backspace takes a chip
// whole; a typed path, and a paste mixing prose and a path, stay text.
//
// The PNGs live in a fixture directory the TUI's environment names as HOME
// (Config.Getenv), so every script pastes the fixed text ~/shot.png — expanded
// by the paste rules as a terminal's `~/` would be — and the frames of the
// pastes that stay text are the same on every run. The run's own HOME stays
// the frame runner's isolated one, which is where the processed copies go
// (paths.AttachmentsDir); the socket run's host shares it, and says it reads
// them (buildSocketHost's ReadsAttachments, P27). Each run waits for its
// chips' processing before it ends (frameState.settled).
func TestFrameGoldenImageChips(t *testing.T) {
	shots := t.TempDir()
	writePNG(t, shots, "shot.png", 40, 20)
	writePNG(t, shots, "two.png", 24, 16)
	for _, tc := range []struct {
		name, keys, wantFirst string
	}{
		{"composer-image-chip-100x30", "<paste:~/shot.png>", "❯ [Image #1] "},
		{"composer-image-two-chips-100x30", "<paste:~/shot.png> <paste:~/two.png>", "❯ [Image #1] [Image #2] "},
		// One Backspace takes all ten characters of the chip.
		{"composer-image-backspace-100x30", "before <paste:~/shot.png><backspace>after", "❯ before after "},
		{"composer-image-typed-path-100x30", "~/shot.png", "❯ ~/shot.png "},
		{"composer-image-mixed-paste-100x30", "<paste:look at ~/shot.png>", "❯ look at ~/shot.png "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runImageFrame(t, shots, 100, 30, "<wait:idle>"+tc.keys)
			assertGolden(t, tc.name, 100, 30, got)
			if first := composerRow(t, got, 0); !strings.HasPrefix(first, tc.wantFirst) {
				t.Fatalf("first composer row is %q, want it to start %q", first, tc.wantFirst)
			}
			if strings.Contains(got, shots) || strings.Contains(got, "craze_attachments") {
				t.Fatalf("a path or the envelope is on screen:\n%s", got)
			}
		})
	}
}

// runImageFrame is runStubFrame with the TUI's environment naming home as
// HOME (and nothing else: no SSH, no display).
func runImageFrame(t *testing.T, home string, cols, rows int, script string) string {
	t.Helper()
	isolateSkillsHome(t)
	plain, _, err := runFrameModes(t, func() Config {
		return Config{
			Session:   frameStub(),
			Theme:     "tokyo-night",
			Workspace: frameWorkspace(t),
			Model:     "grok",
			Yolo:      true,
			Getenv: func(k string) string {
				if k == "HOME" {
					return home
				}
				return ""
			},
		}
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plain
}
