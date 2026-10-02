//go:build linux || darwin

package cli

import (
	"os/exec"
	"runtime"
	"syscall"
)

// startBrowser opens url in the desktop's browser — `open` on macOS,
// `xdg-open` elsewhere — for craze auth login chatgpt (plan 033 §3.13), which
// calls it only in a desktop session (guiSession) and never under test. It
// does not wait: the opener is started with no stdin and its output
// discarded, in a process group of its own, so the Ctrl-C that cancels the
// sign-in at craze's terminal is never delivered to a browser it started;
// its exit is collected in the background. The URL carries no token.
func startBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
