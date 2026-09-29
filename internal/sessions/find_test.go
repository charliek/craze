package sessions

import "testing"

// TestFindNamesARowByItsKeyInAnyWorkspace is Find (plan 030 §3.3): craze serve
// --load <provider>:<sessionId> names a legacy row by the index's own key, so
// the row is found whatever its workspace and whether or not it has a craze
// id; a key the index lacks, a provider the store does not know, and a
// session id under another provider are not found.
func TestFindNamesARowByItsKeyInAnyWorkspace(t *testing.T) {
	legacyIndex(t)
	s := Store{KnownProvider: func(id string) bool { return id != "gone" }}
	row, ok, err := s.Find("cursor", "old")
	if err != nil || !ok {
		t.Fatalf("Find(cursor, old) = %+v, %v, %v", row, ok, err)
	}
	if row.CWD != "/ws" || row.Title != "legacy" || row.CrazeID != "" {
		t.Fatalf("the legacy row read %+v", row)
	}
	if row, ok, _ := s.Find("grok", "new"); !ok || row.CrazeID != "018f-held" {
		t.Fatalf("Find(grok, new) = %+v, %v", row, ok)
	}
	for _, k := range [][2]string{{"cursor", "new"}, {"grok", "old"}, {"cursor", "missing"}, {"gone", "old"}} {
		if row, ok, err := s.Find(k[0], k[1]); ok || err != nil {
			t.Fatalf("Find(%s, %s) = %+v, %v, %v; want none", k[0], k[1], row, ok, err)
		}
	}
}
