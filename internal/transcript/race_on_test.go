//go:build race

package transcript

// raceEnabled reports that the race detector is on, which changes what the
// allocation test can measure.
const raceEnabled = true
