package sessions

import "testing"

// TestUpsertRecordsTheModel (plan 052 § C4): a row carries the model an
// upsert names; an upsert naming none leaves it; the key is absent from a
// row that never had one.
func TestUpsertRecordsTheModel(t *testing.T) {
	setIndex(t)
	var s Store
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "claude", CWD: "/ws", Model: "opus"})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "claude", CWD: "/ws", Model: "haiku"})
	mustUpsert(t, &s, Row{SessionID: "s1", Provider: "claude", CWD: "/ws"})
	mustUpsert(t, &s, Row{SessionID: "s2", Provider: "cursor", CWD: "/ws2"})
	row, ok, err := s.Latest("/ws", "")
	if err != nil || !ok || row.Model != "haiku" {
		t.Fatalf("row %+v ok=%v err=%v", row, ok, err)
	}
	row, ok, err = s.Latest("/ws2", "")
	if err != nil || !ok || row.Model != "" {
		t.Fatalf("cursor row %+v", row)
	}
}
