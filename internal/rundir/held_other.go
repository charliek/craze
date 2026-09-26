//go:build !linux

package rundir

// ancestorFlags opens a cache-tree ancestor on the walk (walk). Darwin has no
// O_PATH, and golang.org/x/sys/unix no O_SEARCH, so an ancestor is opened
// dirFlags, read-only: the one residual is that a search-only ancestor (0711,
// another user's) is refused there, which is rare — /Users is 0755.
const ancestorFlags = dirFlags
