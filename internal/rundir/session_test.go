package rundir

import (
	"runtime"
	"testing"
)

// TestGUISessionsTestSeam (plan 035 P3): in a test binary GUISessionEnv
// forces the answer — "0" a session without the GUI login's graphic access,
// "1" the GUI login session, both known, with the flags the mac-mini reads —
// on every OS; anything else reads the real session, which off macOS is
// never known.
func TestGUISessionsTestSeam(t *testing.T) {
	for _, tc := range []struct {
		value       string
		flags       uint32
		gui, forced bool
	}{
		{"0", 0x1, false, true},
		{"1", 0x2030, true, true},
		{"", 0, false, false},
		{"yes", 0, false, false},
	} {
		t.Setenv(GUISessionEnv, tc.value)
		flags, gui, known := GUISession()
		if tc.forced {
			if flags != tc.flags || gui != tc.gui || !known {
				t.Fatalf("%s=%q: GUISession = %#x, %v, %v; want %#x, %v, true", GUISessionEnv, tc.value, flags, gui, known, tc.flags, tc.gui)
			}
			if gui != (flags&AuditFlagGraphicAccess != 0) {
				t.Fatalf("%s=%q: flags %#x disagree with gui %v", GUISessionEnv, tc.value, flags, gui)
			}
			continue
		}
		if runtime.GOOS != "darwin" && (flags != 0 || gui || known) {
			t.Fatalf("%s=%q on %s: GUISession = %#x, %v, %v; want unknown", GUISessionEnv, tc.value, runtime.GOOS, flags, gui, known)
		}
	}
}
