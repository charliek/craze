package tui

import (
	"context"
	"sync/atomic"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// engineBackend is the in-process Backend (plan 027 §3.12): the engine the
// TUI built in setSession, with this model's one client on it. Every method
// forwards to the engine, so a call through it is exactly the engine call it
// replaced — same arguments, same errors, same (lack of) waiting.
//
// The client is minted once, when the engine is wrapped, and never released:
// the in-process client lives as long as its engine (§3.6), as it always has.
//
// Every command and read passes the epoch fence first (fence): a ctx carrying
// an epoch other than this backend's is refused with backend.ErrStaleEpoch
// before the engine is called at all (§3.12, "Chains are fenced in the
// backend too").
type engineBackend struct {
	eng    *engine.Engine
	client string
	// workspace is Info's Workspace: the session's working directory as the
	// TUI resolved it (tui.New's cwd), captured once when the backend is
	// built. The engine itself never holds a workspace — Options carries
	// none, and the session's own agent.Options.Workspace is not read back
	// out of it — so this is the same source the control server's
	// sessionInfo uses for the socket path (s.opts.Workspace, its own
	// configured value rather than a live engine read; control/info.go).
	workspace string
	// epoch is Epoch: inProcessEpoch for the backend's whole life — it is
	// bound to one engine — and never written in production. It is atomic
	// only because a test moves it, from another goroutine, to stand in for
	// PR 4's reconnect to another incarnation (TestAChainStopsAtABackendReplacement).
	epoch atomic.Uint64
}

// inProcessEpoch is every in-process backend's epoch: it names the one engine
// the backend wraps, and nothing can rebind it.
const inProcessEpoch = 1

var _ backend.Backend = (*engineBackend)(nil)

// newEngineBackend wraps eng, minting this client's id on it. workspace is
// Info's Workspace (see the field's doc): the caller's own resolved cwd, not
// read from the engine.
func newEngineBackend(eng *engine.Engine, workspace string) *engineBackend {
	b := &engineBackend{eng: eng, client: eng.NewClientID(), workspace: workspace}
	b.epoch.Store(inProcessEpoch)
	return b
}

// Epoch is the session this backend is bound to: constant in process.
func (b *engineBackend) Epoch() uint64 { return b.epoch.Load() }

// fence is the epoch check every command and read makes before it calls the
// engine (backend.CheckEpoch).
func (b *engineBackend) fence(ctx context.Context) error {
	return backend.CheckEpoch(ctx, b.Epoch())
}

// engine is the engine this backend wraps. It is for the frame harness's
// capture boundary (streamHead) and tests alone (engineOf): no production path
// other than setSession — which built the engine and hands it to
// Config.OnEngine — reaches the engine except through the Backend.
func (b *engineBackend) engine() *engine.Engine { return b.eng }

// engineBehind is the in-process engine b is or wraps: the backend's own, or
// the one a test's wrapper around it names (engine(); V8's jitter backend).
// It is nil for any other backend, and for none. It is for engine's callers.
func engineBehind(b backend.Backend) *engine.Engine {
	switch b := b.(type) {
	case *engineBackend:
		if b == nil {
			return nil
		}
		return b.eng
	case interface{ engine() *engine.Engine }:
		return b.engine()
	}
	return nil
}

func (b *engineBackend) Start(ctx context.Context) error { return b.eng.Start(ctx) }
func (b *engineBackend) Started(err error)               { b.eng.Started(err) }
func (b *engineBackend) Close() error                    { return b.eng.Close() }
func (b *engineBackend) ClientID() string                { return b.client }

// Stop is the engine's close (plan 030 §3.6a): in process the explicit quit
// and the close are one path, today's, unchanged — the session ends with this
// client's program, whoever stops it. The command names nothing the close
// needs: the engine's Close takes none. It is fenced like every command.
func (b *engineBackend) Stop(ctx context.Context, _ engine.Command) error {
	if err := b.fence(ctx); err != nil {
		return err
	}
	return b.eng.Close()
}

// Read reads the engine's primary directly: the session's own stream, into
// which the engine publishes too, so one read carries the agent's events and
// the engine's alike. A closed primary is ErrClosed (the log never closes it
// while a program reads it, but a reader must not take a closed channel for
// an event).
//
// The contract is backend.Backend.Read's: a ctx done before an event is taken
// returns its error and takes nothing, and an event that was taken is always
// returned. The receive and ctx.Done race in the select below, and Go may
// pick the event after the ctx is done; once it has, the event is the answer
// — it is never checked against the ctx again and dropped, because it is
// already off the primary.
func (b *engineBackend) Read(ctx context.Context) (backend.Item, error) {
	if err := ctx.Err(); err != nil {
		return backend.Item{}, err
	}
	select {
	case <-ctx.Done():
		return backend.Item{}, ctx.Err()
	case ev, ok := <-b.eng.Events():
		if !ok {
			return backend.Item{}, backend.ErrClosed
		}
		return backend.Item{Kind: backend.ItemEvent, Event: ev}, nil
	}
}

func (b *engineBackend) Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	if err := b.fence(ctx); err != nil {
		return engine.SubmitResult{}, err
	}
	return b.eng.Submit(c, text, mode, fromRow)
}

func (b *engineBackend) Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	if err := b.fence(ctx); err != nil {
		return err
	}
	return b.eng.Answer(c, id, a)
}

