package engine_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// TestAnAttachSucceedsWhateverTheSlashCatalogWeighs (plan 043 review W4): the
// slash catalog never makes an attach fail that succeeds without it. A session
// whose agent advertises nine commands with 240 KiB descriptions attaches with
// the default 4 MiB snapshot budget whether or not it publishes the catalog —
// the catalog repeats every description, which with it would take the
// mandatory state over the budget — and the snapshot of the one that does
// says the catalog was left out (settings.truncated.slash, no settings.slash)
// while carrying the commands whole. Negative control: transcript's
// cut.snapshot without its retry fails the publishing session's Attach with
// ErrSnapshotTooLarge.
func TestAnAttachSucceedsWhateverTheSlashCatalogWeighs(t *testing.T) {
	cmds := make([]agent.CommandInfo, 9)
	for i := range cmds {
		cmds[i] = agent.CommandInfo{Name: fmt.Sprintf("cmd%d", i), Description: strings.Repeat(string(rune('a'+i)), 240<<10)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	for _, publish := range []bool{false, true} {
		stub := tui.NewStubNoPrimary()
		stub.InstallOnStart = true
		stub.PublishSlash = publish
		e, err := engine.New(stub, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		stub.SetCommands(cmds)
		if _, err := e.SyncSeq(ctx); err != nil {
			t.Fatal(err)
		}
		a, err := e.Attach(ctx, engine.AttachOptions{})
		if err != nil {
			t.Fatalf("publishing the catalog %v: Attach: %v", publish, err)
		}
		a.Sub.Close()
		st := a.Snapshot.Settings
		if !reflect.DeepEqual(st.Commands, cmds) || st.Truncated.Commands || st.Slash != nil || st.Truncated.Slash != publish {
			t.Fatalf("publishing the catalog %v: %d commands (marked %v), %d slash rows, slash marked %v",
				publish, len(st.Commands), st.Truncated.Commands, len(st.Slash), st.Truncated.Slash)
		}
		if publish && len(stub.Snapshot().Slash) == 0 {
			t.Fatal("fixture: the publishing session holds no catalog")
		}
	}
}
