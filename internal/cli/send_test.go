package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/protocol"
)

// craze send against a live session (plan 041 §3.2, §7 W2 12 and 16): the
// waiter's rig (turnwait_test.go) — an engine over a scripted session behind
// a real control server, its socket in a test HOME's registry — with the
// prompt-input rules craze new --wait shares (prompt_input.go).

// executeIn is executeErr with stdin the command reads from.
func executeIn(argv []string, stdin io.Reader) (stdout, stderr string, code int) {
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(stdin)
	root.SetArgs(argv)
	ranCmd, err := root.ExecuteC()
	if err == nil {
		return out.String(), errBuf.String(), 0
	}
	line, c := diagnose(ranCmd, err)
	stderrOut := errBuf.String()
	if line != "" {
		stderrOut += line + "\n"
	}
	return out.String(), stderrOut, c
}

// sendNext is the handle a send's `next:` line names, failing the test when
// the output is not `<first>\nnext: craze wait <short> --from <handle>\n`.
func sendNext(t *testing.T, stdout, first string) turnHandle {
	t.Helper()
	m := regexp.MustCompile(`^` + regexp.QuoteMeta(first) + `\nnext: craze wait ` + waitRigShort + ` --from (\S+)\n$`).FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("craze send printed %q, want %q and its next line", stdout, first)
	}
	h, err := parseHandle(m[1])
	if err != nil {
		t.Fatalf("the handle %q: %v", m[1], err)
	}
	return h
}

