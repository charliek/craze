package hostspawn

import (
	"testing"

	"github.com/charliek/craze/internal/rundir"
)

func TestReviewDetachedProcessOrigin(t *testing.T) {
	scope := ReviewScope{
		before: map[int]uint64{1: 1, 10: 2, 20: 3}, sessions: map[int]bool{1: true, 10: true, 20: true},
		adopters: map[int]bool{1: true, 10: true}, known: map[int]uint64{50: 9},
	}
	host := Child{pid: 30, ownStart: 7}
	members := map[int]rundir.GroupMember{
		1: {PID: 1, Start: 1, Session: 1}, 10: {PID: 10, Start: 2, Session: 10}, 20: {PID: 20, Start: 3, Session: 20},
		31: {PID: 31, Start: 8, Session: 30}, 50: {PID: 50, Start: 9, Session: 50, PPID: 1},
	}
	for _, tc := range []struct {
		parent           int
		owned, uncertain bool
	}{
		{31, true, false}, {50, true, false}, {20, false, false}, {1, false, true}, {10, false, true}, {42, false, true},
	} {
		helper := rundir.GroupMember{PID: 60, Start: 10, Session: 60, PPID: tc.parent}
		owned, uncertain := scope.origin(helper, members, &host)
		if owned != tc.owned || uncertain != tc.uncertain {
			t.Errorf("parent %d: owned=%t uncertain=%t", tc.parent, owned, uncertain)
		}
	}
	// Reusing a known child PID cannot establish ownership.
	reused := rundir.GroupMember{PID: 50, Start: 20, Session: 50, PPID: 1}
	if owned, uncertain := scope.origin(reused, members, &host); owned || !uncertain {
		t.Fatal(owned, uncertain)
	}
}
