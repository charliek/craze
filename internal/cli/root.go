package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/version"
)

func NewRootCmd() *cobra.Command {
	flags := &tuiFlags{force: true}
	var showVersion bool
	cmd := &cobra.Command{
		Use:   "craze",
		Short: "A terminal UI for coding agents",
		// "built-in agent", never "native": the root --help names native
		// exactly once, as a --provider choice (plan 028 D-65,
		// TestRootHelpNamesNativeOnceAndNeverTheHarness).
		Long: "craze is a terminal UI for coding agents: cursor-agent, grok or gx over ACP, or craze's own " +
			"built-in agent, which needs only a model provider's API key or a ChatGPT plan (see craze auth).",
		SilenceUsage:  true,
		SilenceErrors: true,
		// cobra's own version flag answered before argument validation; keep that.
		// Anything else is an unknown command, never quoted back (args.go).
		Args: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				return nil
			}
			return noArgs(cmd, args)
		},
		// Every command, the root included, refuses to run while the removed
		// config-file variable is set (paths.CheckEnv), before it can read or
		// write anything. No subcommand may define a PersistentPreRunE of its
		// own: it would replace this one. --help never reaches a hook, and
		// --version is let through here; both touch no file, so they still
		// answer.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Only the root's RunE honours --version, so only the root may skip
			// the check for it.
			if showVersion && cmd == cmd.Root() {
				return nil
			}
			if err := paths.CheckEnv(); err != nil {
				return usagef("craze: %v", err)
			}
			if isCompletionRequest(cmd) {
				return checkCompletionRequest(cmd, args)
			}
			if readsRegistry(cmd) {
				startHostSweep()
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if showVersion {
				_, err := fmt.Fprintln(out, version.Version)
				return err
			}
			if !stdoutIsTerminal(cmd) {
				return usagef("craze: refusing to start TUI on a non-tty")
			}
			return runTUI(cmd, flags, processHostEnv())
		},
	}
	// craze owns --version instead of setting cobra's Version field, because
	// the bare output needs SetVersionTemplate, and any cobra Set*Template call
	// makes text/template's executor reachable. That executor looks methods up
	// by name, so the linker then keeps every exported method of every linked
	// type: 14 MB of openai-go alone (TestNoReflectiveMethodLookupLinked in cmd/craze).
	cmd.Flags().BoolVarP(&showVersion, "version", "v", false, "version for craze")
	registerTUIFlags(cmd, flags)
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newPromptCmd())
	cmd.AddCommand(newFrameCmd())
	cmd.AddCommand(newBridgeCmd())
	cmd.AddCommand(newAttachCmd())
	cmd.AddCommand(newPsCmd())
	cmd.AddCommand(newProvidersCmd())
	cmd.AddCommand(newNewCmd())
	cmd.AddCommand(newSendCmd())
	cmd.AddCommand(newWaitCmd())
	cmd.AddCommand(newCancelCmd())
	cmd.AddCommand(newStopCmd())
	cmd.AddCommand(newServeCmd())
	cmd.AddCommand(newHubCmd())
	cmd.AddCommand(newAuthCmd())
	// Every subcommand inherits it, but auth's, which sets its own in the
	// same words (args.go).
	cmd.SetFlagErrorFunc(usageFlagError)
	withoutEchoingArgs(cmd)
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the craze version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}

func Execute() {
	ranCmd, err := NewRootCmd().ExecuteC()
	if err == nil {
		joinHostSweep()
		return
	}
	// What the command has to say is out before the join waits on anything, and the
	// code it has determined is what the process exits with, whatever the join meets.
	// The one other way out is auth's signal handler (terminal.go), and auth starts no
	// sweep.
	code := report(os.Stdout, os.Stderr, ranCmd, os.Args[1:], err)
	joinHostSweep()
	os.Exit(code)
}

// report is what Execute does with a command's error, given the command line:
// the exit code it answers, a usage error's JSON object on stdout when the line
// asked for JSON (usageJSON), and the one stderr line either way.
func report(stdout, stderr io.Writer, ranCmd *cobra.Command, args []string, err error) int {
	line, code := diagnose(ranCmd, err)
	if b := usageJSON(ranCmd, args, err, line, code); b != nil {
		// A stdout whose reader has gone must not take the stderr line with it
		// (craze new's rule): the write fails with EPIPE instead.
		ignoreSIGPIPE()
		_, _ = stdout.Write(b)
	}
	if line != "" {
		fmt.Fprintln(stderr, line)
	}
	return code
}

// usageJSON is a usage error's one JSON object — {"error":{"message":…,"kind":…,
// "retryable":false}} and a newline — when the command line asked for JSON, and
// nil otherwise. Asked for by jsonRequested, since a command line cobra could
// not parse never set the flag. Exit 2 alone: every other failure a --json
// command prints as its own. Never for craze prompt, whose --json is an event
// stream with a contract of its own, or for a completion request, whose stdout
// is the shell's.
func usageJSON(ranCmd *cobra.Command, args []string, err error, line string, code int) []byte {
	if code != 2 || ranCmd == nil || ranCmd.Name() == "prompt" || isCompletionRequest(ranCmd) || !jsonRequested(ranCmd, args) {
		return nil
	}
	b, merr := json.Marshal(failureBody{Error: kindError(line, failureKind(err))})
	if merr != nil {
		return nil
	}
	return append(b, '\n')
}

