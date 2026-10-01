//go:build !linux

package hub

import "time"

// holdThread is Linux's alone (hold_linux_test.go): elsewhere a test that
// needs it skips.
func holdThread(time.Duration) {}
