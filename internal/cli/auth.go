package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
)

// craze auth (plan 031 §3.7, P1–P3): the native provider's API keys, kept
// where a hand-written key has always lived — the inline api_key of its
// providers.toml entry (modeltable.SetKey), which a session tries after the
// provider's env_keys. `login` stores one, `logout` removes one, `list` says
// how each provider is funded. No key is ever checked with its provider (the
// owner's decision: a bad key shows on first use), so none of the three makes
// a network request for a key.
//
// The ChatGPT plan is the exception (plan 033 §3.13): it is funded by Sign in
// with ChatGPT and never by a key, so for it `login` runs the sign-in,
// `logout` signs out, and `list` shows the sign-in — auth_chatgpt.go. Its
// login branches off before any key is read or asked for (X134).
//
// A key is read from stdin when stdin is not a terminal — its first line, at
// most maxKeyLine bytes, trimmed — and otherwise from a prompt that does not
// echo (echoOff). No key, and no part of one, is ever printed, logged or put
// in an error: an argument that names no provider is not quoted back either,
// since a key typed where the provider goes is the likeliest mistake — nor is
// a surplus argument or a flag the commands do not take (authArgs,
// authFlagError), which cobra's own errors would quote.
//
// Output: what the command did goes to stdout; prompts, the provider menu and
// notes go to stderr.

// maxKeyLine is the longest key line craze auth login reads. No provider
// issues a key anywhere near it; a longer line is a file piped by mistake.
const maxKeyLine = 8 << 10

// authRun is one craze auth command's world: the directory it keeps keys in,
// the environment it judges variables by, and its streams. tty is stdin when
// that is a terminal, nil otherwise. ctx is the command's, and noBrowser is
// login's --no-browser, for the ChatGPT plan's sign-in.
type authRun struct {
	name      string // "craze auth login", for messages
	dir       string
	getenv    func(string) string
	in        io.Reader
	tty       *os.File
	out, errw io.Writer
	ctx       context.Context
	noBrowser bool
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage model provider API keys and the ChatGPT sign-in",
		Long: `Manage the API keys of the native provider's model providers, and the
ChatGPT plan's sign-in.

A key is stored in the craze directory's native/providers.toml (0600), which
craze rewrites whole: comments there are not kept. A provider's environment
variable (FIREWORKS_API_KEY, say) is used before its stored key. No key is
checked with its provider when it is stored: a wrong one shows on first use.

The ChatGPT plan takes no key: "craze auth login chatgpt" signs in with
ChatGPT in a browser, "craze auth logout chatgpt" signs out, and
"craze auth list" shows the sign-in.`,
		// A group's own run: bare, it prints its help; with an argument —
		// a subcommand mistyped, `craze auth lgin fireworks` — it is a usage
		// error, where cobra's default would print the help and exit 0. The
		// argument is not quoted back (it may be a key).
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return usagef("%s: unknown command; want login, logout or list", cmd.CommandPath())
		},
	}
	// The group's subcommands inherit it (cobra's FlagErrorFunc).
	cmd.SetFlagErrorFunc(authFlagError)
	cmd.AddCommand(newAuthLoginCmd(), newAuthLogoutCmd(), newAuthListCmd())
	return cmd
}

// authArgs is an auth subcommand's argument check: at most max arguments.
// More is a usage error that says what the command takes and never what it
// was given (plan 031 X29; review r2) — cobra's own checks quote the
// arguments (NoArgs) and exit 1, and a key pasted onto the command line is
// the likeliest surplus.
func authArgs(max int, takes string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > max {
			return usagef("%s: %s", cmd.CommandPath(), takes)
		}
		return nil
	}
}

// authFlagError is the auth commands' error for a flag that does not parse —
// an unknown one, or a value its flag cannot take: a usage error that names
// the command and where its flags are listed, never the flag, which pflag's
// own message quotes and which may be a key (`--sk-…`, review r2).
func authFlagError(cmd *cobra.Command, _ error) error {
	return usagef("%s: unknown or malformed flag; see %s --help", cmd.CommandPath(), cmd.CommandPath())
}

func newAuthLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login [provider]",
		Short: "Store a provider's API key, or sign in to the ChatGPT plan",
		Long: `Store a provider's API key, or sign in to the ChatGPT plan.

The provider is its id or its display name, in any case. With none given on a
terminal, craze shows a numbered list to pick from. The key is read without
echo on a terminal, and otherwise from the first line of stdin.

For the ChatGPT plan (chatgpt), craze prints an address to open in a browser
(and opens it, on a desktop), then waits for the browser to come back to it,
or for the address the browser was sent to pasted here. --no-browser opens
no browser and waits only for the pasted address: the way to sign in from a
machine the browser is not on.`,
		Args: authArgs(1, "takes one argument at most, the provider"),
		RunE: authRunE((*authRun).login),
	}
	cmd.Flags().Bool("no-browser", false, "ChatGPT plan: open no browser and listen for no redirect; paste the redirect address instead")
	return cmd
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout <provider>",
		Short: "Remove a provider's stored API key",
		// None is refused in RunE, whose message lists the providers; the
		// check here runs before the root's PersistentPreRunE.
		Args: authArgs(1, "takes one argument, the provider"),
		RunE: authRunE((*authRun).logout),
	}
}

func newAuthListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the providers and how each is connected",
		Args:  authArgs(0, "takes no arguments"),
		RunE:  authRunE(func(a *authRun, _ string) error { return a.list() }),
	}
}

// authRunE is an auth subcommand's RunE: run over the command's authRun,
// with its provider argument ("" when there is none).
func authRunE(run func(a *authRun, arg string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a, err := newAuthRun(cmd)
		if err != nil {
			return err
		}
		arg := ""
		if len(args) > 0 {
			arg = args[0]
		}
		return run(a, arg)
	}
}

// newAuthRun is cmd's authRun over the real craze directory and environment.
// All three commands fail here when there is no craze directory.
func newAuthRun(cmd *cobra.Command) (*authRun, error) {
	a := &authRun{
		name:   cmd.CommandPath(),
		dir:    paths.NativeDir(),
		getenv: os.Getenv,
		in:     cmd.InOrStdin(),
		out:    cmd.OutOrStdout(),
		errw:   cmd.ErrOrStderr(),
		ctx:    cmd.Context(),
	}
	// login's alone; the others have no such flag, and read false.
	a.noBrowser, _ = cmd.Flags().GetBool("no-browser")
	if a.dir == "" {
		return nil, exitf(1, "%s: there is no craze directory to keep API keys in (set HOME or CRAZE_HOME)", a.name)
	}
	if f, ok := a.in.(*os.File); ok && isTerminal(f) {
		a.tty = f
	}
	return a, nil
}

// login is craze auth login [provider].
func (a *authRun) login(arg string) error {
	infos, err := a.providers()
	if err != nil {
		return err
	}
	var p modeltable.ProviderInfo
	named := strings.TrimSpace(arg) != ""
	switch {
	case named:
		if p, err = a.match(infos, arg); err != nil {
			return err
		}
		if p.SignIn {
			// The ChatGPT plan signs in, before any key is read or asked
			// for (plan 033 §3.13, X134).
			return a.signIn()
		}
	case a.tty == nil:
		return usagef("%s: name a provider (%s); craze shows a list to pick from only on a terminal", a.name, keyProviderIDs(infos))
	}
	var key string
	if a.tty == nil {
		key, err = a.readKeyLine()
	} else {
		p, key, err = a.ask(infos, p, named)
	}
	if err != nil {
		return err
	}
	if p.SignIn {
		// Picked from the menu: ask returned before the key's prompt, with
		// the terminal as it found it; the sign-in turns the echo off again
		// for its own prompt, under its own signal handling (signIn).
		return a.signIn()
	}
	name := sanitizeLine(p.Name)
	if key == "" {
		return exitf(1, "%s: no key given; nothing was saved", a.name)
	}
	if err := modeltable.SetKey(a.dir, p.ID, key); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.out, "Saved the %s key in %s.\n", name, sanitizeLine(filepath.Join(a.dir, modeltable.ProvidersFile)))
	if p.EnvVar != "" {
		fmt.Fprintf(a.out, "%s is set in this environment; craze uses it before the stored key.\n", sanitizeLine(p.EnvVar))
	}
	a.brokenKeyNotes(p.ID)
	return nil
}

