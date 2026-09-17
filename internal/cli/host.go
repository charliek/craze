package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/charliek/craze/internal/host"
	"github.com/charliek/craze/internal/tui"
)

// hostEnv is the process environment as host-status construction reads it
// (plan 015 §3.5). It is injected rather than read from os directly because
// craze's own test runs happen inside real herdr panes: a test that reached
// the real environment would report into the pane it runs in. The zero value
// is an empty environment, which is what every test that does not want a host
// passes.
type hostEnv struct {
	getenv  func(string) string
	environ func() []string
}

// processHostEnv is the real environment, for the one production caller.
func processHostEnv() hostEnv { return hostEnv{getenv: os.Getenv, environ: os.Environ} }

func (e hostEnv) get(key string) string {
	if e.getenv == nil {
		return ""
	}
	return e.getenv(key)
}

func (e hostEnv) list() []string {
	if e.environ == nil {
		return nil
	}
	return e.environ()
}

// hostSet is the reporters whose host gate is met, one field per host, so a
// host added later is a field and a line in each method below rather than a
// reshaping of the callers. The zero value is "report to nobody".
type hostSet struct {
	herdr *host.Herdr
}

// herdrHookGate is the one variable every herdr hook asset gates on (plan 015
// §3.4). HERDR_SOCKET_PATH, HERDR_PANE_ID and HERDR_BIN_PATH are left alone, so
// the plain herdr CLI still resolves the pane from inside the agent.
const herdrHookGate = "HERDR_ENV"

// resolveHosts settles which hosts this run reports to: none when
// `host_status = false` or --no-host-status turned reporting off, and
// otherwise every host whose own environment gate is met. The config and flag
// only ever turn reporting off; the host's environment is what turns it on.
func resolveHosts(f *tuiFlags, env hostEnv) hostSet {
	if f.noHostStatus || !tui.ConfigHostStatus() {
		return hostSet{}
	}
	var s hostSet
	if h, ok := host.NewHerdrFromEnv(env.get); ok {
		s.herdr = h
	}
	return s
}

// reporters is the set as the hub takes it; empty means no hub is built.
func (s hostSet) reporters() []host.Reporter {
	var rs []host.Reporter
	if s.herdr != nil {
		rs = append(rs, s.herdr)
	}
	return rs
}

// childEnv is the agent child's environment: nil — inherit everything, as
// craze always has — when no host is active, and otherwise a full copy of
// environ less each active host's hook gate.
//
// The gate goes because the agent's own installed host hooks would otherwise
// fire inside craze's child and fight craze for the pane: a herdr cursor or
// grok hook stamps a session reference on the pane that silently disables
// every craze report for the rest of the session (plan 015 §2.1). The cost is
// that the child believes it is outside herdr; `host_status = false` is how a
// user gets the child's own hooks back.
func (s hostSet) childEnv(environ []string) []string {
	var strip []string
	if s.herdr != nil {
		strip = append(strip, herdrHookGate)
	}
	if len(strip) == 0 {
		return nil
	}
	return withoutEnv(environ, strip...)
}

// withoutEnv copies environ less every entry whose key — everything before the
// first "=" — is exactly one of keys. A value holding "=" is untouched, and so
// is a key that merely starts with one of keys.
func withoutEnv(environ []string, keys ...string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keys, k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// attachHost builds the hub for the active set and hands it to the TUI, which
// closes it on every exit path. With no host active it builds nothing and
// Config.Host stays nil. It is called only once nothing between it and tui.Run
// can return, so no hub is started that Run will not close.
//
// A reporter's failure is one line for diag, the buffer that is printed after
// the alt screen is gone; the hub serialises its calls and stops making them
// before Close returns, which is before diag is flushed.
func attachHost(cfg *tui.Config, s hostSet, diag io.Writer) {
	rs := s.reporters()
	if len(rs) == 0 {
		return
	}
	cfg.Host = host.NewHub(rs, host.HubOptions{
		Warn: func(msg string) { _, _ = fmt.Fprintln(diag, msg) },
	})
}
