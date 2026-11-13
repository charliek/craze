package engine

import (
	"strings"
	"testing"
)

// TestAHostsWebSearchChoiceIsOnEveryRowItWrites is plan 051 F.3's record at
// the engine: a host whose new session was given --web-search or
// --no-web-search (IndexOptions.WebSearch, craze serve) puts the choice on
// every row it writes, attended or not, without marking it unattended; a host
// given neither writes none, so the index's set-only rule is all that decides
// the record.
func TestAHostsWebSearchChoiceIsOnEveryRowItWrites(t *testing.T) {
	for _, want := range []string{"on", "off", ""} {
		t.Run("webSearch "+want, func(t *testing.T) {
			idx := newFakeIndex()
			r := newRig(t, Options{CrazeSessionID: "018f-the-thread", Index: IndexOptions{
				Store: idx, CWD: "/w", Provider: "native", WebSearch: want,
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
				if row.WebSearch != want || row.Unattended != nil {
					t.Fatalf("a row carried webSearch %q, unattended %+v; want %q and none: %+v", row.WebSearch, row.Unattended, want, rows)
				}
			}
		})
	}
}
