package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze new --wait and craze send's reload with real hosts (plan 041 §3.2,
// §3.4, §7 W2 11, 13, 15): every host a command spawns is this test binary
// run as craze serve (spawnAsChild), over the fake ACP agent, which it finds
// through `[agents]` — no agent binary crosses to a host spawned this way, as
// none crosses to a hub's.

// unattendedHome is a test HOME whose hosts are spawned as children of the
// test binary, with the fake agent as cursor in its config; every host left
// in its registry is stopped when the test ends.
func unattendedHome(t *testing.T) (rundir.Env, string, *childCmds) {
	t.Helper()
	_, ws := serveHome(t)
	// A craze directory of its own (writeCrazeConfig's), so the environment
	// is read again after it.
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t)))
	env := rundir.ProcessEnv()
	kids := spawnAsChild(t, nil)
	// No test here is about a bound of the handshake's: each is serveStep,
	// as the spawn tests' are, so a starved run is not timed out.
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	return env, ws, kids
}

// stopEveryHost stops every host in env's registry and waits for each to go.
func stopEveryHost(t *testing.T, env rundir.Env) {
	t.Helper()
	entries, _ := rundir.Hosts(env)
	for _, e := range entries {
		if stopHost(e) == nil {
			waitReaped(t, e.PID)
		}
	}
}

// cmd is the i-th host command a test built (from 0). Read once the spawn
// that started it has returned to the test.
func (c *childCmds) cmd(t *testing.T, i int) *exec.Cmd {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.cmds) {
		t.Fatalf("host %d was never spawned: %d were", i, len(c.cmds))
	}
	return c.cmds[i]
}

// childArgv is the craze command line a spawned host ran (spawnAsChild hands
// it over as cliChildEnv's JSON).
func childArgv(t *testing.T, cmd *exec.Cmd) []string {
	t.Helper()
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, cliChildEnv+"="); ok {
			var argv []string
			if err := json.Unmarshal([]byte(v), &argv); err != nil {
				t.Fatal(err)
			}
			return argv
		}
	}
	t.Fatalf("no %s in the host's environment", cliChildEnv)
	return nil
}

// checkArgv fails the test unless argv holds every flag of want and starts
// none of dontWant.
func checkArgv(t *testing.T, argv, want, dontWant []string) {
	t.Helper()
	for _, a := range want {
		if !slices.Contains(argv, a) {
			t.Errorf("the host's command line %q lacks %s", argv, a)
		}
	}
	for _, a := range dontWant {
		if slices.ContainsFunc(argv, func(x string) bool { return x == a || strings.HasPrefix(x, a+"=") }) {
			t.Errorf("the host's command line %q passes %s", argv, a)
		}
	}
}

