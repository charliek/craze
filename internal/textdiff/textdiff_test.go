package textdiff

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestIdenticalNoHunks(t *testing.T) {
	added, removed, hunks, err := Lines("a\nb\nc\n", "a\nb\nc\n")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || removed != 0 || len(hunks) != 0 {
		t.Fatalf("+%d -%d %d hunks", added, removed, len(hunks))
	}
}

func TestOneLineChanged(t *testing.T) {
	old := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	newText := strings.Replace(old, "hello", "hello, world", 1)
	added, removed, hunks, err := Lines(old, newText)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || removed != 1 {
		t.Fatalf("want +1 -1, got +%d -%d", added, removed)
	}
	if len(hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	// 2 context lines before, the -/+ pair, 1 context line after (EOF).
	if h.OldStart != 4 || h.NewStart != 4 {
		t.Fatalf("hunk starts %d/%d", h.OldStart, h.NewStart)
	}
	var got []string
	for _, l := range h.Lines {
		got = append(got, string(l.Kind)+l.Text)
	}
	want := []string{
		" ",
		" func main() {",
		"-\tfmt.Println(\"hello\")",
		"+\tfmt.Println(\"hello, world\")",
		" }",
	}
	if len(got) != len(want) {
		t.Fatalf("hunk lines %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if h.Lines[2].OldNo != 6 || h.Lines[2].NewNo != 0 {
		t.Fatalf("delete numbering %+v", h.Lines[2])
	}
	if h.Lines[3].OldNo != 0 || h.Lines[3].NewNo != 6 {
		t.Fatalf("add numbering %+v", h.Lines[3])
	}
}

func TestEmptyOldIsAllAdds(t *testing.T) {
	added, removed, hunks, err := Lines("", "a\nb\n")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || removed != 0 {
		t.Fatalf("+%d -%d", added, removed)
	}
	if len(hunks) != 1 || len(hunks[0].Lines) != 2 {
		t.Fatalf("hunks %+v", hunks)
	}
	for _, l := range hunks[0].Lines {
		if l.Kind != Add {
			t.Fatalf("kind %q", string(l.Kind))
		}
	}
}

func TestEmptyNewIsAllDeletes(t *testing.T) {
	added, removed, _, err := Lines("a\nb\nc\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || removed != 3 {
		t.Fatalf("+%d -%d", added, removed)
	}
}

func TestCRLFNormalised(t *testing.T) {
	added, removed, _, err := Lines("a\r\nb\r\n", "a\nb\n")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || removed != 0 {
		t.Fatalf("CRLF should not count as a change: +%d -%d", added, removed)
	}
}

func TestMissingFinalNewlineIsALine(t *testing.T) {
	added, removed, _, err := Lines("a\nb", "a\nb\n")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || removed != 0 {
		t.Fatalf("+%d -%d", added, removed)
	}
	added, removed, _, err = Lines("a\nb", "a\nc")
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || removed != 1 {
		t.Fatalf("+%d -%d", added, removed)
	}
}

func TestSeparateChangesGiveSeparateHunks(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = "line " + strconv.Itoa(i)
	}
	old := strings.Join(lines, "\n") + "\n"
	lines[2] = "changed-a"
	lines[30] = "changed-b"
	added, removed, hunks, err := Lines(old, strings.Join(lines, "\n")+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || removed != 2 {
		t.Fatalf("+%d -%d", added, removed)
	}
	if len(hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(hunks))
	}
}

func TestTooManyLines(t *testing.T) {
	old := strings.Repeat("a\n", maxLines/2+1)
	newText := strings.Repeat("b\n", maxLines/2+1)
	_, _, _, err := Lines(old, newText)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err %v", err)
	}
}

func TestStepCapTripsBeforeCompleting(t *testing.T) {
	// 4 000 lines a side with nothing in common: the edit distance is far past
	// what maxSteps allows.
	var oldB, newB strings.Builder
	for i := 0; i < 4000; i++ {
		oldB.WriteString("old line ")
		oldB.WriteString(strings.Repeat("o", i%7))
		oldB.WriteByte('\n')
		newB.WriteString("new line ")
		newB.WriteString(strings.Repeat("n", i%5))
		newB.WriteByte('\n')
	}
	_, _, _, err := Lines(oldB.String(), newB.String())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err %v", err)
	}
}

func TestLargeFileOneChangeStaysInBudget(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 200*1024; i++ {
		b.WriteString("line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte(' ')
		b.WriteString(strings.Repeat("x", 64))
		b.WriteByte('\n')
	}
	old := b.String()
	added, removed, hunks, err := Lines(old, old+"tail changed\n")
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || removed != 0 {
		t.Fatalf("+%d -%d", added, removed)
	}
	if len(hunks) != 1 {
		t.Fatalf("hunks %d", len(hunks))
	}
}
