//go:build !linux && !darwin

package cli

import "errors"

// startBrowser has no opener off Linux and macOS, where guiSession never
// reports a desktop session, so craze auth login chatgpt only prints the URL.
func startBrowser(string) error {
	return errors.New("craze cannot open a browser on this system")
}
