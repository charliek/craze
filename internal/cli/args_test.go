package cli

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/hostspawn"
)

// argsKey is what a person pastes at the wrong prompt: none of the command
// line's usage errors may say it back (plan 037 LC-1).
const argsKey = "sk-pasted-key-0123456789"

// TestUsageErrorsNeverEchoWhatTheyWereGiven is LC-1: an unknown command
// word, an argument a command does not take, and a flag that does not parse
// are each a usage error — exit 2, one line in the shape every usage error
// has — that says what the command takes and where its help is, and never
// what it was given. bridge keeps its own contract: the same line, exit 1.
func TestUsageErrorsNeverEchoWhatTheyWereGiven(t *testing.T) {
	serveHome(t)
	for _, tc := range []struct {
		argv []string
		code int
		want string
	}{
		{[]string{argsKey}, 2, "craze: unknown command; see craze --help"},
		{[]string{argsKey, "more"}, 2, "craze: unknown command; see craze --help"},
		{[]string{"--" + argsKey}, 2, "craze: unknown or malformed flag; see craze --help"},
		{[]string{"--" + argsKey + "=" + argsKey}, 2, "craze: unknown or malformed flag; see craze --help"},
		{[]string{"-x"}, 2, "craze: unknown or malformed flag; see craze --help"},
		{[]string{"ps", argsKey}, 2, "craze ps: takes no arguments; see craze ps --help"},
		{[]string{"attach", argsKey}, 2, "craze attach: takes no arguments; see craze attach --help"},
		{[]string{"serve", argsKey}, 2, "craze serve: takes no arguments; see craze serve --help"},
		{[]string{"version", argsKey}, 2, "craze version: takes no arguments; see craze version --help"},
		{[]string{"prompt", "fix", "the", "bug"}, 2,
			"craze prompt: takes one argument at most (quote the prompt text); see craze prompt --help"},
		{[]string{"prompt", "--" + argsKey}, 2, "craze prompt: unknown or malformed flag; see craze prompt --help"},
		{[]string{"new", "--" + argsKey}, 2, "craze new: unknown or malformed flag; see craze new --help"},
		{[]string{"ps", "--json=" + argsKey}, 2, "craze ps: unknown or malformed flag; see craze ps --help"},
		{[]string{"serve", "--" + argsKey}, 2, "craze serve: unknown or malformed flag; see craze serve --help"},
		{[]string{"providers", "--" + argsKey}, 2, "craze providers: unknown or malformed flag; see craze providers --help"},
		{[]string{"auth", "login", "--" + argsKey}, 2, "craze auth login: unknown or malformed flag; see craze auth login --help"},
		{[]string{"completion", "bash", argsKey}, 2,
			"craze completion bash: takes no arguments; see craze completion bash --help"},
		// The parent is runnable, so its argument check runs (astra r11).
		{[]string{"completion", argsKey}, 2, "craze completion: takes no arguments; see craze completion --help"},
		{[]string{"completion", "zsh", "--" + argsKey}, 2,
			"craze completion zsh: unknown or malformed flag; see craze completion zsh --help"},
		// bridge's own contract (root.go's diagnose): one "craze bridge: "
		// line, exit 1, whatever failed.
		{[]string{"bridge", argsKey}, 1, "craze bridge: takes no arguments; see craze bridge --help"},
		{[]string{"bridge", "--" + argsKey}, 1, "craze bridge: unknown or malformed flag; see craze bridge --help"},
	} {
		stdout, stderr, code := executeErr(tc.argv)
		if code != tc.code || stdout != "" || stderr != tc.want+"\n" {
			t.Errorf("craze %q: exit %d, stdout %q, stderr %q; want %d, %q", tc.argv, code, stdout, stderr, tc.code, tc.want)
		}
		if strings.Contains(stdout+stderr, argsKey) {
			t.Errorf("craze %q echoed what it was given: %q", tc.argv, stdout+stderr)
		}
	}
}

