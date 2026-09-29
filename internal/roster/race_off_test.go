//go:build !race

package roster_test

// raceEnabled says the race detector is on: the scale test's CPU bound is
// not asserted under it (scale_test.go).
const raceEnabled = false
