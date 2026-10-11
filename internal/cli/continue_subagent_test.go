package cli

import (
	"testing"
	"time"

	"github.com/charliek/craze/internal/sessions"
)

// --continue and --resume over sub-agent sessions (plan 041 §3.3, §7 W3 17-18):
// an unattended host's rows are skipped by --continue, counted in its
// refusal, and shown last by the picker.

func subAgentRow(id, provider, cwd string) sessions.Row {
	return sessions.Row{
		SessionID: id, Provider: provider, CWD: cwd, Title: "sub " + id, TitleKind: sessions.TitleKindAgent,
		CrazeID:    "018f-" + id,
		Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass},
	}
}

// TestContinueSkipsANewerSubAgentSession: the person's session loads although
// a sub-agent's is newer.
func TestContinueSkipsANewerSubAgentSession(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "person", Provider: "cursor", CWD: ws, Title: "mine", TitleKind: sessions.TitleKindAgent}, time.Hour)
	seedRow(t, subAgentRow("sub", "cursor", ws), time.Minute)

	cfg, built, err := runResolveLoad(t, ws, "--continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if len(built) != 1 || built[0].LoadSessionID != "person" {
		t.Fatalf("--continue built %+v, want a load of the person's session", built)
	}
	if !cfg.Loading {
		t.Fatal("the loaded session is not flagged as loading")
	}
}

// TestContinueWithOnlySubAgentSessionsRefusesWithAHint: the refusal stands
// and says how many sub-agent sessions the directory holds under the
// provider filter, naming craze -r.
func TestContinueWithOnlySubAgentSessionsRefusesWithAHint(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	seedRow(t, subAgentRow("a", "cursor", ws), 2*time.Hour)
	seedRow(t, subAgentRow("b", "grok", ws), time.Hour)
	seedRow(t, subAgentRow("c", "cursor", t.TempDir()), time.Minute)

	hint := " sub-agent sessions here: craze -r lists them)"
	one := " sub-agent session here: craze -r lists them)"
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"any provider", []string{"--continue"}, "craze: no session to continue in " + ws + " (2" + hint},
		{"filtered", []string{"--continue", "--provider", "grok"}, "craze: no session to continue in " + ws + " for provider grok (1" + one},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := runResolveLoad(t, ws, tc.argv...)
			code, msg := exitCode(t, err)
			if code != 1 || msg != tc.want {
				t.Fatalf("exit %d %q, want 1 %q", code, msg, tc.want)
			}
			if cfg.Session != nil {
				t.Fatal("a refusal left a session behind")
			}
		})
	}
}

// TestResumePickerOffersPeopleSessionsFirst: the picker's rows are the
// person's, newest first, then the sub-agent's, and a sub-agent's session is
// never crowded out of the picker by being merely older (it fills the rest).
func TestResumePickerOffersPeopleSessionsFirst(t *testing.T) {
	indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "p-old", Provider: "cursor", CWD: ws, Title: "p-old", TitleKind: sessions.TitleKindAgent}, 3*time.Hour)
	seedRow(t, subAgentRow("sub-new", "cursor", ws), time.Minute)
	seedRow(t, sessions.Row{SessionID: "p-new", Provider: "cursor", CWD: ws, Title: "p-new", TitleKind: sessions.TitleKindAgent}, time.Hour)

	cfg, _, err := runResolveLoad(t, ws, "--resume")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	var got []string
	for _, r := range cfg.Resume {
		got = append(got, r.SessionID)
	}
	want := []string{"p-new", "p-old", "sub-new"}
	if len(got) != len(want) {
		t.Fatalf("picker rows %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picker rows %v, want %v", got, want)
		}
	}
}
