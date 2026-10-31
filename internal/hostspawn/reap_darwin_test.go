package hostspawn

import (
	"bytes"
	"fmt"

	"golang.org/x/sys/unix"
)

// environState says how pid's environment reads, never what is in it, for a failure.
func environState(pid int) string {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	path := ""
	if len(b) > 4 {
		if i := bytes.IndexByte(b[4:], 0); i >= 0 {
			path = string(b[4 : 4+i])
		}
	}
	return fmt.Sprintf("kern.procargs2 %d bytes of %q (%v)", len(b), path, err)
}
