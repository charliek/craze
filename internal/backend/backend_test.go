package backend

import (
	"context"
	"errors"
	"testing"
)

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

// TestTheEpochFenceRefusesOnlyAnotherEpoch (plan 027 §3.12, astra r3 13): a
// call whose context carries the epoch the backend is bound to goes; one that
// carries any other is ErrStaleEpoch; one that carries none is not fenced.
func TestTheEpochFenceRefusesOnlyAnotherEpoch(t *testing.T) {
	ctx := context.Background()
	if _, ok := EpochFrom(ctx); ok {
		t.Fatal("a bare context carries an epoch")
	}
	if err := CheckEpoch(ctx, 7); err != nil {
		t.Fatalf("a context with no epoch was fenced: %v", err)
	}
	at := WithEpoch(ctx, 7)
	if e, ok := EpochFrom(at); !ok || e != 7 {
		t.Fatalf("EpochFrom = %d, %v; want 7", e, ok)
	}
	if err := CheckEpoch(at, 7); err != nil {
		t.Fatalf("the current epoch was refused: %v", err)
	}
	for _, current := range []uint64{0, 6, 8} {
		if err := CheckEpoch(at, current); !errors.Is(err, ErrStaleEpoch) {
			t.Fatalf("epoch 7 against current %d: %v, want ErrStaleEpoch", current, err)
		}
	}
	// A derived context keeps the epoch its parent carried.
	child, cancel := context.WithCancel(at)
	defer cancel()
	if err := CheckEpoch(child, 8); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("a derived context lost its epoch: %v", err)
	}
}