// TestSendStartsQueuesAndWaitsForItsOwnTurn is §7 W2 12's first clause:
// craze send without --wait prints `started <turn>` on an idle session and
// `queued <row> (position <n>)` on a busy one, each with the command that
// follows it by its handle; send --wait on the busy session waits for its own
// prompt's turn — not the one queued ahead of it — and each handle, followed
// later, reports exactly its own turn, whole.
func TestSendStartsQueuesAndWaitsForItsOwnTurn(t *testing.T) {
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(heldTurn(g, textEv("the first")), answerTurn(textEv("the second")), answerTurn(textEv("the third")))

	stdout, stderr, code := executeErr([]string{"send", waitRigShort, "one"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze send on an idle session: exit %d, %q %q", code, stdout, stderr)
	}
	first := sendNext(t, stdout, "started turn-1")
	if first.target != (waitTarget{kind: targetTurn, id: "turn-1"}) {
		t.Fatalf("the started prompt's handle names %+v", first.target)
	}
	r.waitStarted("turn-1")
	stdout, stderr, code = executeErr([]string{"send", waitRigShort, "two"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze send on a busy session: exit %d, %q %q", code, stdout, stderr)
	}
	second := sendNext(t, stdout, "queued q-1 (position 1)")
	if second.target != (waitTarget{kind: targetRow, id: "q-1"}) {
		t.Fatalf("the queued prompt's handle names %+v", second.target)
	}

	third := goCraze("send", waitRigShort, "--wait", "--json", "three")
	r.waitQueued(2)
	g.open()
	stdout, stderr, code = third.wait(t)
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["turn"] != "turn-3" || m["row"] != "q-2" || m["message"] != "the third" || stderr != "" {
		t.Fatalf("send --wait behind two turns: exit %d, %v, %q", code, m, stderr)
	}
	if got := r.s.sent(); !slices.Equal(got, []string{"one", "two", "three"}) {
		t.Fatalf("the session was prompted %q", got)
	}
	for _, c := range []struct {
		h          turnHandle
		turn, text string
	}{{first, "turn-1", "the first"}, {second, "turn-2", "the second"}} {
		stdout, stderr, code = executeErr([]string{"wait", waitRigShort, "--from", c.h.String(), "--json"})
		m = decodeResult(t, stdout)
		if code != 0 || m["status"] != "completed" || m["turn"] != c.turn || m["message"] != c.text || m["partial"] != nil {
			t.Fatalf("wait --from %s: exit %d, %v, %q", c.h, code, m, stderr)
		}
	}
}

// TestSendsHandleIsJustBeforeItsTurn is X1 for craze send without --wait: the
// handle's place is just before its turn's `started`, not where the command
// attached — so a turn the agent ran on its own between the two, its tool
// call and its text, is no part of what `wait --from` reports: the prompt's
// turn wrote no text and made no call, and the result is whole.
func TestSendsHandleIsJustBeforeItsTurn(t *testing.T) {
	r := newWaitRig(t, 0, endTurn())
	conn, tp := r.dialTapped()
	tp.on(`"method":"`+protocol.MethodSessionPrompt+`"`, func() bool {
		// After the attach, before the prompt: the agent's own turn.
		r.s.setForeign(true)
		r.s.emit(toolEv("f1", "read", "completed"))
		r.s.emit(textEv("the agent's own words"))
		r.s.setForeign(false)
		return true
	})
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	job := newPromptJob(cmd, sendWhat, "mine", false, time.Now(), 0, false, 0, make(chan os.Signal))
	defer job.close()
	if err := job.sendOnly(conn); err != nil {
		t.Fatalf("send: %v", err)
	}
	h := sendNext(t, out.String(), "started turn-1")
	r.waitIdle()
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--from", h.String(), "--json"})
	m := decodeResult(t, stdout)
	if code != 0 || m["turn"] != "turn-1" || m["message"] != "" || m["toolCalls"] != float64(0) || m["partial"] != nil {
		t.Fatalf("wait --from send's handle: exit %d, %v, %q", code, m, stderr)
	}
}

// TestSendJSONWithoutWait: --json without --wait is one object — status
// started or queued, the session, the turn or the row and its place, the
// handle.
func TestSendJSONWithoutWait(t *testing.T) {
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(heldTurn(g, textEv("the first")), answerTurn(textEv("the second")))
	stdout, _, code := executeErr([]string{"send", waitRigShort, "--json", "one"})
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "started" || m["turn"] != "turn-1" || m["session"] != waitRigID || m["short"] != waitRigShort ||
		m["handle"] == nil || m["row"] != nil || m["position"] != nil || m["attach"] != "craze attach --session "+waitRigShort {
		t.Fatalf("started --json: exit %d, %v", code, m)
	}
	r.waitStarted("turn-1")
	stdout, _, code = executeErr([]string{"send", waitRigShort, "--json", "two"})
	m = decodeResult(t, stdout)
	if code != 0 || m["status"] != "queued" || m["row"] != "q-1" || m["position"] != float64(1) || m["turn"] != nil || m["handle"] == nil {
		t.Fatalf("queued --json: exit %d, %v", code, m)
	}
	g.open()
	r.waitIdle()
}

// TestPromptInputRules is §7 W2 12's prompt-input clause, through craze send
// (craze new --wait reads its prompt the same way, before it spawns
// anything): one source — words, --prompt-file (`-` for stdin), or stdin when
// it is not a terminal; a file and words together, none (stdin a terminal —
// /dev/null is a character device, which is what craze takes a terminal to
// be), an empty prompt and one over 32 KiB are usage errors, exit 2, and
// nothing is sent; --remove-prompt-file deletes the file once it is read.
func TestPromptInputRules(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("taken")))
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	task := write("task.md", "  from a file\n")
	empty := write("empty.md", " \n\t\n")
	big := write("big.md", strings.Repeat("x", promptTextMax+1))
	for _, tc := range []struct {
		name  string
		argv  []string
		stdin io.Reader
		want  string // the usage error's text
	}{
		{"a file and words", []string{"--prompt-file", task, "words"}, nil, "give the prompt as words or as --prompt-file, not both"},
		{"none, stdin a terminal", nil, devNull, "needs a prompt"},
		{"empty words", []string{"  "}, nil, "the prompt is empty"},
		{"an empty file", []string{"--prompt-file", empty}, nil, "the prompt is empty"},
		{"empty stdin", []string{"--prompt-file", "-"}, strings.NewReader("\n"), "the prompt is empty"},
		{"over 32 KiB", []string{"--prompt-file", big}, nil, "the prompt is over 32 KiB"},
		{"a missing file", []string{"--prompt-file", filepath.Join(dir, "nope")}, nil, "the prompt cannot be read"},
		{"remove with no file", []string{"--remove-prompt-file", "words"}, nil, "--remove-prompt-file needs --prompt-file"},
		{"remove stdin", []string{"--prompt-file", "-", "--remove-prompt-file"}, strings.NewReader("x"), "removes a file, not stdin"},
		{"max-message with no wait", []string{"--max-message", "5", "words"}, nil, "--max-message needs --wait"},
	} {
		in := tc.stdin
		if in == nil {
			in = devNull
		}
		stdout, stderr, code := executeIn(append([]string{"send", waitRigShort}, tc.argv...), in)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "craze send: ") || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit %d, %q %q; want a usage error naming %q", tc.name, code, stdout, stderr, tc.want)
		}
	}
	if got := r.s.sent(); len(got) != 0 {
		t.Fatalf("a refused prompt reached the session: %q", got)
	}
	if _, err := os.Stat(big); err != nil {
		t.Fatalf("a file with no --remove-prompt-file was removed: %v", err)
	}

	for i, tc := range []struct {
		name  string
		argv  []string
		stdin io.Reader
		want  string
	}{
		{"words", []string{"two", "words"}, devNull, "two words"},
		{"a file, removed once read", []string{"--prompt-file", task, "--remove-prompt-file"}, devNull, "from a file"},
		{"stdin by -", []string{"--prompt-file", "-"}, strings.NewReader("by dash\n"), "by dash"},
		{"stdin, not a terminal", nil, strings.NewReader("  piped in\n"), "piped in"},
		{"words, stdin left unread", []string{"the", "words"}, strings.NewReader("not this"), "the words"},
	} {
		stdout, stderr, code := executeIn(append([]string{"send", waitRigShort, "--wait"}, tc.argv...), tc.stdin)
		if code != 0 || stderr != "" || !strings.Contains(stdout, "\ncompleted · ") {
			t.Fatalf("%s: exit %d, %q %q", tc.name, code, stdout, stderr)
		}
		if got := r.s.sent(); len(got) != i+1 || got[i] != tc.want {
			t.Fatalf("%s: the session was prompted %q, want %q last", tc.name, got, tc.want)
		}
	}
	if _, err := os.Stat(task); !os.IsNotExist(err) {
		t.Fatalf("--remove-prompt-file left the file: %v", err)
	}
}

