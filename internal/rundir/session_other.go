//go:build !darwin

package rundir

// auditSession has no meaning off macOS: no login session there keeps a
// keychain craze can tell from another, so it is never known.
func auditSession() (flags uint32, gui bool, known bool) { return 0, false, false }