// logout is craze auth logout <provider>.
func (a *authRun) logout(arg string) error {
	infos, err := a.providers()
	if err != nil {
		return err
	}
	if strings.TrimSpace(arg) == "" {
		return usagef("%s: name the provider whose stored key to remove (%s)", a.name, keyProviderIDs(infos))
	}
	p, err := a.match(infos, arg)
	if err != nil {
		return err
	}
	if p.SignIn {
		return a.signOut(p)
	}
	removed, err := modeltable.RemoveKey(a.dir, p.ID)
	if err != nil {
		return a.fail(err)
	}
	name := sanitizeLine(p.Name)
	if removed {
		fmt.Fprintf(a.out, "Removed the stored %s key.\n", name)
	} else {
		fmt.Fprintf(a.out, "No stored %s key.\n", name)
	}
	if p.EnvVar != "" {
		fmt.Fprintf(a.out, "%s is still connected through %s.\n", name, sanitizeLine(p.EnvVar))
	}
	a.brokenKeyNotes(p.ID)
	return nil
}

// list is craze auth list: one row per provider — its display name, its id
// and how it is funded — then notes: stored keys that cannot be used, and
// what loading the model table says. The table is loaded only for its notes
// (the redundant overrides of plan 031 §3.3, its warnings and the unusable
// variables); one that does not load — a broken models.toml, say — is itself
// a note, since the rows need only providers.toml and the catalog.
func (a *authRun) list() error {
	infos, err := a.providers()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	for _, p := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", sanitizeLine(p.Name), sanitizeLine(p.ID), a.connectedBy(p))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, p := range infos {
		if p.StoredProblem != nil {
			a.note(brokenKeyNote(p))
		}
	}
	table, err := modeltable.Load(a.dir)
	if err != nil {
		a.note("native sessions cannot start until this is fixed: " + strings.TrimPrefix(err.Error(), "modeltable: "))
		return nil
	}
	for _, line := range slices.Concat(table.RedundantOverrides(), table.Warnings, table.EnvWarnings(a.getenv)) {
		a.note(strings.TrimPrefix(line, "modeltable: "))
	}
	return nil
}

// connectedBy is a list row's last column: the variable that funds the
// provider, its stored key, or neither — the order Resolve tries them in —
// and, for the ChatGPT plan, its sign-in (plan 033 §3.13, signInStatus):
// signed in with plan usage, signed in with plan usage off, or not.
func (a *authRun) connectedBy(p modeltable.ProviderInfo) string {
	if p.SignIn {
		return a.signInStatus(p.Via)
	}
	switch p.Via {
	case modeltable.KeyFromEnv:
		return "env " + sanitizeLine(p.EnvVar)
	case modeltable.KeyStored:
		return "stored key"
	}
	return "not connected"
}

// fail is err, from modeltable, as a's exit 1 on one line.
//
// The model table's own errors lose their package's "modeltable: " prefix
// here, at the command line's boundary (plan 037 LC-8): "craze auth login:
// the key for provider …", never "craze auth login: modeltable: …".
func (a *authRun) fail(err error) error {
	return exitf(1, "%s: %s", a.name, sanitizeLine(strings.TrimPrefix(err.Error(), "modeltable: ")))
}

// providers is modeltable.Providers over a's directory and environment, its
// error an exit.
func (a *authRun) providers() ([]modeltable.ProviderInfo, error) {
	infos, err := modeltable.Providers(a.dir, a.getenv)
	if err != nil {
		return nil, a.fail(err)
	}
	return infos, nil
}

// match is the provider arg names: an id, else a display name, either in any
// case. No match, or a name two providers share, is a usage error that lists
// the ids and does not quote arg.
func (a *authRun) match(infos []modeltable.ProviderInfo, arg string) (modeltable.ProviderInfo, error) {
	p, n := matchProvider(infos, arg)
	switch {
	case n == 1:
		return p, nil
	case n > 1:
		return p, usagef("%s: more than one provider has that name; name it by id (%s)", a.name, keyProviderIDs(infos))
	}
	return p, usagef("%s: no such provider; craze has %s", a.name, keyProviderIDs(infos))
}

