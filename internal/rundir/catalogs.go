package rundir

// The model catalog cache (plan 030 §3.14): <Home>/.cache/craze/catalogs,
// beside the registry, the session locks and the host logs, and validated as
// they are. A detached host records the model catalog its ACP agent installed
// there, one <provider>.json per provider under a <provider>.lock of its own
// (internal/modelcache), so the session list's /model can offer a provider's
// models before any session of it has started — an ACP catalog exists only
// once session/new has answered (discovery data.md §5). It is keyed by HOME,
// as the registry is, never by CRAZE_HOME: a catalog is the account's, and a
// relocated craze directory is still the same user's agents.

// catalogsName is the catalog cache's directory under the cache tree.
const catalogsName = "catalogs"

// CatalogDir is the catalog cache's directory, <Home>/.cache/craze/catalogs,
// validated exactly as the rest of the cache tree is (cacheDir: every
// component walked from "/" by descriptor, never through a link; the craze
// tree's leaves the euid's own and 0700), and made 0700 — with the tree above
// it — where it is missing when create is set. It answers the directory's
// canonical path, which only this user can write in, so a file opened there by
// name is this user's own (HostLogDir's rule). Without create, a directory
// that is not there is an error wrapping fs.ErrNotExist: nothing has been
// cached, and a reader has nothing to read.
func CatalogDir(env Env, create bool) (string, error) {
	d, err := env.cacheDir(catalogsName, create)
	if err != nil {
		return "", err
	}
	defer func() { _ = d.close() }()
	return d.path, nil
}