func (b *engineBackend) Unqueue(ctx context.Context, c engine.Command, id string) (agent.QueuedPrompt, error) {
	if err := b.fence(ctx); err != nil {
		return agent.QueuedPrompt{}, err
	}
	return b.eng.Unqueue(c, id)
}

func (b *engineBackend) EditQueued(ctx context.Context, c engine.Command, id, text string, expectedVersion *int) error {
	if err := b.fence(ctx); err != nil {
		return err
	}
	return b.eng.EditQueued(c, id, text, expectedVersion)
}

func (b *engineBackend) ClearQueue(ctx context.Context, c engine.Command) ([]agent.QueuedPrompt, error) {
	if err := b.fence(ctx); err != nil {
		return nil, err
	}
	return b.eng.ClearQueue(c)
}

func (b *engineBackend) Disarm(ctx context.Context, c engine.Command) error {
	if err := b.fence(ctx); err != nil {
		return err
	}
	return b.eng.Disarm(c)
}

func (b *engineBackend) Interject(ctx context.Context, c engine.Command, text string) error {
	if err := b.fence(ctx); err != nil {
		return err
	}
	return b.eng.Interject(ctx, c, text)
}

func (b *engineBackend) SetTitle(ctx context.Context, c engine.Command, title string) error {
	if err := b.fence(ctx); err != nil {
		return err
	}
	return b.eng.SetTitle(c, title)
}

func (b *engineBackend) Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error) {
	if err := b.fence(ctx); err != nil {
		return engine.SetResult{}, err
	}
	return b.eng.Set(ctx, c, s)
}

func (b *engineBackend) Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error) {
	if err := b.fence(ctx); err != nil {
		return engine.CancelResult{}, err
	}
	return b.eng.Cancel(ctx, c, turn)
}

func (b *engineBackend) CancelSubagent(ctx context.Context, c engine.Command, id string) error {
	if err := b.fence(ctx); err != nil {
		return err
	}
	return b.eng.CancelSubagent(c, id)
}

// Ask waits on nothing in process: the ask registry's own read.
func (b *engineBackend) Ask(ctx context.Context, id string) (agent.AskRecord, bool, error) {
	if err := b.fence(ctx); err != nil {
		return agent.AskRecord{}, false, err
	}
	rec, ok := b.eng.Ask(id)
	return rec, ok, nil
}

// Settings is the engine's snapshot's model, mode and config, read now. In
// process it fails only at the epoch fence, which a test alone can move.
func (b *engineBackend) Settings(ctx context.Context) (backend.Settings, error) {
	if err := b.fence(ctx); err != nil {
		return backend.Settings{}, err
	}
	snap := b.eng.State().Snapshot
	return backend.Settings{Model: snap.CurrentModel, Mode: snap.CurrentMode, Config: snap.Config}, nil
}

// LastTurn is State()'s (plan 030 §3.7): read directly, as every in-process
// read is. In process no restore ever comes, so the model never asks it; it
// is here because the socket's is (backend.Backend.LastTurn).
func (b *engineBackend) LastTurn(ctx context.Context) (*engine.LastTurn, error) {
	if err := b.fence(ctx); err != nil {
		return nil, err
	}
	return b.eng.State().LastTurn, nil
}

// Info is the session's static facts (§3.13), read from State().Snapshot's
// static fields and State's own (Incarnation, CrazeSessionID, RetryHorizon).
// It waits on nothing: State() reads the session's snapshot outside the
// engine's mutex and merges the engine's own fields in only briefly under it
// (engine/state.go's State doc), exactly what Settings above already reads on
// every model-change chain step, so a call here is no new way to block. Before
// Start it answers from the session's initial snapshot — the configured
// provider, an empty ProviderSessionID and empty catalogs — as the TUI reads
// it today (GLM 11). PermissionMode and StartedAt are left unsaid (plan 030
// §3.7): the engine holds neither, and in process both are the TUI's own —
// its config's --force, and its own start — which is what the model falls
// back to for a host that does not say.
func (b *engineBackend) Info() backend.SessionInfo {
	st := b.eng.State()
	return backend.SessionInfo{
		CrazeSessionID:    st.CrazeSessionID,
		ProviderSessionID: st.SessionID,
		Incarnation:       st.Incarnation,
		Workspace:         b.workspace,
		Provider:          st.Provider.Name,
		Label:             st.Provider.Label(),
		Capabilities:      st.Provider.Capabilities(),
		Models:            st.Models,
		Modes:             st.Modes,
		RetryHorizon:      st.RetryHorizon,
	}
}

// settingsSnapshot is the snapshot a model-change chain judges its next step
// on: the settings the backend holds now, read in the chain's tea.Cmd, with
// the provider the chain captured when it was dispatched in the Update. What
// an option means — which one is the effort, which the fast toggle — is the
// provider's local vocabulary (agent.EffortOption, agent.FastOption,
// catalogTabs), and the rest of what the chains read is the current model and
// the config; for those reads it is the engine's own State().Snapshot.
func settingsSnapshot(s backend.Settings, p agent.ProviderInfo) agent.Snapshot {
	return agent.Snapshot{CurrentModel: s.Model, CurrentMode: s.Mode, Config: s.Config, Provider: p}
}
