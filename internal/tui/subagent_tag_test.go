package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
)

// The sub-agent tag (plan 041 §3.3, §7 W3 18): a row the index marks carries
// the dim word after its title in the resume picker and the session list's
// saved rows, and no other row does. No golden covers it; these assert it
// directly.

func markedResumeRow(id, provider, title string, age time.Duration) sessions.Row {
	row := resumeRow(id, provider, title, age)
	row.Unattended = &sessions.Unattended{PermissionMode: sessions.PermissionBypass}
	return row
}

// lineWith is the first line of a rendered view holding s.
func lineWith(t *testing.T, view, s string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, s) {
			return line
		}
	}
	t.Fatalf("no line holds %q:\n%s", s, view)
	return ""
}

func TestResumePickerTagsSubAgentRowsOnly(t *testing.T) {
	m, _ := newResumePicker(t, []sessions.Row{
		resumeRow("s-1", "grok", "the person's session", 3*time.Minute),
		markedResumeRow("s-2", "cursor", "the delegated session", 2*time.Hour),
	}, 100, 30)
	view := plainView(m)

	marked := lineWith(t, view, "the delegated session")
	if !strings.Contains(marked, "the delegated session "+subAgentTag) {
		t.Fatalf("the marked row has no tag after its title: %q", marked)
	}
	if !strings.Contains(marked, "cursor") || !strings.Contains(marked, "2h") {
		t.Fatalf("the tag crowded out the provider or age: %q", marked)
	}
	if plain := lineWith(t, view, "the person's session"); strings.Contains(plain, subAgentTag) {
		t.Fatalf("an unmarked row carries the tag: %q", plain)
	}
}

// TestResumePickerTagSurvivesALongTitle: the title is clamped, not the tag.
func TestResumePickerTagSurvivesALongTitle(t *testing.T) {
	long := strings.Repeat("delegated work ", 12)
	m, _ := newResumePicker(t, []sessions.Row{markedResumeRow("s-1", "cursor", long, time.Hour)}, 80, 30)
	line := lineWith(t, plainView(m), "delegated")
	if !strings.Contains(line, subAgentTag) || !strings.Contains(line, "cursor") {
		t.Fatalf("a long title lost the tag or the provider: %q", line)
	}
}

func TestSessionsListTagsSavedSubAgentRowsOnly(t *testing.T) {
	a, s, p, q := newLane(t, "a", "alpha"), newLane(t, "s", "sierra"), newLane(t, "p", "papa"), newLane(t, "q", "quebec")
	r, _ := laneModel(t, a, map[string][]*laneBackend{})
	sub := savedLane(s, "delegated")
	sub.Unattended = &sessions.Unattended{PermissionMode: sessions.PermissionBypass}
	person := savedLane(p, "person's one")
	// A running session whose newest index row is marked is tagged; one with
	// no index row yet (IndexSubAgent false) is not.
	runningSub := laneRow(q, "session q", time.Minute)
	runningSub.IndexSubAgent = true
	r.openSavedList([]roster.Row{laneRow(a, "session a", time.Minute), runningSub}, sub, person)
	view := plainView(r.m)

	marked := lineWith(t, view, "delegated")
	if !strings.Contains(marked, "delegated"+strings.Repeat(" ", 0)) || !strings.Contains(marked, subAgentTag) {
		t.Fatalf("the saved sub-agent row has no tag: %q", marked)
	}
	if strings.Index(marked, subAgentTag) < strings.Index(marked, "delegated") {
		t.Fatalf("the tag is not after the title: %q", marked)
	}
	if plain := lineWith(t, view, "person's one"); strings.Contains(plain, subAgentTag) {
		t.Fatalf("an unmarked saved row carries the tag: %q", plain)
	}
	if running := lineWith(t, view, "session a"); strings.Contains(running, subAgentTag) {
		t.Fatalf("a running row with no index mark carries the tag: %q", running)
	}
	tagged := lineWith(t, view, "session q")
	if !strings.Contains(tagged, subAgentTag) || strings.Index(tagged, subAgentTag) < strings.Index(tagged, "session q") {
		t.Fatalf("a running sub-agent row has no tag after its title: %q", tagged)
	}
}
