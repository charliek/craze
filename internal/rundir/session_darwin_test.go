//go:build darwin

package rundir

import (
	"testing"
	"unsafe"
)

// TestGUISessionIsKnownOnMacOS (plan 035 P3): getaudit_addr answers for this
// process, so the session is known, and gui is exactly the graphic-access
// flag. Which session it is depends on how the test was started — 0x1-ish
// over plain ssh, 0x2030-ish in the GUI login session — so it is logged, not
// asserted. The struct the call fills is <bsm/audit.h>'s 48 bytes.
func TestGUISessionIsKnownOnMacOS(t *testing.T) {
	t.Setenv(GUISessionEnv, "")
	if n := unsafe.Sizeof(auditinfoAddr{}); n != 48 {
		t.Fatalf("auditinfoAddr is %d bytes, want struct auditinfo_addr's 48", n)
	}
	flags, gui, known := GUISession()
	if !known {
		t.Fatal("GUISession is not known on macOS: getaudit_addr failed")
	}
	if gui != (flags&AuditFlagGraphicAccess != 0) {
		t.Fatalf("flags %#x disagree with gui %v", flags, gui)
	}
	t.Logf("macOS login session: gui %v (audit flags %#x)", gui, flags)
}