// TestUnknownCommandSuggestsACommand: a word close to a command's name gets
// that name — a craze command's, never the word — and the exit and the rest
// of the line are an unknown command's.
func TestUnknownCommandSuggestsACommand(t *testing.T) {
	serveHome(t)
	for _, tc := range []struct{ word, want string }{
		{"pss", `craze: unknown command; see craze --help (did you mean "ps"?)`},
		{"atach", `craze: unknown command; see craze --help (did you mean "attach"?)`},
		{"prov", `craze: unknown command; see craze --help (did you mean "providers"?)`},
	} {
		stdout, stderr, code := executeErr([]string{tc.word})
		if code != 2 || stdout != "" || stderr != tc.want+"\n" {
			t.Errorf("craze %s: exit %d, stdout %q, stderr %q; want 2, %q", tc.word, code, stdout, stderr, tc.want)
		}
	}
}

// TestSpawnedCommandsNameTheBadFlag is CR-17: a command craze spawns itself —
// the hidden hub and frame, and craze serve when a spawner's marks are in its
// environment — names the flag it could not parse, by its name up to any
// "=", so a host log says which flag a newer launcher passed. Never a value.
// craze serve run by hand names none.
func TestSpawnedCommandsNameTheBadFlag(t *testing.T) {
	serveHome(t)
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"hub", "--bogus=" + argsKey}, "craze hub: unknown or malformed flag --bogus; see craze hub --help"},
		{[]string{"hub", "--bogus", argsKey}, "craze hub: unknown or malformed flag --bogus; see craze hub --help"},
		{[]string{"hub", "-z"}, "craze hub: unknown or malformed flag -z; see craze hub --help"},
		{[]string{"frame", "--cols=" + argsKey}, "craze frame: unknown or malformed flag --cols; see craze frame --help"},
		{[]string{"frame", "--keys"}, "craze frame: unknown or malformed flag --keys; see craze frame --help"},
		{[]string{"frame", "---x=" + argsKey}, "craze frame: unknown or malformed flag ---x; see craze frame --help"},
	} {
		stdout, stderr, code := executeErr(tc.argv)
		if code != 2 || stdout != "" || stderr != tc.want+"\n" {
			t.Errorf("craze %q: exit %d, stdout %q, stderr %q; want 2, %q", tc.argv, code, stdout, stderr, tc.want)
		}
		if strings.Contains(stderr, argsKey) {
			t.Errorf("craze %q echoed a value: %q", tc.argv, stderr)
		}
	}

	argv := []string{"serve", "--newer-flag=" + argsKey}
	_, stderr, code := executeErr(argv)
	if want := "craze serve: unknown or malformed flag; see craze serve --help\n"; code != 2 || stderr != want {
		t.Fatalf("craze serve by hand: exit %d, %q; want 2, %q", code, stderr, want)
	}
	for _, mark := range []string{hostspawn.HostChildEnv, hostspawn.ReadyFDEnv} {
		t.Run(mark, func(t *testing.T) {
			t.Setenv(mark, "1")
			_, stderr, code := executeErr(argv)
			if want := "craze serve: unknown or malformed flag --newer-flag; see craze serve --help\n"; code != 2 || stderr != want {
				t.Fatalf("a spawned craze serve: exit %d, %q; want 2, %q", code, stderr, want)
			}
		})
	}
}