// matchProvider is the one provider arg names and 1, or how many match: an
// id match wins over a name match, and ids are unique.
func matchProvider(infos []modeltable.ProviderInfo, arg string) (modeltable.ProviderInfo, int) {
	arg = strings.TrimSpace(arg)
	for _, p := range infos {
		if strings.EqualFold(p.ID, arg) {
			return p, 1
		}
	}
	var found modeltable.ProviderInfo
	n := 0
	for _, p := range infos {
		if strings.EqualFold(p.Name, arg) {
			found, n = p, n+1
		}
	}
	return found, n
}

// keyProviderIDs is every provider's id, in list order, for a message.
func keyProviderIDs(infos []modeltable.ProviderInfo) string {
	if len(infos) == 0 {
		return "none"
	}
	ids := make([]string, len(infos))
	for i, p := range infos {
		ids[i] = p.ID
	}
	return sanitizeLine(strings.Join(ids, ", "))
}

// ask is login's conversation on the terminal: the menu when no provider was
// named, then the key's prompt — the terminal's echo off from before the
// first prompt is drawn until the key is read (echoOff), so nothing typed at
// either shows as it is typed, however soon after its prompt it comes.
func (a *authRun) ask(infos []modeltable.ProviderInfo, p modeltable.ProviderInfo, named bool) (modeltable.ProviderInfo, string, error) {
	q, err := quiet(a.tty)
	if err != nil {
		return p, "", exitf(1, "%s: turning the terminal's echo off: %v; nothing was saved", a.name, err)
	}
	defer q.restore()
	if !named {
		if p, err = a.choose(infos); err != nil {
			return p, "", err
		}
		if p.SignIn {
			// The ChatGPT plan takes no key: login signs in instead, once
			// the deferred restore has put the echo back and taken this
			// handler away (the sign-in has its own: signIn).
			return p, "", nil
		}
	}
	key, err := a.promptKey(sanitizeLine(p.Name))
	return p, key, err
}

// maxMenuLine is the longest menu answer choose keeps; the rest of a longer
// line is read and dropped.
const maxMenuLine = 256

// choose shows the numbered menu on the terminal and reads the choice, the
// echo off (ask): a number, or an id or display name as login's argument
// takes. Connected providers are marked. Only a number on the list is written
// back after the prompt, to look as if it was echoed; any other answer never
// is — nor quoted in an error — since a key pasted at the wrong prompt is the
// mistake this guards.
func (a *authRun) choose(infos []modeltable.ProviderInfo) (modeltable.ProviderInfo, error) {
	if len(infos) == 0 {
		return modeltable.ProviderInfo{}, exitf(1, "%s: there is no provider to connect", a.name)
	}
	fmt.Fprintln(a.errw, "Connect a model provider:")
	for i, p := range infos {
		mark := ""
		if p.Connected() {
			mark = " (connected)"
		}
		fmt.Fprintf(a.errw, "  %d. %s%s\n", i+1, sanitizeLine(p.Name), mark)
	}
	fmt.Fprintf(a.errw, "Provider [1-%d]: ", len(infos))
	line, long, err := readTTYLine(a.tty, maxMenuLine)
	if n, ok := menuNumber(line, len(infos)); ok && !long {
		fmt.Fprintf(a.errw, "%d\n", n)
	} else {
		fmt.Fprintln(a.errw) // the Enter that ended the answer was not echoed
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return modeltable.ProviderInfo{}, exitf(1, "%s: reading the choice: %v", a.name, err)
	}
	if long {
		return modeltable.ProviderInfo{}, errNotOnTheList(a.name)
	}
	return a.pick(infos, line)
}

// menuNumber is the menu entry answer names by number, 1 to n.
func menuNumber(answer string, n int) (int, bool) {
	i, err := strconv.Atoi(strings.TrimSpace(answer))
	return i, err == nil && i >= 1 && i <= n
}

// errNotOnTheList is login's refusal of a menu answer, which it never quotes.
func errNotOnTheList(name string) error {
	return usagef("%s: that is not a provider on the list; nothing was saved", name)
}

// pick is the provider a menu answer names; see choose.
func (a *authRun) pick(infos []modeltable.ProviderInfo, answer string) (modeltable.ProviderInfo, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return modeltable.ProviderInfo{}, exitf(1, "%s: no provider chosen; nothing was saved", a.name)
	}
	if n, ok := menuNumber(answer, len(infos)); ok {
		return infos[n-1], nil
	}
	if p, n := matchProvider(infos, answer); n == 1 {
		return p, nil
	}
	return modeltable.ProviderInfo{}, errNotOnTheList(a.name)
}

