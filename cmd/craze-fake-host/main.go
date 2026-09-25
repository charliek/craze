// craze-fake-host is the standalone twin of internal/fakehost (plan 027
// §3.11): it serves the real control socket wire, deterministically, in
// front of a Stub instead of a real agent. It is a thin main over
// internal/fakehost — everything that matters lives there.
//
// Usage:
//
//	craze-fake-host --socket PATH
//
// It prints one ready line to stdout, {"socket", "sessionId", "hostId"}, then
// reads NDJSON ops from stdin, one per line ({"name": "...", ...} —
// internal/fakehost's Host.Do), and exits once stdin reaches EOF or an op
// named "quit" runs.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/charliek/craze/internal/fakehost"
)

func main() {
	socket := flag.String("socket", "", "unix socket path to serve")
	flag.Parse()
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "craze-fake-host: --socket is required")
		os.Exit(2)
	}
	if err := run(*socket); err != nil {
		fmt.Fprintln(os.Stderr, "craze-fake-host:", err)
		os.Exit(1)
	}
}

func run(socket string) error {
	h, err := fakehost.New(fakehost.Options{})
	if err != nil {
		return err
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()

	ready := struct {
		Socket    string `json:"socket"`
		SessionID string `json:"sessionId"`
		HostID    string `json:"hostId"`
	}{Socket: socket, SessionID: h.SessionID(), HostID: h.HostID()}
	line, err := json.Marshal(ready)
	if err != nil {
		return err
	}
	if _, err := fmt.Println(string(line)); err != nil {
		return err
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	quit := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			fmt.Fprintln(os.Stderr, "craze-fake-host: op:", err)
			continue
		}
		if err := h.Do(line); err != nil {
			fmt.Fprintln(os.Stderr, "craze-fake-host: op:", err)
		}
		if probe.Name == "quit" {
			quit = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "craze-fake-host: stdin:", err)
	}
	if !quit {
		_ = h.Close(context.Background())
	}
	return <-served
}
