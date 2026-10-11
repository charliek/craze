package engine

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/sessions"
)

// TestAnUnattendedHostMarksEveryRowItWrites is plan 041 §3.2's index record at
// the engine: a host nobody attends (IndexOptions.Unattended, craze serve
// --unattended) puts its start settings on every row it writes — the first
// prompt's seed, a rename, the turn's touch — and every other host writes
// none, so the index's set-only rule is all that decides the mark.
func TestAnUnattendedHostMarksEveryRowItWrites(t *testing.T) {
	rec := &sessions.Unattended{Mode: "plan", PermissionMode: sessions.PermissionPrompt, Model: "m-1", Effort: "high"}
	for _, tc := range []struct {
		name string
		rec  *sessions.Unattended
	}{{"unattended", rec}, {"attended", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newFakeIndex()
			r := newRig(t, Options{CrazeSessionID: "018f-the-thread", Index: IndexOptions{
				Store: idx, CWD: "/w", Provider: "cursor", Unattended: tc.rec,
				TitleLine: func(s string) string { return strings.TrimSpace(s) },
			}})
			r.submit("a first prompt")
			r.until(lastEnding)
			if err := r.e.SetTitle(Command{}, "renamed"); err != nil {
				t.Fatalf("rename: %v", err)
			}
			r.sync()
			rows := idx.all()
			if len(rows) < 2 {
				t.Fatalf("%d rows written: %+v", len(rows), rows)
			}
			for _, row := range rows {
				if (row.Unattended == nil) != (tc.rec == nil) || tc.rec != nil && *row.Unattended != *tc.rec {
					t.Fatalf("a row carried %+v, want %+v: %+v", row.Unattended, tc.rec, rows)
				}
			}
		})
	}
}