// readKeyLine is the key from stdin, not a terminal: its first line, at
// most maxKeyLine bytes, trimmed; "" is no key.
func (a *authRun) readKeyLine() (string, error) {
	line, err := readKeyLine(a.in)
	if err != nil {
		return "", exitf(1, "%s: %v; nothing was saved", a.name, err)
	}
	return line, nil
}

// promptKey is the key from its prompt on the terminal, the echo already off
// (ask): at most maxKeyLine bytes, trimmed; "" is no key.
func (a *authRun) promptKey(name string) (string, error) {
	fmt.Fprintf(a.errw, "%s API key: ", name)
	line, long, err := readTTYLine(a.tty, maxKeyLine)
	fmt.Fprintln(a.errw) // the Enter that ended the key was not echoed
	if err != nil && !errors.Is(err, io.EOF) {
		return "", exitf(1, "%s: reading the key: %v; nothing was saved", a.name, err)
	}
	if long {
		return "", exitf(1, "%s: %v; nothing was saved", a.name, errKeyLineTooLong)
	}
	return strings.TrimSpace(line), nil
}

var errKeyLineTooLong = fmt.Errorf("the key is longer than %d KiB", maxKeyLine>>10)

// readKeyLine is r's first line — up to a newline or the end — trimmed, or
// errKeyLineTooLong when it runs past maxKeyLine bytes, its line ending not
// counted. Nothing after the first line is used, and no more than the limit
// and a CRLF is read.
func readKeyLine(r io.Reader) (string, error) {
	br := bufio.NewReader(io.LimitReader(r, maxKeyLine+2))
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) > maxKeyLine {
		return "", errKeyLineTooLong
	}
	return strings.TrimSpace(line), nil
}

// readTTYLine reads one line from the terminal f a byte at a time, so
// nothing after it is taken — the key prompt that follows the menu reads the
// same descriptor — and all of it, so no part of a long one is left for the
// shell to read once craze exits. It keeps at most max bytes and says whether
// there were more (long); the newline is not included, a carriage return is
// dropped and a backspace takes back the byte before it, as x/term's
// ReadPassword reads a line.
func readTTYLine(f *os.File, max int) (line string, long bool, err error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		n, rerr := f.Read(b)
		if n == 1 {
			switch b[0] {
			case '\n':
				return string(buf), long, nil
			case '\r':
			case '\b':
				if len(buf) > 0 && !long {
					buf = buf[:len(buf)-1]
				}
			default:
				if len(buf) < max {
					buf = append(buf, b[0])
				} else {
					long = true
				}
			}
		}
		if rerr != nil {
			return string(buf), long, rerr
		}
	}
}

// brokenKeyNotes are the notes for the stored keys, other than except's,
// that cannot be used (plan 031 §3.7, r2-7): login and logout keep them as
// they were, and say so.
func (a *authRun) brokenKeyNotes(except string) {
	infos, err := modeltable.Providers(a.dir, a.getenv)
	if err != nil {
		return
	}
	for _, p := range infos {
		if p.ID != except && p.StoredProblem != nil {
			a.note(brokenKeyNote(p))
		}
	}
}

// brokenKeyNote says p's stored key cannot be used, by the rule — never the
// value — and how to replace or remove it.
func brokenKeyNote(p modeltable.ProviderInfo) string {
	why := "it overlaps craze's redaction marker"
	if errors.Is(p.StoredProblem, modeltable.ErrKeyTooShort) {
		why = fmt.Sprintf("it is shorter than %d bytes", modeltable.MinKeyLen)
	}
	id := sanitizeLine(p.ID)
	return fmt.Sprintf(`the stored %s key cannot be used: %s; replace it with "craze auth login %s" or remove it with "craze auth logout %s"`,
		sanitizeLine(p.Name), why, id, id)
}

// note is one line of a's notes, on stderr.
func (a *authRun) note(line string) {
	fmt.Fprintln(a.errw, "note: "+sanitizeLine(line))
}
