package engine

import "github.com/charliek/craze/internal/agent"

// PublishesSlashCatalog reports whether the session publishes the slash
// catalog section (plan 043 P2, D5): it is an agent.SlashCataloger whose
// PublishesSlashCatalog says so — the live session and the native session.
// It is a property of the session the engine was built over, fixed for the
// engine's life and true from before the session's start (the install that
// first publishes the section comes later, and a client that has none yet
// builds its menu as it would without one) — the session capability
// slashCatalog on the wire, and the TUI's in-process Info alike — and it
// waits on nothing.
func (e *Engine) PublishesSlashCatalog() bool {
	c, ok := e.sess.(agent.SlashCataloger)
	return ok && c.PublishesSlashCatalog()
}
