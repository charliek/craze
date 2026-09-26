package tui

import (
	"context"

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
type engineBackend struct {
	eng    *engine.Engine
	client string
}

var _ backend.Backend = (*engineBackend)(nil)

// newEngineBackend wraps eng, minting this client's id on it.
func newEngineBackend(eng *engine.Engine) *engineBackend {
	return &engineBackend{eng: eng, client: eng.NewClientID()}
}

// engine is the engine this backend wraps. It is for tests alone (engineOf):
// no production path other than setSession — which built the engine and hands
// it to Config.OnEngine — reaches the engine except through the Backend.
func (b *engineBackend) engine() *engine.Engine { return b.eng }

func (b *engineBackend) Start(ctx context.Context) error { return b.eng.Start(ctx) }
func (b *engineBackend) Started(err error)               { b.eng.Started(err) }
func (b *engineBackend) Close() error                    { return b.eng.Close() }
func (b *engineBackend) ClientID() string                { return b.client }

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

func (b *engineBackend) Submit(_ context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	return b.eng.Submit(c, text, mode, fromRow)
}

func (b *engineBackend) Answer(_ context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	return b.eng.Answer(c, id, a)
}

func (b *engineBackend) Unqueue(_ context.Context, c engine.Command, id string) (agent.QueuedPrompt, error) {
	return b.eng.Unqueue(c, id)
}

func (b *engineBackend) EditQueued(_ context.Context, c engine.Command, id, text string, expectedVersion *int) error {
	return b.eng.EditQueued(c, id, text, expectedVersion)
}

func (b *engineBackend) ClearQueue(_ context.Context, c engine.Command) ([]agent.QueuedPrompt, error) {
	return b.eng.ClearQueue(c)
}

func (b *engineBackend) Disarm(_ context.Context, c engine.Command) error { return b.eng.Disarm(c) }

func (b *engineBackend) Interject(ctx context.Context, c engine.Command, text string) error {
	return b.eng.Interject(ctx, c, text)
}

func (b *engineBackend) SetTitle(_ context.Context, c engine.Command, title string) error {
	return b.eng.SetTitle(c, title)
}

func (b *engineBackend) Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error) {
	return b.eng.Set(ctx, c, s)
}

func (b *engineBackend) Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error) {
	return b.eng.Cancel(ctx, c, turn)
}

func (b *engineBackend) CancelSubagent(_ context.Context, c engine.Command, id string) error {
	return b.eng.CancelSubagent(c, id)
}

// Ask waits on nothing in process: the ask registry's own read.
func (b *engineBackend) Ask(_ context.Context, id string) (agent.AskRecord, bool, error) {
	rec, ok := b.eng.Ask(id)
	return rec, ok, nil
}

// Settings is the engine's snapshot's model, mode and config, read now. In
// process it cannot fail.
func (b *engineBackend) Settings(context.Context) (backend.Settings, error) {
	snap := b.eng.State().Snapshot
	return backend.Settings{Model: snap.CurrentModel, Mode: snap.CurrentMode, Config: snap.Config}, nil
}

// State is the transitional live read the mirror still makes (§3.12); C21
// deletes it with the mirror's move onto the fold.
func (b *engineBackend) State() engine.State { return b.eng.State() }

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
