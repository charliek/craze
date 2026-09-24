package tui

import "testing"

// entries is the pane's display list as values, oldest first, in the []entry
// shape the transcript held before the pane (plan 024 §3.8, the compatibility
// accessor): render cache included, so a test can read what a row drew. It is
// a copy, so writing to it changes nothing. C5c retires it wherever a test can
// read the shared model instead.
func (t *pane) entries() []entry {
	out := make([]entry, len(t.rows))
	for i, e := range t.rows {
		out[i] = *e
	}
	return out
}

// TestModelCopiesShareTheirPanes pins pane's aliasing rule (plan 024 §3.8):
// bubbletea copies Model by value, and every copy holds the same panes, so a
// row written through one copy is a row of the other — main's, the rows it was
// painted into, and a sub-agent's pane created after the copy was taken.
func TestModelCopiesShareTheirPanes(t *testing.T) {
	m := sized(t)
	notes := len(texts(m, entryNote))
	cp := m
	cp.addNote("written through the copy")
	if got := texts(m, entryNote); len(got) != notes+1 || got[notes] != "written through the copy" {
		t.Fatalf("the original holds the notes %q, want the copy's row last", got)
	}
	cp.refreshViewport()
	if plain := m.main.transcriptPlain; len(plain) == 0 || plain[len(plain)-1] != "written through the copy" {
		t.Fatalf("the original's painted rows are not the copy's: %q", plain)
	}

	sub := cp.ensureSub("task-1")
	sub.addNote("in the child", cp.now())
	if m.subs["task-1"] != sub {
		t.Fatal("a sub-agent's pane made through the copy is not the original's")
	}
	if got := m.subs["task-1"].entries(); len(got) != 1 || got[0].text != "in the child" {
		t.Fatalf("the original's sub-agent pane holds %+v, want the child's row", got)
	}
}
