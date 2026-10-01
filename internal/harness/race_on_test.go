//go:build race

package harness

// raceEnabled reports that the race detector is on, which a test whose cost
// is its size alone, with nothing concurrent in it, scales itself down for.
const raceEnabled = true
