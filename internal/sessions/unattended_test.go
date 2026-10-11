package sessions

import (
	"os"
	"strings"
	"testing"
)

// The session index's unattended record (plan 041 §3.2, P10–P11, §7 W2 15):
// written only when set, read back whole, set-only, and inherited by craze id
// by every new row of a marked thread.

// rowOf is the index's row for (provider, sessionID), failing the test when
// there is none.
func rowOf(t *testing.T, s *Store, provider, sessionID string) Row {
	t.Helper()
	row, ok, err := s.Find(provider, sessionID)
	if err != nil || !ok {
		t.Fatalf("Find(%s, %s): ok=%v err=%v", provider, sessionID, ok, err)
	}
	return row
}

// TestUnattendedRoundTripsAndIsOmittedWhenUnset: a marked row's record is in
// the file as one object with its four keys — "" kept, not dropped — and read
// back as it was written; an unmarked row has no key at all.
func TestUnattendedRoundTripsAndIsOmittedWhenUnset(t *testing.T) {
	path := setIndex(t)
	var s Store
	rec := Unattended{Mode: "plan", PermissionMode: PermissionPrompt, Model: "m-1", Effort: ""}
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", CrazeID: "018f-sub", Unattended: &rec})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws", CrazeID: "018f-person"})

	if got := rowOf(t, &s, "cursor", "s1").Unattended; got == nil || *got != rec {
		t.Fatalf("the marked row read back %+v, want %+v", got, rec)
	}
	if got := rowOf(t, &s, "cursor", "s2").Unattended; got != nil {
		t.Fatalf("the unmarked row read back %+v", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `"unattended"`); n != 1 {
		t.Fatalf("%d unattended keys in the file, want one:\n%s", n, b)
	}
	if !strings.Contains(string(b), `"unattended":{"effort":"","mode":"plan","model":"m-1","permissionMode":"prompt"}`) {
		t.Fatalf("the record is not in the file whole:\n%s", b)
	}
}

// TestUnattendedIsSetOnly: a write with no record never clears one (most
// writes are touches and titles), a later record never replaces the first
// (it says how the session first started), and an unmarked row is marked by
// the first write that has one.
func TestUnattendedIsSetOnly(t *testing.T) {
	setIndex(t)
	var s Store
	first := Unattended{Mode: "ask", PermissionMode: PermissionBypass, Model: "a", Effort: "high"}
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", CrazeID: "018f-sub", Unattended: &first})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", CrazeID: "018f-sub", Title: "t", TitleKind: TitleKindAgent})
	other := Unattended{PermissionMode: PermissionPrompt}
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", Unattended: &other})
	if got := rowOf(t, &s, "cursor", "s1").Unattended; got == nil || *got != first {
		t.Fatalf("the record became %+v, want the first %+v", got, first)
	}

	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws"})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws", Unattended: &other})
	if got := rowOf(t, &s, "cursor", "s2").Unattended; got == nil || *got != other {
		t.Fatalf("an unmarked row was given %+v, want %+v", got, other)
	}
}

// TestANewRowOfAMarkedThreadInheritsItsRecord is P10: a load under a new
// provider session id writes a new row of the same craze id, and that row is
// marked as the thread's first was — whoever writes it, a person's
// interactive host (no record of its own) too, and over a record of the
// writer's own. A row of another craze id, and one with no id, inherit
// nothing.
func TestANewRowOfAMarkedThreadInheritsItsRecord(t *testing.T) {
	setIndex(t)
	var s Store
	rec := Unattended{Mode: "plan", PermissionMode: PermissionPrompt, Model: "m-1", Effort: "low"}
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws", CrazeID: "018f-sub", Unattended: &rec})

	mustUpsert(t, &s, Row{SessionID: "s1-loaded", Provider: "cursor", CWD: "/ws", CrazeID: "018f-sub"})
	if got := rowOf(t, &s, "cursor", "s1-loaded").Unattended; got == nil || *got != rec {
		t.Fatalf("the load's new row has %+v, want the thread's %+v", got, rec)
	}
	mine := Unattended{PermissionMode: PermissionBypass}
	mustUpsert(t, &s, Row{SessionID: "s1-again", Provider: "cursor", CWD: "/ws", CrazeID: "018f-sub", Unattended: &mine})
	if got := rowOf(t, &s, "cursor", "s1-again").Unattended; got == nil || *got != rec {
		t.Fatalf("a marked writer's new row has %+v, want the thread's %+v", got, rec)
	}

	mustUpsert(t, &s, Row{SessionID: "p1", Provider: "cursor", CWD: "/ws", CrazeID: "018f-person"})
	mustUpsert(t, &s, Row{SessionID: "p0", Provider: "cursor", CWD: "/ws"})
	for _, sid := range []string{"p1", "p0"} {
		if got := rowOf(t, &s, "cursor", sid).Unattended; got != nil {
			t.Fatalf("row %s inherited %+v from another thread", sid, got)
		}
	}
}

// TestAnUnattendedObjectRoundTripsThroughAnOlderShape: a key a newer craze
// added inside the record survives this craze's rewrite of the row, and an
// `unattended` that is not an object is no record and is written back as it
// was.
func TestAnUnattendedObjectRoundTripsThroughAnOlderShape(t *testing.T) {
	path := setIndex(t)
	writeRaw(t, path,
		`{"sessionId":"s1","provider":"cursor","cwd":"/ws","title":"t","pinned":false,"crazeId":"018f-a",`+
			`"unattended":{"mode":"plan","permissionMode":"bypass","model":"","effort":"","fast":true},`+
			`"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z"}`+"\n"+
			`{"sessionId":"s2","provider":"cursor","cwd":"/ws","title":"t","pinned":false,"unattended":"odd",`+
			`"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z"}`+"\n")
	var s Store
	if got := rowOf(t, &s, "cursor", "s1").Unattended; got == nil || got.Mode != "plan" || got.PermissionMode != PermissionBypass {
		t.Fatalf("the record read %+v", got)
	}
	if got := rowOf(t, &s, "cursor", "s2").Unattended; got != nil {
		t.Fatalf("a non-object read as %+v", got)
	}
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "cursor", CWD: "/ws"})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"fast":true`, `"mode":"plan"`, `"unattended":"odd"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the rewrite lost %s:\n%s", want, b)
		}
	}
}
