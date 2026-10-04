package engine_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// TestRefreshModelsIsTheSessionsDoor (plan 034 §3.4, Q17; A25 in process): a
// session with no models to refresh while it runs — the plain Stub, as an
// ACP session — is answered unsupported, with no error, and RefreshesModels
// says false; one that has (tui.RefreshingStub, as a native session) answers
// with its own refresh — current with nothing staged, applied with its list
// and revision once something is — and RefreshesModels says true. Either is
// not_accepting before its start and after its close: nothing is asked of the
// session then. Negative control: a door with no gate answers before the
// start (unsupported, or the session's current) instead of not_accepting.
func TestRefreshModelsIsTheSessionsDoor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh bool
	}{{"an ACP-like session", false}, {"a native-like session", true}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := tui.NewStubNoPrimary()
			stub.SetNativeDir("/n")
			var sess agent.Session = stub
			if tc.refresh {
				sess = tui.RefreshingStub{Stub: stub}
			}
			e, err := engine.New(sess, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			if e.RefreshesModels() != tc.refresh {
				t.Fatalf("RefreshesModels() = %v, want %v", e.RefreshesModels(), tc.refresh)
			}
			ctx := context.Background()
			if _, err := e.RefreshModels(ctx, ""); !errors.Is(err, engine.ErrNotAccepting) {
				t.Fatalf("before the start: %v; want ErrNotAccepting", err)
			}
			if err := e.Start(ctx); err != nil {
				t.Fatal(err)
			}
			stub.StageModels([]agent.ModelInfo{{ID: "native/new", Name: "New"}})
			r, err := e.RefreshModels(ctx, "/n")
			switch {
			case err != nil:
				t.Fatalf("the refresh: %v", err)
			case !tc.refresh && (r.Status != agent.ModelsUnsupported || r.SameDir != nil):
				t.Fatalf("the refresh: %+v; want unsupported and nothing else", r)
			case tc.refresh && (r.Status != agent.ModelsApplied || r.Revision != 1 || r.SameDir == nil || !*r.SameDir):
				t.Fatalf("the refresh: %+v; want applied at revision 1, sameDir true", r)
			}
			if tc.refresh {
				st := e.State()
				if !reflect.DeepEqual(st.Models, []agent.ModelInfo{{ID: "native/new", Name: "New"}}) || st.CatalogRevision != 1 {
					t.Fatalf("the state after it: %+v at revision %d", st.Models, st.CatalogRevision)
				}
				if r, err := e.RefreshModels(ctx, ""); err != nil || r.Status != agent.ModelsCurrent || r.Revision != 1 {
					t.Fatalf("a second refresh: %+v, %v; want current at revision 1", r, err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := e.RefreshModels(ctx, ""); !errors.Is(err, engine.ErrNotAccepting) {
				t.Fatalf("after the close: %v; want ErrNotAccepting", err)
			}
		})
	}
}
