//go:build !race

package tui

// raceEnabled reports that the race detector is on: a test of a pure,
// single-goroutine function can then do less of the same work.
const raceEnabled = false
