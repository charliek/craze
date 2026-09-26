package backend

import "testing"

// TestTheZeroItemCarriesNoKind: a Read that fails answers the zero Item beside
// its error, and a caller that looked at the item before the error must not
// find an event in it. Every kind is therefore non-zero, and distinct.
func TestTheZeroItemCarriesNoKind(t *testing.T) {
	var zero Item
	seen := map[ItemKind]bool{}
	for _, k := range []ItemKind{ItemEvent, ItemReady, ItemRestore, ItemEnd} {
		if k == zero.Kind {
			t.Fatalf("kind %d is the zero Item's", k)
		}
		if seen[k] {
			t.Fatalf("kind %d is named twice", k)
		}
		seen[k] = true
	}
}
