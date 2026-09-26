//go:build linux

package rundir

import "golang.org/x/sys/unix"

// ancestorFlags opens a cache-tree ancestor on the walk (walk): O_PATH, which
// needs no permission on the directory itself — only search on the one it is
// looked up in — so an ancestor its owner made search-only for others (a
// root-owned 0711 /home) is walked as lstat walked it. fstat and the *at calls
// relative to an O_PATH descriptor work as they do on any other; the leaves,
// which craze lists, reads and fchmods, are opened dirFlags. Never through a
// symlink at its own name; openat adds O_CLOEXEC.
const ancestorFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW
