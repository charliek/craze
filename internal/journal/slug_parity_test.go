package journal_test

import (
	"encoding/json"
	"os"
	"testing"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/journal"
)

// slugFixture is one case of testdata/slug_fixtures.json: a workspace path
// and the directory name both slugs must give it.
type slugFixture struct {
	Name string `json:"name"`
	In   string `json:"in"`
	Want string `json:"want"`
}

// TestSlugMatchesTheHarnessStoreByteForByte is the guard on the duplicated
// slug. depguard keeps the harness from importing a shared helper, so
// internal/journal carries its own copy of the store's algorithm; this test
// (in an external package, which may import the harness where the journal
// itself may not) holds both to the same recorded outputs. A change to
// either copy fails here until the other and the fixtures change with it.
// The fixtures include cuts that land inside 2-, 3- and 4-byte runes, the
// 200-byte edge, roots, Windows-style separators and the empty string.
func TestSlugMatchesTheHarnessStoreByteForByte(t *testing.T) {
	raw, err := os.ReadFile("testdata/slug_fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []slugFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 20 {
		t.Fatalf("only %d slug fixtures; the file is truncated or the wrong one", len(fixtures))
	}
	cutShort := 0
	for _, fx := range fixtures {
		got, theirs := journal.Slug(fx.In), store.Slug(fx.In)
		if got != fx.Want {
			t.Errorf("%s: journal.Slug(%q) = %q, want %q", fx.Name, fx.In, got, fx.Want)
		}
		if theirs != fx.Want {
			t.Errorf("%s: store.Slug(%q) = %q, want %q (the fixtures record the store's rule)", fx.Name, fx.In, theirs, fx.Want)
		}
		if !utf8.ValidString(got) || len(got) > 200 {
			t.Errorf("%s: journal.Slug = %q (%d bytes, valid UTF-8 %v); want at most 200 valid bytes", fx.Name, got, len(got), utf8.ValidString(got))
		}
		// A hashed slug shorter than 200 bytes is one whose cut walked
		// back off a multi-byte rune.
		if len(fx.Want) < 200 && len(fx.Want) > 190 {
			cutShort++
		}
	}
	if cutShort == 0 {
		t.Fatal("no fixture cuts inside a multi-byte rune; the rune-boundary walk is untested")
	}
}