// TestNewWaitSpawnsItsHostWithoutTheHub is §7 W2 11 (P1): craze new --wait
// spawns its host itself — with a hub running, and nothing asked of it: the
// hub's log names no session.create, and the host's entry carries no request
// — and prints the result, exit 0. The host runs with --no-host-status and
// --unattended, no --agent-bin, and the environment the hub hands its hosts:
// none of CRAZE_PROVIDER, CRAZE_AGENT_BIN, TMUX, TMUX_PANE, HERDR_* or
// ROOST_* of the shell that ran the command. Then §7 W2 12's last clause:
// plain craze new, no words, from a stdin that is not a terminal, still
// creates an unprompted session through the hub — whose log now names the
// create, so the look above was not blind.
func TestNewWaitSpawnsItsHostWithoutTheHub(t *testing.T) {
	env, ws, kids := unattendedHome(t)
	hubs := hubAsChild(t, cliChildHubHosts+"=1")
	ensureHub(t, env)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	shell := map[string]string{"CRAZE_PROVIDER": "grok", "CRAZE_AGENT_BIN": "/nowhere/agent", "TMUX": "/tmp/tmux-0/default,1,0",
		"TMUX_PANE": "%1", "HERDR_SOCKET_PATH": "/nowhere/herdr.sock", "ROOST_SESSION_ID": "r-1"}
	for k, v := range shell {
		t.Setenv(k, v)
	}

	stdout, stderr, code := executeErr([]string{"new", "--wait", "--provider", "cursor", "-C", ws, "do", "the", "task"})
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "echo: do the task\n---\ncompleted · ") {
		t.Fatalf("craze new --wait: exit %d, %q %q", code, stdout, stderr)
	}
	if n := kids.count(); n != 1 {
		t.Fatalf("%d hosts spawned, want 1", n)
	}
	host := kids.cmd(t, 0)
	checkArgv(t, childArgv(t, host), []string{"serve", "--no-host-status", "--unattended", "--provider=cursor", "--workspace=" + ws},
		[]string{"--agent-bin", "--request-id", "--request-hash", "--hub-pid", "--load"})
	for _, kv := range host.Env {
		k, _, _ := strings.Cut(kv, "=")
		if _, set := shell[k]; set || strings.HasPrefix(k, "HERDR_") || strings.HasPrefix(k, "ROOST_") {
			t.Errorf("the host was handed %s", kv)
		}
	}
	if !slices.Contains(host.Env, "CRAZE_FAKE_SCRIPT=echo") {
		t.Errorf("the host lost the variables the hub keeps: %q", host.Env)
	}
	entries, err := rundir.Hosts(env)
	if err != nil || len(entries) != 1 || entries[0].Workspace != ws || entries[0].RequestID != "" {
		t.Fatalf("the registry: %+v, %v", entries, err)
	}
	if log := hubLog(env)(); strings.Contains(log, "session.create") {
		t.Fatalf("a session.create reached the hub:\n%s", log)
	}
	if n := len(hubs.pids()); n != 1 {
		t.Fatalf("%d hubs were spawned, want the one the test started", n)
	}

	stdout, stderr, code = executeIn([]string{"new", "--provider", "cursor", "-C", ws, "--json"}, strings.NewReader("not a prompt"))
	var res protocol.CreateResult
	if code != 0 || json.Unmarshal([]byte(stdout), &res) != nil || res.Prompt != protocol.CreatePromptNone {
		t.Fatalf("plain craze new from a pipe: exit %d, %q %q", code, stdout, stderr)
	}
	if log := hubLog(env)(); !strings.Contains(log, "session.create") {
		t.Fatalf("the hub's log names no create after plain craze new:\n%s", log)
	}
}

// askHold is an agent that is the fake agent with its questions held: a
// shell pipeline that passes each line the fake writes to craze, but holds a
// cursor/ask_question line — while the FIFO $CRAZE_TEST_ASK_GATE exists —
// until one byte has been read from it. So a test lets a question reach the
// host exactly when it means to: after the command that started the turn has
// detached (§7 W2 13). The fake is $CRAZE_TEST_FAKE_AGENT; both variables
// reach the agent through the host, as every variable the hub keeps does.
const askHold = `#!/bin/sh
"$CRAZE_TEST_FAKE_AGENT" "$@" | while IFS= read -r line; do
	case "$line" in
	*cursor/ask_question*) if [ -p "$CRAZE_TEST_ASK_GATE" ]; then head -c 1 "$CRAZE_TEST_ASK_GATE" >/dev/null; fi ;;
	esac
	printf '%s\n' "$line"
done
`

