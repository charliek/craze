package sessions

import "testing"

// TestUpsertRecordsTheEffort (plan 052 L5 F1): a row carries the effort an
// upsert names; an upsert naming none leaves it; the key is absent from a row
// that never had one, and survives a reread.
func TestUpsertRecordsTheEffort(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "claude", CWD: "/ws", Effort: "high"})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "claude", CWD: "/ws", Effort: "low"})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "claude", CWD: "/ws"})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws2"})
	var fresh Store
	row, ok, err := fresh.Latest("/ws", "")
	if err != nil || !ok || row.Effort != "low" {
		t.Fatalf("row %+v ok=%v err=%v", row, ok, err)
	}
	row, ok, err = fresh.Latest("/ws2", "")
	if err != nil || !ok || row.Effort != "" {
		t.Fatalf("cursor row %+v", row)
	}
}
