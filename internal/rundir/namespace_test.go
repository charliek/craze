package rundir

import (
	"path/filepath"
	"testing"
)

// TestSocketInNamespace: a socket under <base>/<ns>/ is in the namespace of
// the craze directory ns was made from, and in no other's (plan 033 P27).
func TestSocketInNamespace(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".craze")
	other := filepath.Join(t.TempDir(), "elsewhere")
	ns, err := Namespace(home)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join("/run/user/1000/craze", ns, "h-0123.sock")
	for _, tc := range []struct {
		name     string
		socket   string
		crazeDir string
		want     bool
	}{
		{"the same craze directory", sock, home, true},
		{"the same, spelled with a trailing slash", sock, home + "/", true},
		{"another craze directory", sock, other, false},
		{"no craze directory", sock, "", false},
		{"no socket", "", home, false},
		{"a socket outside any namespace directory", "/tmp/czg-1/s", home, false},
	} {
		if got := SocketInNamespace(tc.socket, tc.crazeDir); got != tc.want {
			t.Errorf("%s: SocketInNamespace(%q, %q) = %v, want %v", tc.name, tc.socket, tc.crazeDir, got, tc.want)
		}
	}
}
