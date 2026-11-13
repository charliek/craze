package sessions

import (
	"os"
	"strings"
	"testing"
)

// The session index's web search choice (plan 051 F.3): a field of the row of
// its own, beside the unattended record and not inside it — written only when
// set, read back through a disk round trip, set-only, inherited by craze id,
// and a value this craze does not know kept as it came.

// TestWebSearchRoundTripsAndIsOmittedWhenUnset: "on" and "off" are in the
// file as the row's own key and read back as written; a row with neither has
// no key at all; none of them is marked unattended by it.
func TestWebSearchRoundTripsAndIsOmittedWhenUnset(t *testing.T) {
	path := setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "on", Provider: "native", CWD: "/ws", CrazeID: "018f-on", WebSearch: "on"})
	mustUpsert(t, &s, Row{SessionID: "off", Provider: "native", CWD: "/ws", CrazeID: "018f-off", WebSearch: "off"})
	mustUpsert(t, &s, Row{SessionID: "none", Provider: "native", CWD: "/ws", CrazeID: "018f-none"})

	for sid, want := range map[string]string{"on": "on", "off": "off", "none": ""} {
		row := rowOf(t, &s, "native", sid)
		if row.WebSearch != want || row.Unattended != nil {
			t.Fatalf("row %s read back webSearch %q, unattended %+v; want %q and none", sid, row.WebSearch, row.Unattended, want)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines:\n%s", len(lines), b)
	}
	for i, want := range []string{`"webSearch":"on"`, `"webSearch":"off"`, ""} {
		switch {
		case want == "" && strings.Contains(lines[i], "webSearch"):
			t.Fatalf("a row with no choice has the key: %s", lines[i])
		case want != "" && strings.Count(lines[i], want) != 1:
			t.Fatalf("line %d lacks %s: %s", i, want, lines[i])
		}
		if strings.Contains(lines[i], "unattended") {
			t.Fatalf("the choice marked the row unattended: %s", lines[i])
		}
	}
}

// TestWebSearchIsSetOnly: a write with no choice never clears one, a later
// choice never replaces the first (a follow-up cannot change it), and a row
// with none takes the first write that has one.
func TestWebSearchIsSetOnly(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "native", CWD: "/ws", CrazeID: "018f-a", WebSearch: "off"})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "native", CWD: "/ws", CrazeID: "018f-a", Title: "t", TitleKind: TitleKindAgent})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "native", CWD: "/ws", WebSearch: "on"})
	if got := rowOf(t, &s, "native", "s1").WebSearch; got != "off" {
		t.Fatalf("the choice became %q, want the first, off", got)
	}

	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "native", CWD: "/ws"})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "native", CWD: "/ws", WebSearch: "on"})
	if got := rowOf(t, &s, "native", "s2").WebSearch; got != "on" {
		t.Fatalf("a row with none was given %q, want on", got)
	}
}

// TestANewRowOfAThreadInheritsItsWebSearch: a load under a new provider
// session id writes a new row of the same craze id, which carries the
// thread's choice — whoever writes it, and over the writer's own. A row of
// another craze id, and one with no id, inherit nothing.
func TestANewRowOfAThreadInheritsItsWebSearch(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "native", CWD: "/ws", CrazeID: "018f-web", WebSearch: "on"})

	mustUpsert(t, &s, Row{SessionID: "s1-loaded", Provider: "native", CWD: "/ws", CrazeID: "018f-web"})
	if got := rowOf(t, &s, "native", "s1-loaded").WebSearch; got != "on" {
		t.Fatalf("the load's new row has %q, want the thread's on", got)
	}
	mustUpsert(t, &s, Row{SessionID: "s1-again", Provider: "native", CWD: "/ws", CrazeID: "018f-web", WebSearch: "off"})
	if got := rowOf(t, &s, "native", "s1-again").WebSearch; got != "on" {
		t.Fatalf("a writer's own choice won over the thread's: %q", got)
	}
	mustUpsert(t, &s, Row{SessionID: "p1", Provider: "native", CWD: "/ws", CrazeID: "018f-other"})
	mustUpsert(t, &s, Row{SessionID: "p0", Provider: "native", CWD: "/ws"})
	for _, sid := range []string{"p1", "p0"} {
		if got := rowOf(t, &s, "native", sid).WebSearch; got != "" {
			t.Fatalf("row %s inherited %q from another thread", sid, got)
		}
	}
}

// TestAWebSearchValueThisCrazeDoesNotKnow: a value other than on or off (a
// later craze's) reads as no choice and is written back as it came; one of
// another type the same.
func TestAWebSearchValueThisCrazeDoesNotKnow(t *testing.T) {
	path := setIndex(t)
	writeRaw(t, path,
		`{"sessionId":"s1","provider":"native","cwd":"/ws","title":"t","pinned":false,"webSearch":"ask-later",`+
			`"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z"}`+"\n"+
			`{"sessionId":"s2","provider":"native","cwd":"/ws","title":"t","pinned":false,"webSearch":true,`+
			`"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z"}`+"\n")
	var s Store
	for _, sid := range []string{"s1", "s2"} {
		if got := rowOf(t, &s, "native", sid).WebSearch; got != "" {
			t.Fatalf("row %s read %q", sid, got)
		}
	}
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "native", CWD: "/ws"})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "native", CWD: "/ws"})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"webSearch":"ask-later"`, `"webSearch":true`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the rewrite lost %s:\n%s", want, b)
		}
	}
}
