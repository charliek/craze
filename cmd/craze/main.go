package main

import (
	"os"

	"github.com/charliek/craze/internal/cli"
	"github.com/charliek/craze/internal/hostspawn"
)

func main() {
	// Before anything can start a process: a native command's own mark is its
	// host's, and goes no further than what that command runs that is not craze.
	hostspawn.ClearCommandMark()
	// A reparenting spawn's intermediate (plan 041 X32) starts its host and
	// exits here, before the command line is parsed or anything is read,
	// written or locked; in any other craze this returns at once. Never for
	// craze models --shipped (plan 050 C2), which reads no environment, so a
	// stray mark cannot fail it; no intermediate runs that command line.
	if !cli.ShippedModelsRequest(os.Args[1:]) {
		hostspawn.Reparent()
	}
	cli.Execute()
}
