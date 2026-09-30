package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/charmbracelet/x/term"
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
// a network request.
//
// A key is read from stdin when stdin is not a terminal — its first line, at
// most maxKeyLine bytes, trimmed — and otherwise from a prompt that does not
// echo. No key, and no part of one, is ever printed, logged or put in an
// error: an argument that names no provider is not quoted back either, since
// a key typed where the provider goes is the likeliest mistake.
//
// Output: what the command did goes to stdout; prompts, the provider menu and
// notes go to stderr.

// maxKeyLine is the longest key line craze auth login reads. No provider
// issues a key anywhere near it; a longer line is a file piped by mistake.
const maxKeyLine = 8 << 10

// authRun is one craze auth command's world: the directory it keeps keys in,
// the environment it judges variables by, and its streams. tty is stdin when
// that is a terminal, nil otherwise.
type authRun struct {
	name      string // "craze auth login", for messages
	dir       string
	getenv    func(string) string
	in        io.Reader
	tty       *os.File
	out, errw io.Writer
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage model provider API keys",
		Long: `Manage the API keys of the native provider's model providers.

A key is stored in the craze directory's native/providers.toml (0600), which
craze rewrites whole: comments there are not kept. A provider's environment
variable (FIREWORKS_API_KEY, say) is used before its stored key. No key is
checked with its provider when it is stored: a wrong one shows on first use.`,
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
	cmd.AddCommand(newAuthLoginCmd(), newAuthLogoutCmd(), newAuthListCmd())
	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login [provider]",
		Short: "Store a provider's API key",
		Long: `Store a provider's API key.

The provider is its id or its display name, in any case. With none given on a
terminal, craze shows a numbered list to pick from. The key is read without
echo on a terminal, and otherwise from the first line of stdin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: authRunE((*authRun).login),
	}
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout <provider>",
		Short: "Remove a provider's stored API key",
		// One argument, checked in RunE: cobra's own check would run before
		// the root's PersistentPreRunE, and its error is not a usage exit.
		Args: cobra.MaximumNArgs(1),
		RunE: authRunE((*authRun).logout),
	}
}

func newAuthListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the providers and how each is connected",
		Args:  cobra.NoArgs,
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
	}
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
	switch {
	case strings.TrimSpace(arg) != "":
		if p, err = a.match(infos, arg); err != nil {
			return err
		}
	case a.tty == nil:
		return usagef("%s: name a provider (%s); craze shows a list to pick from only on a terminal", a.name, keyProviderIDs(infos))
	default:
		if p, err = a.choose(infos); err != nil {
			return err
		}
	}
	name := sanitizeLine(p.Name)
	key, err := a.readKey(name)
	if err != nil {
		return err
	}
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
		fmt.Fprintf(tw, "%s\t%s\t%s\n", sanitizeLine(p.Name), sanitizeLine(p.ID), connectedBy(p))
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
// provider, its stored key, or neither — the order Resolve tries them in.
func connectedBy(p modeltable.ProviderInfo) string {
	switch p.Via {
	case modeltable.KeyFromEnv:
		return "env " + sanitizeLine(p.EnvVar)
	case modeltable.KeyStored:
		return "stored key"
	}
	return "not connected"
}

// fail is err, from modeltable, as a's exit 1 on one line.
func (a *authRun) fail(err error) error {
	return exitf(1, "%s: %s", a.name, sanitizeLine(err.Error()))
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

// choose shows the numbered menu on the terminal and reads the choice: a
// number, or an id or display name as login's argument takes. Connected
// providers are marked. The answer is never quoted back — a key pasted at the
// wrong prompt is the mistake this guards.
func (a *authRun) choose(infos []modeltable.ProviderInfo) (modeltable.ProviderInfo, error) {
	if len(infos) == 0 {
		return modeltable.ProviderInfo{}, exitf(1, "%s: there is no provider to connect", a.name)
	}
	fmt.Fprintln(a.errw, "Connect a model provider:")
	for i, p := range infos {
		mark := ""
		if p.Via != modeltable.KeyNone {
			mark = " (connected)"
		}
		fmt.Fprintf(a.errw, "  %d. %s%s\n", i+1, sanitizeLine(p.Name), mark)
	}
	fmt.Fprintf(a.errw, "Provider [1-%d]: ", len(infos))
	line, err := readLine(a.tty, 256)
	if err != nil && !errors.Is(err, io.EOF) {
		return modeltable.ProviderInfo{}, exitf(1, "%s: reading the choice: %v", a.name, err)
	}
	return a.pick(infos, line)
}

// pick is the provider a menu answer names; see choose.
func (a *authRun) pick(infos []modeltable.ProviderInfo, answer string) (modeltable.ProviderInfo, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return modeltable.ProviderInfo{}, exitf(1, "%s: no provider chosen; nothing was saved", a.name)
	}
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(infos) {
		return infos[n-1], nil
	}
	if p, n := matchProvider(infos, answer); n == 1 {
		return p, nil
	}
	return modeltable.ProviderInfo{}, usagef("%s: that is not a provider on the list; nothing was saved", a.name)
}

// readKey is the key: from a prompt that does not echo when stdin is a
// terminal, else stdin's first line. Either way it is at most maxKeyLine
// bytes and trimmed; "" is no key.
func (a *authRun) readKey(name string) (string, error) {
	if a.tty == nil {
		line, err := readKeyLine(a.in)
		if err != nil {
			return "", exitf(1, "%s: %v; nothing was saved", a.name, err)
		}
		return line, nil
	}
	fmt.Fprintf(a.errw, "%s API key: ", name)
	b, err := readSecret(a.tty)
	fmt.Fprintln(a.errw) // the Enter that ended the key was not echoed
	if err != nil && !errors.Is(err, io.EOF) {
		return "", exitf(1, "%s: reading the key: %v; nothing was saved", a.name, err)
	}
	if len(b) > maxKeyLine {
		return "", exitf(1, "%s: %v; nothing was saved", a.name, errKeyLineTooLong)
	}
	return strings.TrimSpace(string(b)), nil
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

// readLine reads one line from f a byte at a time, so nothing after it is
// taken from the terminal — the key prompt that follows reads the same
// descriptor. The newline is not included; max bounds the line.
func readLine(f *os.File, max int) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for len(buf) < max {
		n, err := f.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return string(buf), nil
			}
			buf = append(buf, b[0])
		}
		if err != nil {
			return string(buf), err
		}
	}
	return string(buf), nil
}

// readSecret reads one line from the terminal f without echo. A signal that
// arrives meanwhile — Ctrl-C at the prompt — would otherwise end craze with
// the terminal's echo still off, so the terminal is put back first and craze
// then exits as the signal asked.
func readSecret(f *os.File) ([]byte, error) {
	fd := f.Fd()
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-sigs:
			_ = term.Restore(fd, state)
			code := 128 + int(syscall.SIGINT)
			if s, ok := sig.(syscall.Signal); ok {
				code = 128 + int(s)
			}
			os.Exit(code)
		case <-done:
		}
	}()
	return term.ReadPassword(fd)
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
