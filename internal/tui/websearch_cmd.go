package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/websearch"
)

// /websearch (issues #137, #141): a native session's web search at a glance,
// and the remembered choice for new sessions.
//
//   - Bare, it adds a few note rows: the provider, this session's consent and
//     key as its last web status section says them (agent.WebStatusState,
//     published at every turn's start and after every web call), its call
//     totals, and what new sessions do — config.toml first, then the
//     remembered choice (websearch.json in the native directory).
//   - /websearch on, off and ask change the remembered choice: allowed
//     through Exa, disabled, or forgotten so the next session asks. They
//     change new sessions only; this one keeps the tools it opened with. A
//     config.toml websearch setting still wins, and the row says so.
//
// It needs no agent, so it runs mid-turn (runsWhileWorking).

// websearchBuiltin is /websearch's row: in the slash catalog on a native
// session alone, as /connect's is (the table's slashNative gate).
var websearchBuiltin = tableBuiltin("websearch")

// webSearchUsage is the command's usage line.
const webSearchUsage = "usage: /websearch [on|off|ask]"

// runWebSearch is /websearch [on|off|ask].
func (m Model) runWebSearch(args string) (tea.Model, tea.Cmd) {
	m.input.SetValue("")
	if !m.connectOffered() {
		m.addError("/websearch: web search is a native session feature")
		return m, nil
	}
	if m.nativeDir == "" {
		m.addError("/websearch: no native directory to read or write " + websearch.StateFile + " in")
		return m, nil
	}
	path := filepath.Join(m.nativeDir, websearch.StateFile)
	cfg := ConfigWebSearch()
	var (
		choice websearch.Choice
		err    error
		said   string
	)
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "":
		for _, line := range webSearchPanel(m.webStatus(), cfg, path) {
			m.addNote(line)
		}
		return m, nil
	case "on":
		choice, said = websearch.Choice{Provider: websearch.DefaultProvider}, "web search: new sessions search the web through Exa without asking"
		err = websearch.WriteChoice(path, choice)
	case "off":
		choice, said = websearch.Choice{Disabled: true}, "web search: new sessions have no web tools"
		err = websearch.WriteChoice(path, choice)
	case "ask":
		said = "web search: nothing remembered; the next session asks on its first web call"
		if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	default:
		m.addError(webSearchUsage)
		return m, nil
	}
	if err != nil {
		m.addError("/websearch: " + websearch.StateFile + " could not be written; nothing changed")
		return m, nil
	}
	m.addNote(said + " (" + webSearchScope(m.remote, path) + ")")
	if over := configOverride(cfg); over != "" {
		m.addNote("web search: " + over + ", which wins over this")
	}
	return m, nil
}

// webSearchScope is what /websearch on, off or ask changed, for its note: a
// session that has decided keeps its choice and one still undecided adopts
// the new one at its next web call; and, viewing a session served elsewhere,
// that only this machine's file changed — a host on another machine reads its
// own.
func webSearchScope(remote bool, path string) string {
	scope := "a session that has decided keeps its choice; one still undecided adopts this at its next web call"
	if remote {
		scope = "this machine's " + path + " only: a session host elsewhere reads its own; " + scope
	}
	return scope
}

// webStatus is the session's last web status section, nil before one.
func (m Model) webStatus() *agent.WebStatusState { return m.snap.WebStatus }

// configOverride is what config.toml says of web search when it says
// anything that wins over the remembered choice, "" when it says nothing.
func configOverride(cfg agent.WebConfig) string {
	switch cfg.Setting {
	case agent.WebSettingOn:
		return "config.toml turns web search on ([websearch] provider = " + cfg.Provider + ")"
	case agent.WebSettingOff:
		return "config.toml turns web search off (websearch = false)"
	case agent.WebSettingInvalid:
		return "config.toml's websearch setting is not valid, so web search is off"
	}
	return ""
}

// webSearchPanel is the bare command's rows: st the session's last web
// status (nil before any), cfg config.toml's reading, and path the remembered
// choice's file.
func webSearchPanel(st *agent.WebStatusState, cfg agent.WebConfig, path string) []string {
	const pad = "  %-14s %s"
	rows := []string{"Web search · Exa (exa.ai)"}
	row := func(name, value string) { rows = append(rows, fmt.Sprintf(pad, name, value)) }
	if st == nil {
		row("This session", "no web status yet: it is sent at the first turn, when the session offers web tools")
	} else {
		row("This session", sessionConsent(st.Consent))
		row("Key", keySource(st.Key))
		if st.Searches+st.Fetches == 0 {
			row("Calls", "none yet")
		} else {
			row("Calls", fmt.Sprintf("%s, %s", nounCount(st.Searches, "search", "searches"), nounCount(st.Fetches, "fetch", "fetches")))
		}
		if st.Failed > 0 {
			failed := fmt.Sprintf("%d", st.Failed)
			if st.RateLimited > 0 {
				failed += fmt.Sprintf(", %d rate limited", st.RateLimited)
			}
			row("Failed", failed)
		}
		if st.Retries > 0 {
			row("Retried", nounCount(st.Retries, "rate-limited attempt", "rate-limited attempts")+" sent again")
		}
	}
	row("New sessions", newSessions(cfg, path))
	return rows
}

// sessionConsent is a WebStatusState.Consent in the panel's words.
func sessionConsent(c string) string {
	switch c {
	case "allowed":
		return "on"
	case "allowed for this session":
		return "on, for this session"
	case "disabled":
		return "off: disabled from the consent card"
	case "declined":
		return "off: the consent card was skipped"
	case "undecided":
		return "asks on its first web call"
	}
	return c
}

// keySource is a WebStatusState.Key in the panel's words.
func keySource(k string) string {
	switch k {
	case string(websearch.KeyStored):
		return "stored key (craze auth login websearch/exa)"
	case string(websearch.KeyEnv):
		return websearch.ExaKeyEnv
	case string(websearch.KeyNone):
		return "keyless (Exa's free limit; craze auth login websearch/exa raises it)"
	}
	return k
}

// newSessions is what a new session does about web search: config.toml's
// setting when it has one, else the remembered choice.
func newSessions(cfg agent.WebConfig, path string) string {
	if over := configOverride(cfg); over != "" {
		return over
	}
	ch, why := websearch.ReadChoice(path)
	switch {
	case ch.Provider != "":
		return "on (" + websearch.StateFile + ") · /websearch off or ask to change"
	case ch.Disabled:
		return "off (" + websearch.StateFile + ") · /websearch on or ask to change"
	case why != "":
		return "ask on the first web call (" + why + ")"
	}
	return "ask on the first web call · /websearch on to allow without asking"
}

// nounCount is n with its noun.
func nounCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
