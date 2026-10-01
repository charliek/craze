//go:build linux

package rundir

import (
	"path/filepath"
	"slices"
	"testing"
)

// On Linux, with the sweep as it ships (sweepSockets untouched, and not read
// here), an old refusing host socket with no lock is removed: the mirror of
// Darwin's TestTheDarwinSweepLeavesAnOldRefusingSocket, so the setting each
// platform ships with is checked against an expectation of its own.
func TestTheLinuxSweepRemovesAnOldRefusingSocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	p := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	listenAt(t, p)
	r := sweepOrphans(t, env, swept())
	if exists(t, p) || !slices.Equal(r.Sockets, []string{p}) {
		t.Fatalf("the sweep on Linux left the old refusing socket %s (report %#v)", p, r)
	}
}
