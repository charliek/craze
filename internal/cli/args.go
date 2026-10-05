package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/charliek/craze/internal/hostspawn"
)

// craze's usage errors for the command line itself (plan 037 LC-1): an
// unknown command word, an argument a command does not take, and a flag that
// does not parse. Each is a usage error — usagef, exit 2 — that says what the
// command takes and where its help is, in the shape every other usage error
// has: "craze <cmd>: <what it takes>; see craze <cmd> --help".
//
// None of them echoes what it was given. The unknown word, the surplus
// argument and the bad flag are the three places a key pasted at the wrong
// prompt lands, and cobra's own messages quote each one back to the terminal
// (auth's rule, plan 031 X29/X37, now everyone's). A recognised flag's invalid
// value is not one of the three: it keeps its own message, which quotes it
// (`unknown provider "x"`), since the user put it in that flag on purpose.
//
// auth keeps its own (authArgs, authFlagError), which were already right, and
// providers its authArgs; providers' flag errors are the root's now, since its
// own quoted pflag's message, the flag included. bridge's errors reach
// Execute's diagnose like any other and come out as its one-line exit-1
// contract.

// noArgs is the argument check of a command that takes none. The root's is
// an unknown command (unknownCommand); any other command's says it takes no
// arguments.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if !cmd.HasParent() {
		return unknownCommand(cmd, args[0])
	}
	return usagef("%s: takes no arguments; see %s --help", cmd.CommandPath(), cmd.CommandPath())
}

// atMostArgs is the argument check of a command that takes up to n, and hint
// is what to do instead — prompt's "quote the prompt text", for the unquoted
// prompt that is everyone's first mistake.
func atMostArgs(n int, hint string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) <= n {
			return nil
		}
		takes := "takes no arguments"
		switch {
		case n == 1:
			takes = "takes one argument at most"
		case n > 1:
			takes = fmt.Sprintf("takes %d arguments at most", n)
		}
		if hint != "" {
			takes += " (" + hint + ")"
		}
		return usagef("%s: %s; see %s --help", cmd.CommandPath(), takes, cmd.CommandPath())
	}
}

// unknownCommand is the root's refusal of a word that names no command:
// never the word itself, but the command it is closest to when there is one
// (cobra's own suggestions — a craze command's name, never the argument).
func unknownCommand(root *cobra.Command, word string) error {
	if root.SuggestionsMinimumDistance <= 0 {
		root.SuggestionsMinimumDistance = 2
	}
	msg := "craze: unknown command; see craze --help"
	if names := root.SuggestionsFor(word); len(names) > 0 {
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		msg += " (did you mean " + joinOr(quoted) + "?)"
	}
	return usagef("%s", msg)
}

// usageFlagError is every command's error for a flag that does not parse —
// unknown, missing its value, or a value its type cannot take — set on the
// root, so every subcommand inherits it (cobra's FlagErrorFunc). It never
// names the flag: a key pasted as `--sk-…` is the likeliest unknown flag of
// all. The one exception is a command craze spawns itself (spawnedCommand):
// there the flag's name — the token up to "=", never a value — is what a
// host log needs to say which flag a newer launcher passed after an upgrade
// (plan 037 CR-17), and no person typed it.
func usageFlagError(cmd *cobra.Command, err error) error {
	path := cmd.CommandPath()
	if spawnedCommand(cmd) {
		if name := badFlagName(err); name != "" {
			return usagef("%s: unknown or malformed flag %s; see %s --help", path, name, path)
		}
	}
	return usagef("%s: unknown or malformed flag; see %s --help", path, path)
}

// spawnedCommand is whether cmd is one craze runs itself rather than one a
// person types: a hidden command (craze hub, craze frame), or craze serve
// started by a spawner, whose marks are still in the environment while its
// flags parse (takeReadyPipe takes them later, in RunE).
func spawnedCommand(cmd *cobra.Command) bool {
	if cmd.Hidden {
		return true
	}
	if cmd.Name() != "serve" || !cmd.HasParent() || cmd.Parent().HasParent() {
		return false
	}
	_, child := os.LookupEnv(hostspawn.HostChildEnv)
	_, ready := os.LookupEnv(hostspawn.ReadyFDEnv)
	return child || ready
}

// badFlagName is the flag err is about, as it was spelled up to any "=" —
// "--name", or "-x" for a shorthand — and "" when err says none.
func badFlagName(err error) string {
	var (
		notExist *pflag.NotExistError
		required *pflag.ValueRequiredError
		invalid  *pflag.InvalidValueError
		syntax   *pflag.InvalidSyntaxError
	)
	switch {
	case errors.As(err, &notExist):
		return dashed(notExist.GetSpecifiedName(), notExist.GetSpecifiedShortnames())
	case errors.As(err, &required):
		return dashed(required.GetSpecifiedName(), required.GetSpecifiedShortnames())
	case errors.As(err, &invalid) && invalid.GetFlag() != nil:
		return "--" + invalid.GetFlag().Name
	case errors.As(err, &syntax):
		name, _, _ := strings.Cut(syntax.GetSpecifiedFlag(), "=")
		return sanitizeLine(name)
	}
	return ""
}

// dashed is a flag's name as it was typed: "-x" when it came in a group of
// shorthands, else "--name". pflag's name for a long flag is already cut at
// "=".
func dashed(name, shorthands string) string {
	if shorthands != "" {
		return "-" + sanitizeLine(name)
	}
	return "--" + sanitizeLine(name)
}

