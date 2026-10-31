package hostspawn

import (
	"fmt"
	"os"
	"strconv"
)

// environState says how pid's environment reads, never what is in it, for a failure.
func environState(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	return fmt.Sprintf("environ %d bytes (%v)", len(b), err)
}