// jsonRequested says whether args, the command line cmd ran from, asks for
// --json, read as pflag would have read it: the last --json wins, and a value
// that is no bool (--json=maybe) still asks, that being the error to report. It
// is not a search for the word. Nothing after `--` is a flag, and a flag that
// takes a value takes the next word as it stands, so `--title --json` names a
// title and `new --wait -- --json` a prompt. A flag cmd does not know takes no
// word, its arity being unknown: `--bogus --json` asks. TestJSONRequestedReadsAsPflagDoes
// holds it to the parser. pflag's own ParseAll cannot do this reading: even told to
// pass over flags it does not know, it stops at a malformed one (`---x`) and at a
// help flag it has not been given, and a line it stopped on is what this is for.
func jsonRequested(cmd *cobra.Command, args []string) bool {
	flags, on := cmd.Flags(), false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return on
		case arg == "--json":
			on = true
		case strings.HasPrefix(arg, "--json="):
			v, err := strconv.ParseBool(strings.TrimPrefix(arg, "--json="))
			on = err != nil || v
		case strings.HasPrefix(arg, "--"):
			if name, _, inline := strings.Cut(arg[2:], "="); !inline && takesValue(flags.Lookup(name)) {
				i++
			}
		case len(arg) > 1 && arg[0] == '-':
			// A run of shorthands, as pflag reads one, each in turn: the first
			// that takes a value has the rest of the word or, when it is the
			// last, the next word. One it does not know ends the reading there
			// and takes no word. `=` is one such, which is why -w=C --json asks:
			// the rest of the word is w's value, never another shorthand.
			for j := 1; j < len(arg); j++ {
				f := flags.ShorthandLookup(arg[j : j+1])
				if f == nil {
					break
				}
				if takesValue(f) {
					if j == len(arg)-1 {
						i++
					}
					break
				}
			}
		}
	}
	return on
}

// takesValue says f, nil when no such flag, takes a value that may be the next
// word: not a bool, which has a value of its own for its bare form.
func takesValue(f *pflag.Flag) bool { return f != nil && f.NoOptDefVal == "" }

// diagnose is what Execute prints (one line, or none) and the code it exits
// with, for ranCmd's error: craze bridge's own contract (bridgeLine, exit 1;
// exit 3 for errNoHubRunning alone)
// for every error that reaches it once the ran command is bridge's — flag
// parsing, an extra argument, and the root's shared PersistentPreRunE (a
// stray removed config-file variable in an SSH environment) included, since bridge defines
// none of its own (root.go's own rule: no subcommand may) — or, for every
// other command, today's mapping: an exitError's own code and message, else
// exit 1 with the error printed as it is.
func diagnose(ranCmd *cobra.Command, err error) (line string, code int) {
	if ranCmd != nil && ranCmd.Name() == "bridge" {
		if errors.Is(err, errNoHubRunning) {
			return bridgeLine(err), errNoHubRunning.code
		}
		return bridgeLine(err), 1
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.msg, ee.code
	}
	// A completion request's own argument check is cobra's (it takes at
	// least the word being completed), and runs before anything of craze's
	// could: it is a usage error like every other, in craze's words.
	if isCompletionRequest(ranCmd) {
		return diagnose(nil, completionRequestUsage(ranCmd, "takes the command line to complete"))
	}
	return err.Error(), 1
}

// bridgeLine is diagnose's bridge-specific mapping: an exitError's own
// message — a stray removed config-file variable's "craze: ..." (usagef, exit 2) included,
// its leading "craze: " stripped — or, for a raw cobra error (an unknown
// flag, an extra argument), its own text; bridgePrefix is added once, never
// twice, since bridge.go's own errors already carry it. This is the one place
// every bridge error is printed, so it is also where the one-line contract is
// made structural (sanitizeLine): whatever a registry value (a workspace
// path), an OS error, or anything else the message happens to embed contains,
// the line this returns can never itself contain a newline.
func bridgeLine(err error) string {
	msg := err.Error()
	var ee *exitError
	if errors.As(err, &ee) {
		msg = ee.msg
	}
	msg = strings.TrimPrefix(msg, "craze: ")
	msg = sanitizeLine(msg)
	if strings.HasPrefix(msg, bridgePrefix) {
		return msg
	}
	return bridgePrefix + msg
}

// sanitizeLine replaces every control character in s (unicode.IsControl:
// \r, \n, and the other C0/C1 controls included) with a space. It is what
// keeps bridgeLine's "one line" guarantee structural rather than incidental:
// a workspace path, a registry value, or an OS error's own text can carry a
// newline (or a \r that a terminal would render as one), and this is the one
// place that is stripped out before the line ever reaches stderr.
func sanitizeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