// TestAnUnattendedHostAnswersItsQuestionsWithNoClient is §7 W2 13: the fake
// ACP agent's ask script asks two questions every turn; an unattended host
// answers each with its first option — craze new --wait completes, listing
// the answer — and so it does with no client attached: craze send returns
// once its prompt has started; the agent's question is held (askHold) until
// the session has no client attached at all, the turn still running; only
// then does it reach the host, which answers it, and the turn ends with
// nobody there — craze wait from send's handle reports it completed.
func TestAnUnattendedHostAnswersItsQuestionsWithNoClient(t *testing.T) {
	_, ws := serveHome(t)
	agentDir := t.TempDir()
	hold := filepath.Join(agentDir, "ask-hold")
	if err := os.WriteFile(hold, []byte(askHold), 0o700); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(agentDir, "ask-gate")
	t.Setenv("CRAZE_TEST_FAKE_AGENT", fakeAgentPath(t))
	t.Setenv("CRAZE_TEST_ASK_GATE", gate)
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", hold))
	env := rundir.ProcessEnv()
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	t.Setenv("CRAZE_FAKE_SCRIPT", "ask")

	stdout, stderr, code := executeErr([]string{"new", "--wait", "--json", "--provider", "cursor", "-C", ws, "ask me"})
	m := decodeResult(t, stdout)
	wantAuto := []any{map[string]any{"kind": "question", "summary": "Pick one", "choice": "A; X"}}
	auto, _ := json.Marshal(m["autoAnswered"])
	want, _ := json.Marshal(wantAuto)
	if code != 0 || m["status"] != "completed" || m["message"] != "asked:answered:q1=opt-a;q2=opt-x" || string(auto) != string(want) {
		t.Fatalf("craze new --wait over two questions: exit %d, %v, %q", code, m, stderr)
	}
	short, id := m["short"].(string), m["session"].(string)

	// From here the agent's questions wait at the gate.
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = executeErr([]string{"send", short, "again"})
	match := regexp.MustCompile(`^started turn-2\nnext: craze wait ` + short + ` --from (\S+)\n$`).FindStringSubmatch(stdout)
	if code != 0 || match == nil {
		t.Fatalf("craze send: exit %d, %q %q", code, stdout, stderr)
	}
	e, ok := hostServing(env, id)
	if !ok {
		t.Fatal("the session's host is gone")
	}
	waitFor(t, "craze send to detach", func() bool { return listRow(t, e).Attached == 0 })
	// Turn-2 runs still (a last turn is absent while one does), its
	// question not yet asked of the host.
	if row := listRow(t, e); row.Activity != protocol.ActivityWorking || row.LastTurn != nil || row.PendingAsks != 0 {
		t.Fatalf("before the question: %+v", row)
	}
	// The question goes to the host now, with nobody attached.
	g, err := os.OpenFile(gate, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Write([]byte{1})
	_ = g.Close()
	if err != nil {
		t.Fatal(err)
	}
	if lt := waitLastTurn2(t, e); lt.TurnID != "turn-2" || lt.Outcome != protocol.TurnDone {
		t.Fatalf("the unattended turn ended %+v", lt)
	}
	stdout, stderr, code = executeErr([]string{"wait", short, "--from", match[1], "--json"})
	m = decodeResult(t, stdout)
	auto, _ = json.Marshal(m["autoAnswered"])
	if code != 0 || m["status"] != "completed" || m["turn"] != "turn-2" || string(auto) != string(want) || m["partial"] != nil {
		t.Fatalf("craze wait from send's handle: exit %d, %v, %q", code, m, stderr)
	}
}

// waitLastTurn2 is the session's last turn once turn-2 has ended, polled over
// sessions.list (which attaches nothing) within serveStep.
func waitLastTurn2(t *testing.T, e rundir.Entry) *protocol.LastTurn {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		if lt := listRow(t, e).LastTurn; lt != nil && lt.TurnID == "turn-2" {
			return lt
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn-2 did not end within %v: %+v", serveStep, listRow(t, e))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSendReloadsAnEndedSessionWithItsRecordedSettings is §7 W2 15: craze
// new --wait's index row carries `unattended` with its start settings; craze
// send to it once stopped reloads it — `craze serve --load <crazeId>` with
// the recorded mode, --no-force, model and effort, and --unattended — the
// agent asked for the load and then the mode and the model, and spawned
// without --force; the row stays marked. A person's row (no record) is
// reloaded as an ordinary interactive host: none of those flags.
func TestSendReloadsAnEndedSessionWithItsRecordedSettings(t *testing.T) {
	env, ws, kids := unattendedHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	stdout, stderr, code := executeErr([]string{"new", "--wait", "--json", "--provider", "cursor", "--model", "m-1", "--effort", "low",
		"--plan", "--no-force", "-C", ws, "the task"})
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["message"] != "echo: the task" {
		t.Fatalf("craze new --wait: exit %d, %v, %q", code, m, stderr)
	}
	id, short := m["session"].(string), m["short"].(string)
	rec := sessions.Unattended{Mode: "plan", PermissionMode: sessions.PermissionPrompt, Model: "m-1", Effort: "low"}
	if row := waitIndexRowByID(t, id); row.Unattended == nil || *row.Unattended != rec {
		t.Fatalf("the new session's row: %+v", row)
	}
	// The host's own pid, from its entry: the command started it reparented
	// (X32), so the process its spawn built was the intermediate, long gone.
	e, ok := hostServing(env, id)
	if !ok {
		t.Fatalf("no host serves the new session %s", id)
	}
	if stdout, stderr, code = executeErr([]string{"stop", short}); code != 0 || stdout != "stopped "+short+"\n" {
		t.Fatalf("craze stop: exit %d, %q %q", code, stdout, stderr)
	}
	waitReaped(t, e.PID)

	dumps := t.TempDir()
	argvDump, callsDump := filepath.Join(dumps, "argv"), filepath.Join(dumps, "calls")
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	t.Setenv("CRAZE_FAKE_DUMP_ARGV", argvDump)
	t.Setenv("CRAZE_FAKE_DUMP_CALLS", callsDump)
	stdout, stderr, code = executeErr([]string{"send", short, "--wait", "--json", "and again"})
	m = decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["message"] != "echo: and again" || m["session"] != id {
		t.Fatalf("craze send to the stopped session: exit %d, %v, %q", code, m, stderr)
	}
	checkArgv(t, childArgv(t, kids.cmd(t, 1)),
		[]string{"--load=" + id, "--unattended", "--plan", "--no-force", "--model=m-1", "--effort=low", "--no-host-status"},
		[]string{"--provider", "--workspace", "--agent-bin", "--ask", "--fast", "--no-fast"})
	agentArgv, err := os.ReadFile(argvDump)
	if err != nil || slices.Contains(strings.Split(string(agentArgv), "\n"), "--force") {
		t.Fatalf("the reloaded agent's command line: %q, %v", agentArgv, err)
	}
	calls, err := os.ReadFile(callsDump)
	got := strings.Split(strings.TrimSpace(string(calls)), "\n")
	load := slices.Index(got, "session/load")
	if err != nil || load < 0 || !slices.Contains(got[load:], "session/set_mode") || !slices.Contains(got[load:], "session/set_model") {
		t.Fatalf("the reloaded agent was asked %q, %v", got, err)
	}
	if row := indexRowByID(t, id); row.Unattended == nil || *row.Unattended != rec {
		t.Fatalf("the reloaded session's row: %+v", row)
	}

	person := "0199c0de-5555-7000-8000-00000000e005"
	seedIndexRow(t, sessions.Row{SessionID: "person-1", Provider: "cursor", CWD: absDir(ws), CrazeID: person, Title: "mine"})
	stdout, stderr, code = executeErr([]string{"send", "0000e005", "--wait", "--json", "hello"})
	if m = decodeResult(t, stdout); code != 0 || m["status"] != "completed" || m["session"] != person {
		t.Fatalf("craze send to a person's session: exit %d, %v, %q", code, m, stderr)
	}
	checkArgv(t, childArgv(t, kids.cmd(t, 2)), []string{"--load=" + person, "--no-host-status"},
		[]string{"--unattended", "--plan", "--ask", "--no-force", "--model", "--effort", "--provider", "--workspace"})
	if row := indexRowByID(t, person); row.Unattended != nil {
		t.Fatalf("a person's row was marked %+v", row.Unattended)
	}
}

// TestTwoSendsReloadingOneSessionShareItsHost is the hunt's reload race: two
// craze sends reload one ended session at once. The first's host is held
// where it has claimed the session and is not yet in the registry, so the
// second, finding no host, spawns its own — which the claim refuses held, and
// whose spawner waits for the holder (spawnHost's rendezvous) and attaches
// to it. Both prompts run in the one host, each send its own turn.
func TestTwoSendsReloadingOneSessionShareItsHost(t *testing.T) {
	_, ws := serveHome(t)
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t)))
	env := rundir.ProcessEnv()
	gate := filepath.Join(t.TempDir(), "claimed")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	kids := spawnAsChild(t, func(n int) []string {
		if n == 1 {
			return []string{cliChildGate + "=" + gate}
		}
		return nil
	})
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	polled := make(chan struct{})
	var once sync.Once
	prev := hostspawn.Polled
	hostspawn.Polled = func() { once.Do(func() { close(polled) }) }
	t.Cleanup(func() { hostspawn.Polled = prev })
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	id := "0199c0de-6666-7000-8000-00000000f006"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id,
		Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass}})

	// The first host is at its claim once it opens the gate to read.
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err == nil {
			opened <- f
		}
	}()
	first := goCraze("send", "0000f006", "--wait", "--json", "first")
	var held *os.File
	select {
	case held = <-opened:
	case <-time.After(serveStep):
		t.Fatal("the first host never reached its claim")
	}
	defer func() { _ = held.Close() }()
	second := goCraze("send", "0000f006", "--wait", "--json", "second")
	select {
	case <-polled:
	case <-time.After(serveStep):
		t.Fatal("the second send never waited for the holder")
	}
	if _, err := held.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	turns := map[string]string{}
	for _, run := range []*crazeRun{first, second} {
		stdout, stderr, code := run.wait(t)
		m := decodeResult(t, stdout)
		if code != 0 || m["status"] != "completed" || m["session"] != id {
			t.Fatalf("a send: exit %d, %v, %q", code, m, stderr)
		}
		turns[m["turn"].(string)] = m["message"].(string)
	}
	if len(turns) != 2 || !slices.Contains([]string{"echo: first", "echo: second"}, turns["turn-1"]) ||
		!slices.Contains([]string{"echo: first", "echo: second"}, turns["turn-2"]) {
		t.Fatalf("the two sends' turns: %v", turns)
	}
	if n := kids.count(); n != 2 {
		t.Fatalf("%d hosts spawned, want 2 (the second refused held)", n)
	}
	entries, err := rundir.Hosts(env)
	if err != nil || len(entries) != 1 || entries[0].CrazeSessionID != id {
		t.Fatalf("the registry after both sends: %+v, %v", entries, err)
	}
}