// withoutEchoingArgs gives cobra's generated completion commands craze's
// argument check: they would otherwise answer a surplus argument with cobra's
// NoArgs, which quotes it. So they are made here, rather than when the root
// runs (cobra's ExecuteC makes them only if none exist), and root must hold
// every other command already, as their construction reads them.
//
// Each script is written to the writer the root's stdout was at
// construction, so the root is given rootStdout for that moment: a writer
// that is whatever the root's stdout is when the script is written — the
// process's, or the one a test set later.
func withoutEchoingArgs(root *cobra.Command) {
	root.SetOut(rootStdout{root})
	root.InitDefaultCompletionCmd()
	root.SetOut(nil)
	for _, c := range root.Commands() {
		if c.Name() != "completion" {
			continue
		}
		// The parent is made runnable: cobra answers a command that is not
		// with its help before it checks the arguments, so a surplus one
		// would print help and exit 0 (astra r11). Bare, it is still help.
		c.Args = noArgs
		c.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
		for _, sub := range c.Commands() {
			sub.Args = noArgs
		}
	}
}

// isCompletionRequest is whether cmd is cobra's hidden request for a shell's
// completions — cobra.ShellCompRequestCmd, called as itself or as its
// no-description alias — which cobra adds to the root only when it is run.
func isCompletionRequest(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == cobra.ShellCompRequestCmd && cmd.HasParent() && !cmd.Parent().HasParent()
}

// completionRequestUsage is a completion request's usage error: what it
// takes, never what it was given.
func completionRequestUsage(cmd *cobra.Command, what string) error {
	path := cmd.Root().Name() + " " + cmd.CalledAs()
	return usagef("%s: %s; see %s --help", path, what, path)
}

// checkCompletionRequest refuses a completion request whose command line
// cobra's completion would report as an error, before cobra's parser can say
// so (astra r11): cobra prints that failure — the arguments it was given and
// the flag parser's error, which quotes a flag's value, an unknown flag or a
// cluster of shorthands — on stderr, where a pasted key would be echoed like
// anywhere else. It refuses exactly what cobra v1.10.2's getCompletions
// would, and nothing else (astra r14): the command is found and its flags
// parsed as cobra does it — on a probe, a command tree of its own, so nothing
// here sets a flag the real completion then parses again — after the flag
// being completed, if one is, is set aside (completionFlagCheck); and a flag
// being completed that the command does not have is refused only where cobra
// reports it, which is not after a "--" (flag completion is off there). A
// command that parses no flags of its own is never refused. A line that cobra
// completes is left to cobra, whose completions and directive are unchanged.
func checkCompletionRequest(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return completionRequestUsage(cmd, "takes the command line to complete")
	}
	probe := NewRootCmd()
	probe.InitDefaultHelpCmd()
	final, finalArgs, err := probe.Find(slices.Clone(args[:len(args)-1]))
	if err != nil {
		return completionRequestUsage(cmd, "unknown command")
	}
	if final.DisableFlagParsing {
		return nil
	}
	final.InitDefaultHelpFlag()
	final.InitDefaultVersionFlag()
	parse, unknownFlag := completionFlagCheck(final, finalArgs, args[len(args)-1])
	// cobra's own count of what a "--" would add: more arguments with one
	// appended means flag completion is off (a "--" came already).
	_ = final.ParseFlags(append(slices.Clone(parse), "--"))
	withDash := final.Flags().NArg()
	if final.ParseFlags(parse) != nil {
		return completionRequestUsage(cmd, "unknown or malformed flag")
	}
	if unknownFlag && withDash <= final.Flags().NArg() {
		return completionRequestUsage(cmd, "unknown or malformed flag")
	}
	return nil
}

// completionFlagCheck is cobra's checkIfFlagCompletion, for what it decides
// that can fail: the arguments the completion parses — less a trailing flag
// whose value last is being typed — and whether the flag being completed is
// one final does not have, which cobra reports when flag completion is on.
// An empty flag name ("--=x") completes no flag, as in cobra.
func completionFlagCheck(final *cobra.Command, args []string, last string) (parse []string, unknownFlag bool) {
	var name string
	withEqual := false
	if len(last) > 0 && last[0] == '-' {
		i := strings.Index(last, "=")
		if i < 0 {
			return args, false // a flag's name is being typed
		}
		if strings.HasPrefix(last[:i], "--") {
			name = last[2:i]
		} else {
			name = last[i-1 : i]
		}
		withEqual = true
	}
	parse = args
	if name == "" && len(args) > 0 {
		if prev := args[len(args)-1]; isFlagArg(prev) && !strings.Contains(prev, "=") {
			if strings.HasPrefix(prev, "--") {
				name = prev[2:]
			} else {
				name = prev[len(prev)-1:]
			}
			parse = args[:len(args)-1]
		}
	}
	if name == "" {
		return parse, false
	}
	f := completionFlag(final, name)
	if f == nil {
		return args, true
	}
	if !withEqual && f.NoOptDefVal != "" {
		return args, false // a flag that takes no value: last is an argument
	}
	return parse, false
}

// isFlagArg is cobra's test for an argument that is a flag.
func isFlagArg(arg string) bool {
	return (len(arg) >= 3 && arg[:2] == "--") || (len(arg) >= 2 && arg[0] == '-' && arg[1] != '-')
}

// completionFlag is the flag cobra's completion finds by name (its
// findFlag): a one-letter name is a shorthand, the command's own or one it
// inherits.
func completionFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if len(name) == 1 {
		f := cmd.Flags().ShorthandLookup(name)
		if f == nil {
			f = cmd.InheritedFlags().ShorthandLookup(name)
		}
		if f == nil {
			return nil
		}
		name = f.Name
	}
	return cmd.Flag(name)
}

// rootStdout writes to the root command's stdout as it is at the write.
type rootStdout struct{ root *cobra.Command }

func (w rootStdout) Write(p []byte) (int, error) { return w.root.OutOrStdout().Write(p) }
