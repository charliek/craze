package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// craze auth for the web search's key (plan 051 §F.5, R4): not a model
// provider's, kept in native/credentials.toml (modeltable.SetCredential), and
// sent by a native session's web search when the variable (EXA_API_KEY) holds
// none — without either, the search uses the provider's keyless endpoint.
//
// Addressing: a credential's full name, websearch/exa, is always accepted by
// login and logout, and is dispatched before providers.toml is read, so a
// providers.toml craze cannot read never stops a search key being stored or
// removed. The bare id, exa (or its display name), means the same unless a
// model provider matches it by id or display name, in any case — then bare
// exa is that provider, exactly as before, and the search key stays reachable
// by its full name. Bare exa with a providers.toml craze cannot read fails as
// any provider name does, and its message names the full name. list shows
// the credentials after the providers, under a "Web search" heading, even
// when the providers cannot be listed.

// webByFullName is the credential arg names by its full name, in any case.
func webByFullName(arg string) (modeltable.WebCredential, bool) {
	arg = strings.TrimSpace(arg)
	for _, c := range modeltable.WebCredentials() {
		if strings.EqualFold(arg, c.FullName()) {
			return c, true
		}
	}
	return modeltable.WebCredential{}, false
}

// webByBareName is the credential arg names by its id or display name, in
// any case — what it means when no model provider has that name (shadowed).
func webByBareName(arg string) (modeltable.WebCredential, bool) {
	arg = strings.TrimSpace(arg)
	for _, c := range modeltable.WebCredentials() {
		if strings.EqualFold(arg, c.ID) || strings.EqualFold(arg, c.Name) {
			return c, true
		}
	}
	return modeltable.WebCredential{}, false
}

// webTarget is the credential arg names when it names no model provider of
// infos: its full name always, its bare name only when no provider matches
// it by id or display name (one or more: an ambiguous provider name is still
// a provider's).
func webTarget(infos []modeltable.ProviderInfo, arg string) (modeltable.WebCredential, bool) {
	if c, ok := webByFullName(arg); ok {
		return c, true
	}
	if _, n := matchProvider(infos, arg); n > 0 {
		return modeltable.WebCredential{}, false
	}
	return webByBareName(arg)
}

// loginWeb is login for credential c: the key read as a provider's is —
// stdin's first line, or ask's prompt with the echo off — then stored.
func (a *authRun) loginWeb(c modeltable.WebCredential) error {
	var key string
	var err error
	if a.tty == nil {
		key, err = a.readKeyLine()
	} else {
		_, key, err = a.ask(nil, authChoice{web: &c}, true)
	}
	if err != nil {
		return err
	}
	return a.storeWeb(c, key)
}

// storeWeb stores key as c's and says where, and what the variable does to
// it: used first when it holds a key, skipped when it holds one that cannot
// be (plan 051 §F.5).
func (a *authRun) storeWeb(c modeltable.WebCredential, key string) error {
	if key == "" {
		return exitf(1, "%s: no key given; nothing was saved", a.name)
	}
	if err := modeltable.SetCredential(a.dir, c.ID, key); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.out, "Saved the %s web search key in %s.\n", sanitizeLine(c.Name), sanitizeLine(filepath.Join(a.dir, modeltable.CredentialsFile)))
	w := a.webInfo(c)
	switch {
	case w.Via == modeltable.KeyFromEnv:
		fmt.Fprintf(a.out, "%s is set in this environment; craze uses it before the stored key.\n", sanitizeLine(c.Env))
	case w.EnvProblem != nil:
		fmt.Fprintf(a.out, "%s is set in this environment to a value that cannot be a key (%s); craze skips it and uses the stored key.\n",
			sanitizeLine(c.Env), keyRule(w.EnvProblem))
	}
	return nil
}

// logoutWeb is logout for credential c.
func (a *authRun) logoutWeb(c modeltable.WebCredential) error {
	removed, err := modeltable.RemoveCredential(a.dir, c.ID)
	if err != nil {
		return a.fail(err)
	}
	name := sanitizeLine(c.Name)
	if removed {
		fmt.Fprintf(a.out, "Removed the stored %s web search key.\n", name)
	} else {
		fmt.Fprintf(a.out, "No stored %s web search key.\n", name)
	}
	if a.webInfo(c).Via == modeltable.KeyFromEnv {
		fmt.Fprintf(a.out, "%s web search is still connected through %s.\n", name, sanitizeLine(c.Env))
	}
	return nil
}

// webInfo is c as WebKeys judges it now.
func (a *authRun) webInfo(c modeltable.WebCredential) modeltable.WebKeyInfo {
	infos, _ := modeltable.WebKeys(a.dir, a.getenv)
	for _, w := range infos {
		if w.ID == c.ID {
			return w
		}
	}
	return modeltable.WebKeyInfo{WebCredential: c, Via: modeltable.KeyNone}
}

// listWeb is list's "Web search" section on stdout — one row per credential:
// its display name, its full name and how it is keyed — and returns the
// lines to note about it (WebKeys'). gap puts a blank line before the
// heading, after the providers' rows.
func (a *authRun) listWeb(gap bool) ([]string, error) {
	infos, lines := modeltable.WebKeys(a.dir, a.getenv)
	if gap {
		fmt.Fprintln(a.out)
	}
	fmt.Fprintln(a.out, "Web search")
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	for _, w := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", sanitizeLine(w.Name), sanitizeLine(w.FullName()), webConnectedBy(w))
	}
	return lines, tw.Flush()
}

// webConnectedBy is a web row's last column: the variable, the stored key
// or the keyless endpoint — the order the search takes them in — and, when
// the variable is set to a value that cannot be a key, that it is skipped.
func webConnectedBy(w modeltable.WebKeyInfo) string {
	s := "no key (keyless endpoint)"
	switch w.Via {
	case modeltable.KeyFromEnv:
		s = "env " + sanitizeLine(w.Env)
	case modeltable.KeyStored:
		s = "stored key"
	}
	if w.EnvProblem != nil {
		s += "; " + sanitizeLine(w.Env) + " is set but unusable"
	}
	return s
}

// keyRule is a key rule's error (ErrKeyTooShort, ErrKeyOverlapsMarker) in
// words, never the value.
func keyRule(err error) string { return "it " + modeltable.KeyRule(err) }