// TestUsageErrorsLeaveTheRestAlone: what LC-1 does not cover still answers as
// it did — --version and its short form, help for any topic (a word that
// names no command is the root's help, and is not quoted), the completion
// scripts, and craze new's words, which are its prompt.
func TestUsageErrorsLeaveTheRestAlone(t *testing.T) {
	serveHome(t)
	for _, argv := range [][]string{{"--version"}, {"-v"}, {"--version", "extra"}} {
		if stdout, stderr, code := executeErr(argv); code != 0 || strings.TrimSpace(stdout) == "" || stderr != "" {
			t.Errorf("craze %q: exit %d, stdout %q, stderr %q", argv, code, stdout, stderr)
		}
	}
	stdout, stderr, code := executeErr([]string{"help", argsKey})
	if code != 0 || !strings.Contains(stdout, "Usage:") || strings.Contains(stdout+stderr, argsKey) {
		t.Errorf("craze help <word>: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = executeErr([]string{"completion", "bash"})
	if code != 0 || !strings.Contains(stdout, "craze") || stderr != "" {
		t.Errorf("craze completion bash: exit %d, %d bytes, stderr %q", code, len(stdout), stderr)
	}
	stdout, stderr, code = executeErr([]string{"completion"})
	if code != 0 || !strings.Contains(stdout, "bash") || stderr != "" {
		t.Errorf("craze completion: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestCompletionRequestsNeverEchoWhatTheyWereGiven is astra r11's P2: the
// shells' completion requests — cobra's hidden __complete and its
// no-description alias — parse the command line being completed with
// cobra's own parser, which prints what it was given and the parser's error
// on stderr. So a line that would not parse is refused first, a usage error
// in craze's words, exit 2, never naming a value, an unknown flag or a
// cluster of shorthands; so is a request with nothing to complete. A line
// that parses is completed as cobra always completed it.
func TestCompletionRequestsNeverEchoWhatTheyWereGiven(t *testing.T) {
	serveHome(t)
	for _, req := range []string{"__complete", "__completeNoDesc"} {
		want := "craze " + req + ": unknown or malformed flag; see craze " + req + " --help\n"
		for _, argv := range [][]string{
			{req, "ps", "--json=" + argsKey, ""},
			{req, "ps", "--" + argsKey, ""},
			{req, "ps", "--" + argsKey + "=x"},
			{req, "-x" + argsKey[:3], ""},
			{req, "prompt", "-" + argsKey[:4], ""},
		} {
			stdout, stderr, code := executeErr(argv)
			if code != 2 || stdout != "" || stderr != want {
				t.Errorf("craze %q: exit %d, stdout %q, stderr %q; want 2, %q", argv, code, stdout, stderr, want)
			}
		}
		stdout, stderr, code := executeErr([]string{req})
		if want := "craze " + req + ": takes the command line to complete; see craze " + req + " --help\n"; code != 2 || stdout != "" || stderr != want {
			t.Errorf("craze %s with nothing to complete: exit %d, stdout %q, stderr %q; want 2, %q", req, code, stdout, stderr, want)
		}
	}
	// What cobra completes is completed exactly as it was before C6 (the
	// outputs are the base binary's, astra r14): the commands, a flag's name,
	// a flag's value, a flag that takes none — and, after a "--", where flag
	// completion is off, an unknown flag or a cluster, which cobra takes as
	// words; so is a flag with no name.
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"__completeNoDesc", "pro"}, "prompt\nproviders\n:4\n"},
		{[]string{"__complete", "ps", "--js"}, "--json\tprint the hub's roster (sessions.list's result) as JSON\n:4\n"},
		{[]string{"__completeNoDesc", "ps", ""}, ":0\n"},
		{[]string{"__complete", "--provider", ""}, ":0\n"},
		{[]string{"__completeNoDesc", "completion", ""}, "bash\nfish\npowershell\nzsh\n:4\n"},
		{[]string{"__complete", "--=x"}, ":4\n"},
		{[]string{"__completeNoDesc", "prompt", "--=x"}, ":0\n"},
	} {
		stdout, stderr, code := executeErr(tc.argv)
		if code != 0 || stdout != tc.want || !strings.HasPrefix(stderr, "Completion ended with directive") {
			t.Errorf("craze %q: exit %d, stdout %q, stderr %q; want 0, %q", tc.argv, code, stdout, stderr, tc.want)
		}
	}
	for _, req := range []string{"__complete", "__completeNoDesc"} {
		for _, argv := range [][]string{
			{req, "prompt", "--", "--unknown=value"},
			{req, "prompt", "--", "--unknown", ""},
			{req, "prompt", "--", "-xyz", ""},
			{req, "prompt", "--", "--" + argsKey + "=x"},
			{req, "--", "-xyz", ""},
		} {
			stdout, stderr, code := executeErr(argv)
			if code != 0 || stdout != ":0\n" || !strings.HasPrefix(stderr, "Completion ended with directive") {
				t.Errorf("craze %q: exit %d, stdout %q, stderr %q; want 0, \":0\\n\"", argv, code, stdout, stderr)
			}
		}
	}
}
