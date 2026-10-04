package rundir

import (
	"os"
	"testing"
)

// The macOS login session (plan 035 P3, SF-126). A process inherits its
// audit (security) session from its parent, and the GUI login's keychain is
// unlocked only inside the GUI login session. A hub first started over ssh,
// and every host it creates, is outside it — and cursor cannot start there.
// GUISession says which session this process is in, so the hub can log it, a
// host whose agent exited can say what may be wrong, and the provider
// availability check can mark cursor unavailable outside it (plan 036 §3.1).
// It does not prove the keychain is locked (a GUI session's can be locked by
// hand; an ssh session's unlocked with security unlock-keychain), so the mark
// refuses only a choice made in a TUI picker — never an explicit --provider
// or $CRAZE_PROVIDER, and never a hub's session.create, which start as they
// always have (plan 036 decision 3).

// AuditFlagGraphicAccess is <bsm/audit_session.h>'s
// AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS: the session has the GUI login's
// graphic access. The mac-mini reads 0x1 (IS_INITIAL alone) over plain ssh
// and 0x2030 (HAS_GRAPHIC_ACCESS, HAS_TTY, HAS_CONSOLE_ACCESS) in its GUI
// login session.
const AuditFlagGraphicAccess = 0x10

// GUISessionEnv forces GUISession in a test child — a process of a test
// binary (testing.Testing), such as a hub or a host a test re-executed — so
// the hub's and the hosts' tests do not depend on the machine they run on:
// "0" is a session without graphic access, as over ssh, and "1" the GUI login
// session, each with the flags the mac-mini reads. Anything else, and every
// process that is not a test binary's, reads the real session.
const GUISessionEnv = "CRAZE_TEST_GUI_SESSION"

// GUISession is this process's audit session: its flags, whether it has the
// GUI login's graphic access (AuditFlagGraphicAccess), and whether that is
// known at all — true on macOS (getaudit_addr), false on every other OS and
// for a call that failed.
func GUISession() (flags uint32, gui bool, known bool) {
	if testing.Testing() {
		switch os.Getenv(GUISessionEnv) {
		case "0":
			return 0x1, false, true
		case "1":
			return 0x2030, true, true
		}
	}
	return auditSession()
}
