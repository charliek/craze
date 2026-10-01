package rundir

import "path/filepath"

// SocketInNamespace reports whether socket — a host's control socket, as its
// registry entry names it (Entry.Socket) — lies in crazeDir's namespace: the
// socket tree is <base>/<ns>/<hostId>.sock, and ns is Namespace(crazeDir)
// (trees.go). That is a host started under the same CRAZE_HOME as the
// caller, so one that reads the same craze directory: plan 033's P27, where a
// TUI makes image chips only for a session whose host can read the TUI's own
// attachments directory. A craze directory Namespace refuses is in no
// namespace, and an empty socket names none.
//
// It is a reading of the path, not of the disk: the namespace directory is a
// leaf craze made, never a symlink, and Entry.Socket is canonical. Two craze
// directories whose namespaces collide (8 hex digits of a SHA-256) would read
// as one; the host's own confined read still refuses a path outside its
// attachments directory, so the cost is path text, not a wrong file.
func SocketInNamespace(socket, crazeDir string) bool {
	if socket == "" {
		return false
	}
	ns, err := Namespace(crazeDir)
	if err != nil {
		return false
	}
	return filepath.Base(filepath.Dir(filepath.Clean(socket))) == ns
}
