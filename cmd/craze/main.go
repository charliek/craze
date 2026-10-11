package main

import (
	"github.com/charliek/craze/internal/cli"
	"github.com/charliek/craze/internal/hostspawn"
)

func main() {
	// A reparenting spawn's intermediate (plan 041 X32) starts its host and
	// exits here, before the command line is parsed or anything is read,
	// written or locked; in any other craze this returns at once.
	hostspawn.Reparent()
	cli.Execute()
}
