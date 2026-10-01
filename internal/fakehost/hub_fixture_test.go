package fakehost

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// The two-socket fixtures' hub (plan 032 §3.15; fixtures 19 and 20): the
// real hub, internal/hub's Run, in this process over the fixture's own
// registry — the production hub but for its idle grace, which is long enough
// never to end a fixture. Its id, version and this process's pid are named by
// placeholder in what it writes (fixtureRunner.hubToWire).

func init() { fixtureHub = startFixtureHub }

// startFixtureHub runs a hub over env and answers its socket and its id, from
// its ready line; it is stopped — and its Run waited for — when the test ends.
func startFixtureHub(t *testing.T, env rundir.Env) (string, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- hub.Run(ctx, hub.Options{
			Env:       env,
			Codecs:    protocol.Codecs{Event: agent.EventCodecVersion, Snapshot: transcript.SnapshotVersion},
			Ready:     hub.NewReadyPipe(w),
			IdleGrace: time.Hour,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(fixtureTimeout):
			t.Errorf("the fixture's hub did not stop within %v", fixtureTimeout)
		}
		_ = r.Close()
	})
	raw := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r) // one line, then the pipe closed
		raw <- b
	}()
	var line hub.ReadyLine
	select {
	case b := <-raw:
		if err := json.Unmarshal(b, &line); err != nil || !line.OK {
			t.Fatalf("the fixture's hub did not come up: %q (%v)", b, err)
		}
	case <-time.After(fixtureTimeout):
		t.Fatalf("the fixture's hub wrote no ready line within %v", fixtureTimeout)
	}
	return line.Socket, line.HubID
}