// TestNewWaitSignalBeforeItsPromptStopsTheSession is §3.1 "Signals" for craze
// new --wait before its prompt was sent: the session's start is held (the
// fake agent's effort set, gated), a signal comes once the host is up and
// the command is attaching, and the session the command spawned is stopped —
// its host gone — and the result is `cancelled`, exit 3; the agent was never
// prompted. --model is only what was asked for, never resolved, so the result
// has no model.
func TestNewWaitSignalBeforeItsPromptStopsTheSession(t *testing.T) {
	env, ws, _ := unattendedHome(t)
	dir := t.TempDir()
	setGate, prompts := filepath.Join(dir, "set"), filepath.Join(dir, "prompts")
	if err := syscall.Mkfifo(setGate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SCRIPT", "effort")
	t.Setenv("CRAZE_FAKE_SET_GATE", setGate)
	t.Setenv("CRAZE_FAKE_DUMP_PROMPTS", prompts)
	sigs := withWaitSignals(t)
	spawned := make(chan hostRef, 1)
	prev := promptSpawned
	promptSpawned = func(ref hostRef) { spawned <- ref }
	t.Cleanup(func() { promptSpawned = prev })

	run := goCraze("new", "--wait", "--json", "--provider", "cursor", "--model", "nope", "--effort", "low", "-C", ws, "never sent")
	var ref hostRef
	select {
	case ref = <-spawned:
	case <-time.After(serveStep):
		t.Fatal("craze new --wait never spawned its host")
	}
	sigs <- syscall.SIGINT
	stdout, stderr, code := run.wait(t)
	m := decodeResult(t, stdout)
	if code != 3 || m["status"] != "cancelled" || m["session"] != ref.entry.CrazeSessionID || m["message"] != "" ||
		!errorIs(m, "interrupted before the prompt was sent; the session it started was stopped", "interrupted", false) || stderr != "" {
		t.Fatalf("a signal before the prompt: exit %d, %v, %q", code, m, stderr)
	}
	if model, ok := m["model"]; ok {
		t.Fatalf("a model never resolved is on the result: %v", model)
	}
	waitReaped(t, ref.entry.PID)
	if entries, err := rundir.Hosts(env); err != nil || len(entries) != 0 {
		t.Fatalf("the registry after the stop: %+v, %v", entries, err)
	}
	if b, err := os.ReadFile(prompts); err == nil && len(b) != 0 {
		t.Fatalf("the agent was prompted: %s", b)
	}
}

// TestOnlyTheDirectSpawnTakesTheHubsEnvironment (the hunt's environment
// item): a spawn with hubEnv — craze new --wait's, craze send's — hands its
// host the hub's environment (hub.ChildEnv), and the TUI launcher's own
// spawn, without it, still hands its host the launch's: the terminal it runs
// in included.
func TestOnlyTheDirectSpawnTakesTheHubsEnvironment(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv("TMUX", "/tmp/tmux-0/default,1,0")
	kids := spawnAsChild(t, nil)
	for i, hubEnv := range []bool{false, true} {
		opts := spawnFor(t, env, ws)
		opts.hubEnv = hubEnv
		ref, err := spawnNow(t, opts)
		if err != nil {
			t.Fatalf("spawn (hubEnv %v): %v", hubEnv, err)
		}
		if got := slices.Contains(kids.cmd(t, i).Env, "TMUX=/tmp/tmux-0/default,1,0"); got == hubEnv {
			t.Fatalf("hubEnv %v: the host was handed TMUX: %v", hubEnv, got)
		}
		if err := stopHost(ref.entry); err == nil {
			waitReaped(t, ref.entry.PID)
		}
	}
}
