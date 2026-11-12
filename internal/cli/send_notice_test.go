package cli

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

// TestAFailedReloadKeepsTheDeprecationNotice (plan 050 review): craze send
// --wait to an ended unattended native session recorded with a deprecated
// --model (the shipped catalog's fireworks/kimi-k3) reloads it with that
// model, and a reload that fails — here no key funds anything, so the host
// cannot start the session at all — still says the notice, judged for the
// model the reload asked for (the row's): one stderr line and, under --json,
// the failure's `model_deprecated` warning; without --json the line alone.
// Negative control: a row that recorded no model says none.
func TestAFailedReloadKeepsTheDeprecationNotice(t *testing.T) {
	_, ws, _ := unattendedHome(t)
	native := agent.NativeProvider().Name()
	const notice = "craze send: fireworks/kimi-k3 is deprecated; use fireworks/kimi-k3p1-pro\n"
	for i, tc := range []struct {
		name  string
		model string
		json  bool
		want  bool
	}{
		{"--json", "fireworks/kimi-k3", true, true},
		{"no --json", "fireworks/kimi-k3", false, true},
		{"no recorded model (control)", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "0199c0de-6666-7000-8000-00000000f00" + string(rune('1'+i))
			seedIndexRow(t, sessions.Row{SessionID: "gone-" + id, Provider: native, CWD: absDir(ws), CrazeID: id, Title: "t",
				Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass, Model: tc.model}})
			argv := []string{"send", id[len(id)-8:], "--wait", "go"}
			if tc.json {
				argv = append(argv, "--json")
			}
			stdout, stderr, code := executeErr(argv)
			if code != 1 {
				t.Fatalf("craze send: exit %d, want 1 (the reload fails): %q %q", code, stdout, stderr)
			}
			if got := strings.Contains(stderr, notice); got != tc.want {
				t.Fatalf("stderr says the notice: %v, want %v: %q", got, tc.want, stderr)
			}
			if got := strings.Contains(stdout, `"kind":"model_deprecated"`); got != (tc.want && tc.json) {
				t.Fatalf("stdout carries the warning: %v, want %v: %q", got, tc.want && tc.json, stdout)
			}
		})
	}
}