// TestNewWaitOnlyFlagsNeedWait: on plain craze new each flag only --wait
// takes is a usage error — before any hub is asked for.
func TestNewWaitOnlyFlagsNeedWait(t *testing.T) {
	serveHome(t)
	for _, argv := range [][]string{{"--timeout", "1s"}, {"--max-message", "5"}, {"--prompt-file", "x"},
		{"--remove-prompt-file"}, {"--plan"}, {"--ask"}} {
		stdout, stderr, code := executeErr(append([]string{"new", "--provider", "cursor", "-C", t.TempDir()}, argv...))
		if code != 2 || stdout != "" || stderr != "craze new: "+argv[0]+" needs --wait\n" {
			t.Errorf("plain craze new %v: exit %d, %q %q", argv, code, stdout, stderr)
		}
	}
	for _, argv := range [][]string{{"--plan", "--ask"}, {"--fast", "--no-fast"}, {"--timeout", "-1s"}, {"--max-message", "-1"}} {
		stdout, stderr, code := executeErr(append([]string{"new", "--wait", "--provider", "cursor", "-C", t.TempDir(), "task"}, argv...))
		if code != 2 || stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "craze new: ") {
			t.Errorf("craze new --wait %v: exit %d, %q %q", argv, code, stdout, stderr)
		}
	}
}

