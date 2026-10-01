//go:build darwin

package rundir

import (
	"path/filepath"
	"testing"
)

// On Darwin, with the sweep as it ships (sweepSockets untouched, and not read
// here), an old refusing host socket with no lock survives the sweep: XNU
// refuses a connect to a live listener whose backlog is full as it refuses
// one to a dead socket, so a refusal there proves nothing.
func TestTheDarwinSweepLeavesAnOldRefusingSocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	p := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	listenAt(t, p)
	r := sweepOrphans(t, env, swept())
	if !isSocket(t, p) || len(r.Sockets) != 0 {
		t.Fatalf("the sweep on Darwin removed the old refusing socket %s (report %#v)", p, r)
	}
}
