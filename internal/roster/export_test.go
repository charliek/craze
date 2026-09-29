package roster

import (
	"context"
	"net"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// What the package's tests reach of its internals. They are an external test
// package (roster_test) because they run internal/fakehost's hosts, and
// fakehost imports internal/tui, which imports this package.

// The poll's rules.
const (
	TickEvery   = tickEvery
	DialBudget  = dialBudget
	MaxInFlight = maxInFlight
	SavedMax    = savedMax
)

// DialUnix is production's dial.
var DialUnix = dialUnix

// TestOptions are the seams a test sets (options'); a zero field is
// production's, except that there is no registry, index file or peer check
// of the process's: Hosts is the registry, IndexPath "" when nil, and the
// test's hosts, this process's own, are not peer-checked.
type TestOptions struct {
	Ticks      <-chan time.Time
	Now        func() time.Time
	Hosts      func() ([]rundir.Entry, error)
	Dial       func(ctx context.Context, path string) (net.Conn, error)
	IndexPath  func() string
	DialBudget time.Duration
	Ticked     func()
	Applied    func(hostID string, err error)
}

// OpenForTest is Open with to's seams.
func OpenForTest(index Index, to TestOptions) *Roster {
	o := defaults(rundir.Env{})
	o.check = nil
	o.ticks = to.Ticks
	o.hosts = to.Hosts
	o.indexPath = func() string { return "" }
	if to.Now != nil {
		o.now = to.Now
	}
	if to.Dial != nil {
		o.dial = to.Dial
	}
	if to.IndexPath != nil {
		o.indexPath = to.IndexPath
	}
	if to.DialBudget != 0 {
		o.dialBudget = to.DialBudget
	}
	o.ticked, o.applied = to.Ticked, to.Applied
	return open(index, o)
}
