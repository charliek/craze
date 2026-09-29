package engine

import (
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// HostOptions is the Options a host builds a session's engine with (plan 030
// §3.3): the one place they are spelled, for every host there is — the TUI's
// own setSession, the frame harness's socket host (plan 027 §3.16), which
// builds its engine exactly as the TUI would have, and craze serve, the
// headless host, which must build the very engine the TUI would have built in
// its place. It lived in internal/tui (engineOptions) until craze serve needed
// it: a host that is not a TUI takes it from here, and the engine still knows
// no client (plan 021 §3.1) — the two rules it carries, UnindexedProvider and
// IndexTitleLine, are about the index and the provider registry, never about a
// screen.
//
// crazeID is the durable craze session id the session already has ("" mints
// one): a loaded row's, which the host claimed before it built anything. index
// is the session index (nil persists nothing). cwd is the absolute workspace
// the rows are keyed by, and provider the resolved default's name — the
// provider the session was started as, recorded until the session reports one
// of its own.
//
// The zero ChainPolicy is every host's: Esc stops a turn and the queue behind
// it carries on, and a prompt the session refuses is shown as the refusal it is
// rather than waited out (ChainPolicy).
func HostOptions(crazeID string, index Index, cwd, provider string) Options {
	return Options{
		CrazeSessionID: crazeID,
		Index: IndexOptions{
			Store: index,
			CWD:   cwd,
			// The provider a row is recorded under before the session has
			// answered with one of its own: the resolved default it was
			// started as.
			Provider: provider,
			// A provider craze cannot load again stays out of the index
			// (plan 028 §3.5): unresumable, which is not the same question
			// as hidden — before D-65 listed it, native was hidden and
			// indexed.
			Unindexed: UnindexedProvider,
			TitleLine: IndexTitleLine,
		},
	}
}

// UnindexedProvider reports whether id resolves to a provider whose sessions
// craze cannot load again, which the engine then never writes a row for
// (IndexOptions.Unindexed), so --continue and --resume never offer one (plan
// 028 §3.5). It is the TUI's hiddenProvider's twin for the session index: the
// two questions used to be one, and native was where they first parted —
// hidden, yet resumable — until D-65 listed it too. An id the registry does not
// know is not refused here, as it is not by hiddenProvider: a session only ever
// reports its own provider's id.
func UnindexedProvider(id string) bool {
	p, err := agent.ProviderByName(id)
	return err == nil && !p.Resumable()
}

// IndexTitleRunes is how long a session title may be in the index. Runes, not
// bytes: the cap exists so a picker row is a row, and a prompt is as likely to
// open in Japanese as in ASCII. The TUI's /rename caps a title it is about to
// ask for at the same number (its titleRuneCap), and a test holds the two
// together.
const IndexTitleRunes = 120

// IndexTitleLine folds a title onto the one line a session-index row holds and
// caps it at IndexTitleRunes: what every row's title is written through
// (IndexOptions.TitleLine), whichever host wrote it. The fold is the TUI's own
// sanitizeLine, which internal/transcript carries transcribed so that nothing
// below the clients imports a terminal library (transcript.SanitizeLine, plan
// 024 §3.1); a drift guard in internal/tui holds this against the TUI's own
// sanitizeLine and cap, byte for byte, so a row one host writes is exactly the
// row another would have.
func IndexTitleLine(title string) string {
	return capRunes(transcript.SanitizeLine(title), IndexTitleRunes)
}

// capRunes is s cut to at most n runes.
func capRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