// TestSendWaitSignalDropsItsQueuedPrompt is §3.1 "Signals" on send --wait,
// an owned form: a signal while its prompt waits behind another turn removes
// that row — `dropped`, exit 3 — and the turn ahead of it runs on.
func TestSendWaitSignalDropsItsQueuedPrompt(t *testing.T) {
	r := newWaitRig(t, 0)
	sigs := withWaitSignals(t)
	// The signal may come before the prompt's answer is in, which the
	// cleanup then waits for: long, so a starved run still sees it.
	prev := cleanupWait
	cleanupWait = serveStep
	t.Cleanup(func() { cleanupWait = prev })
	g := r.gate()
	r.script(heldTurn(g, textEv("the first")), answerTurn(textEv("never")))
	r.submit("someone else's")
	r.waitStarted("turn-1")
	w := goCraze("send", waitRigShort, "--wait", "--json", "mine")
	r.waitQueued(1)
	sigs <- syscall.SIGTERM
	stdout, stderr, code := w.wait(t)
	m := decodeResult(t, stdout)
	if code != 3 || m["status"] != "dropped" || m["row"] != "q-1" || stderr != "" {
		t.Fatalf("send --wait's signal: exit %d, %v, %q", code, m, stderr)
	}
	if st := r.eng.State(); st.Turn != "turn-1" || len(st.Queue) != 0 {
		t.Fatalf("after the signal: turn %q, queue %d", st.Turn, len(st.Queue))
	}
	g.open()
	r.waitIdle()
	if got := r.s.sent(); !slices.Equal(got, []string{"someone else's"}) {
		t.Fatalf("the session was prompted %q", got)
	}
}

// TestSendOutcomeUnknownIsNotResent is §7 W2 16: a prompt whose answer was
// lost with its connection — written, and the client moved to another
// identity (the host replaced) before the answer — is never sent again:
// exit 1, `the prompt may not have reached it`, naming craze wait, and
// neither host ran it twice. The prompt goes out through the command's own
// gate and wait (promptJob.waitFor) over a connection the test watches.
func TestSendOutcomeUnknownIsNotResent(t *testing.T) {
	for _, wait := range []bool{true, false} {
		t.Run(map[bool]string{true: "--wait", false: "no wait"}[wait], func(t *testing.T) {
			r := newWaitRig(t, 0)
			sB, engB := r.newEngine(0, answerTurn(textEv("B's")))
			conn, tp := r.dialTapped()
			tp.on(`"method":"`+protocol.MethodSessionPrompt+`"`, func() bool {
				// Written as far as the client knows, and never answered: the
				// host is replaced under it.
				r.srv.SetEngine(engB)
				tp.drop()
				return false
			})
			var out, errBuf bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetErr(&errBuf)
			job := newPromptJob(cmd, sendWhat, "mine", wait, time.Now(), 0, true, 0, make(chan os.Signal))
			defer job.close()
			job.session = resultSession{id: conn.sessionID, provider: conn.entry.Provider}
			var err error
			if wait {
				err = job.waitFor(conn)
			} else {
				err = job.sendOnly(conn)
			}
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 1 || !strings.Contains(ee.msg, "the prompt may not have reached it") ||
				!strings.Contains(ee.msg, "craze wait "+waitRigShort) {
				t.Fatalf("the lost answer: %v; stdout %q", err, out.String())
			}
			if m := decodeResult(t, out.String()); m["error"] == nil {
				t.Fatalf("--json printed %v", m)
			}
			if got := r.s.sent(); len(got) != 0 {
				t.Fatalf("A ran %q", got)
			}
			if got := sB.sent(); len(got) != 0 {
				t.Fatalf("the prompt was sent again, to B: %q", got)
			}
		})
	}
}
