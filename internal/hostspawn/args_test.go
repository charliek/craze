package hostspawn

import (
	"slices"
	"strings"
	"testing"
)

// TestArgsPassTheStartSettingsOnlyWhenSet: --effort and --fast/--no-fast
// (plan 032 §3.11) reach craze serve's command line only when the spawner set
// them — "" and nil leave the host the provider's own defaults — and an effort
// that looks like a flag stays a value. internal/cli's
// TestSpawnArgvCarriesEverySessionFlag parses the result with serve's own
// flag set.
func TestArgsPassTheStartSettingsOnlyWhenSet(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name     string
		spec     Spec
		want     []string
		dontWant []string
	}{
		{"neither", Spec{}, nil, []string{"--effort=", "--fast", "--no-fast"}},
		{"effort and fast", Spec{Effort: "-high", Fast: &on}, []string{"--effort=-high", "--fast"}, []string{"--no-fast"}},
		{"no-fast", Spec{Fast: &off}, []string{"--no-fast"}, []string{"--effort=", "--fast"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := Args(tc.spec)
			for _, a := range tc.want {
				if !slices.Contains(argv, a) {
					t.Fatalf("%q lacks %s", argv, a)
				}
			}
			for _, a := range tc.dontWant {
				if slices.ContainsFunc(argv, func(x string) bool { return x == a || strings.HasSuffix(a, "=") && strings.HasPrefix(x, a) }) {
					t.Fatalf("%q passes %s", argv, a)
				}
			}
		})
	}
}
