package rundir

// The waiting commands' full results (plan 041 §3.1, P13):
// <Home>/.cache/craze/results, beside the registry, the session locks, the
// host logs and the catalog cache, and validated as they are. `craze wait
// --max-message N` (and new --wait, send --wait) prints the head of a long
// message and plan and writes the whole result here, one <short>-<turn>.md a
// turn, for a caller whose output is cut (Claude Code's Bash truncates near
// 30,000 characters). It is keyed by HOME, as the rest of the cache tree is.

// resultsName is the results' directory under the cache tree.
const resultsName = "results"

// ResultsDir is the results' directory, <Home>/.cache/craze/results,
// validated exactly as the rest of the cache tree is (cacheDir: every
// component walked from "/" by descriptor, never through a link; the craze
// tree's leaves the euid's own and 0700) and made 0700 — with the tree above
// it — where it is missing. It answers the directory's canonical path, which
// only this user can write in, so a file written there by name is this
// user's own (HostLogDir's rule).
func ResultsDir(env Env) (string, error) {
	d, err := env.cacheDir(resultsName, true)
	if err != nil {
		return "", err
	}
	defer func() { _ = d.close() }()
	return d.path, nil
}
