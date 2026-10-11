package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The prompt's edges the C3 review (round 1) found, each placed by the test:
// a reload's host is never this command's to stop; a listed host gone before
// the dial is reloaded past; a receipt is built from the answer the gate
// recorded; a second signal the waiter took hurries the undo; a queued
// receipt's place comes from the queue when its event was not seen; an
// accepted prompt whose answer was lost is neither undone nor resent.

// jobCmd is a command whose output a test reads: what newPromptJob prints to.
func jobCmd() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return cmd, &out
}

// TestSendNeverStopsTheHostItsReloadSpawned (C3 review r1, 1): craze send
// --wait reloads an ended session, and a signal comes before its prompt was
// sent. The reloaded host is not the command's to stop — another send may
// have found it and prompted it meanwhile — so it is left serving the
// session, unprompted, and the result is `cancelled` with no word of a stop.
func TestSendNeverStopsTheHostItsReloadSpawned(t *testing.T) {
	env, ws, _ := unattendedHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	id := "0199c0de-9999-7000-8000-000000009009"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id,
		Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass}})
	sigs := withWaitSignals(t)
	spawned := make(chan hostRef, 1)
	prev := promptSpawned
	promptSpawned = func(ref hostRef) { spawned <- ref }
	t.Cleanup(func() { promptSpawned = prev })

	run := goCraze("send", "00009009", "--wait", "--json", "never sent")
	var ref hostRef
	select {
	case ref = <-spawned:
	case <-time.After(serveStep):
		t.Fatal("craze send never spawned a host to reload the session")
	}
	sigs <- syscall.SIGINT
	stdout, stderr, code := run.wait(t)
	m := decodeResult(t, stdout)
	if code != 3 || m["status"] != "cancelled" || m["error"] != "interrupted before the prompt was sent" || stderr != "" {
		t.Fatalf("a signal before the reload's prompt: exit %d, %v, %q", code, m, stderr)
	}
	e, ok := hostServing(env, id)
	if !ok || e.HostID != ref.entry.HostID || !processAlive(ref.entry.PID) {
		t.Fatalf("the reloaded host is gone: %+v %v", e, ok)
	}
	if row := listRow(t, e); row.LastTurn != nil || row.Prompted {
		t.Fatalf("the reloaded session was prompted: %+v", row)
	}
}

// TestSendReloadsWhenTheListedHostGoesBeforeTheDial (C3 review r1, 2): the
// registry lists a host serving the session, and it goes between that answer
// and craze send's dial — its socket gone. The dial's failure is no refusal of
// the host's: the registry is read again, and the session is loaded in a host
// of craze send's own, its prompt run there.
func TestSendReloadsWhenTheListedHostGoesBeforeTheDial(t *testing.T) {
	env, ws, kids := unattendedHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	id := "0199c0de-aaaa-7000-8000-00000000a00a"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id,
		Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass}})
	h, err := fakehost.New(fakehost.Options{HostID: "0000000000a1", CrazeSessionID: id, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := h.Register(env)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(reg.Listener()) }()
	var once sync.Once
	end := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), serveStep)
			defer cancel()
			_ = h.Close(ctx)
			<-served
			_ = reg.Close()
		})
	}
	t.Cleanup(end)
	dialled := 0
	prev := sendDialling
	sendDialling = func(e rundir.Entry) {
		dialled++
		if e.HostID == "0000000000a1" {
			end()
		}
	}
	t.Cleanup(func() { sendDialling = prev })

	stdout, stderr, code := executeErr([]string{"send", "0000a00a", "--wait", "--json", "after it went"})
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["message"] != "echo: after it went" || m["session"] != id {
		t.Fatalf("craze send past a host gone before the dial: exit %d, %v, %q", code, m, stderr)
	}
	if dialled == 0 || kids.count() != 1 {
		t.Fatalf("dialled %d listed hosts, spawned %d", dialled, kids.count())
	}
	checkArgv(t, childArgv(t, kids.cmd(t, 0)), []string{"--load=" + id, "--unattended"}, nil)
}

// TestSendsReceiptIsTheAnswerItsGateRecorded (C3 review r1, 3): the prompt's
// answer comes only after craze send gave up waiting for it (a second
// signal), yet before it reads its gate. The prompt was taken, and the
// receipt says so from the answer the gate recorded — `started turn-1`, exit
// 0 — never a `queued` with no row (whether the turn's start was still to be
// read on the stream, and the handle with it, is the stream's).
func TestSendsReceiptIsTheAnswerItsGateRecorded(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("ok")))
	conn, tp := r.dialTapped()
	release := r.gate()
	tp.on(`"method":"`+protocol.MethodSessionPrompt+`"`, func() bool {
		<-release.ch
		return true
	})
	prev := sendFate
	sendFate = func(g *submitGate) {
		release.open()
		waitFor(t, "the prompt's answer", func() bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.answered
		})
	}
	t.Cleanup(func() { sendFate = prev })
	sigs := make(chan os.Signal, 4)
	cmd, out := jobCmd()
	job := newPromptJob(cmd, sendWhat, "mine", false, time.Now(), 0, false, 0, sigs)
	defer job.close()
	job.session = resultSession{id: conn.sessionID, provider: conn.entry.Provider}
	done := make(chan error, 1)
	go func() { done <- job.sendOnly(conn) }()
	waitFor(t, "the prompt held at the client", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
	sigs <- syscall.SIGINT
	sigs <- syscall.SIGINT
	select {
	case err := <-done:
		if err != nil || !strings.HasPrefix(out.String(), "started turn-1\nnext: craze wait "+waitRigShort) {
			t.Fatalf("craze send's receipt: %v, %q", err, out)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("craze send did not return")
	}
}

// TestASecondSignalTakenByTheWaiterHurriesTheUndo (C3 review r1, 4): craze
// new --wait's prompt is not yet sent — the identity proof it waits on is
// never answered — when a signal comes, and then a second, which the waiter
// takes as its own: it returns at once. The session the command started is
// then told to stop, and not waited for — its agent ignores SIGTERM, so its
// host takes the agent's kill grace to go — and the command is done before
// the host is.
func TestASecondSignalTakenByTheWaiterHurriesTheUndo(t *testing.T) {
	r := newWaitRig(t, 0, endTurn())
	// Dialled while the rig's host is the registry's only one (r.entry).
	conn, tp := r.dialTapped()
	ws := t.TempDir()
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv("CRAZE_FAKE_STUBBORN", "1")
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	ref, err := spawnNow(t, spawnFor(t, r.env, ws))
	if err != nil {
		t.Fatal(err)
	}
	tp.swallow(`"method":"` + protocol.MethodSessionsList + `"`)
	prevWait := cleanupWait
	cleanupWait = serveStep
	t.Cleanup(func() { cleanupWait = prevWait })
	awaiting := make(chan struct{}, 1)
	prevAwait := waitAwaitingReply
	waitAwaitingReply = func() {
		select {
		case awaiting <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { waitAwaitingReply = prevAwait })
	sigs := make(chan os.Signal, 4)
	cmd, out := jobCmd()
	job := newPromptJob(cmd, newWhat, "never sent", true, time.Now(), 0, true, 0, sigs)
	defer job.close()
	job.spawned = &ref
	job.session = resultSession{id: ref.entry.CrazeSessionID, provider: "cursor"}
	done := make(chan error, 1)
	go func() { done <- job.waitFor(conn) }()
	waitFor(t, "the identity's proof sent", func() bool { return len(tp.lines(protocol.MethodSessionsList)) >= 1 })
	sigs <- syscall.SIGINT
	select {
	case <-awaiting:
	case <-time.After(stubWatchdog):
		t.Fatal("the waiter never waited for the prompt's answer")
	}
	sigs <- syscall.SIGINT
	var exited bool
	select {
	case err = <-done:
		exited = ref.child.Exited()
	case <-time.After(stubWatchdog):
		t.Fatal("craze new --wait did not return")
	}
	m := decodeResult(t, out.String())
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 || m["status"] != "cancelled" ||
		m["error"] != "interrupted before the prompt was sent; the session it started was told to stop" {
		t.Fatalf("the second signal: %v, %v", err, m)
	}
	if exited {
		t.Fatal("the undo waited for the host to go after the second signal")
	}
	waitReaped(t, ref.entry.PID)
}

// TestSendsQueuedReceiptReadsItsPlaceFromTheQueue (C3 review r1, 8): a
// queued prompt's receipt takes its place from its `queued` event, and when
// the stream did not show it, from the session's queue as session.state reads
// it; and when the row has left the queue by then too, the receipt says
// `queued <row>` with no place, and --json no position.
func TestSendsQueuedReceiptReadsItsPlaceFromTheQueue(t *testing.T) {
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(heldTurn(g, textEv("the first")), answerTurn(textEv("the second")))
	r.submit("someone else's")
	r.waitStarted("turn-1")
	prev := sendPlace
	t.Cleanup(func() { sendPlace = prev })
	sendPlace = func(*promptJob, *waitConn, waitTarget) (streamPlace, bool) { return streamPlace{}, false }
	stdout, stderr, code := executeErr([]string{"send", waitRigShort, "mine"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze send: exit %d, %q %q", code, stdout, stderr)
	}
	sendNext(t, stdout, "queued q-1 (position 1)")

	sendPlace = func(*promptJob, *waitConn, waitTarget) (streamPlace, bool) {
		g.open()
		waitFor(t, "the queue drained", func() bool { return len(r.eng.State().Queue) == 0 })
		return streamPlace{}, false
	}
	stdout, stderr, code = executeErr([]string{"send", waitRigShort, "--json", "more"})
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "queued" || m["row"] != "q-2" || m["position"] != nil || m["handle"] == nil {
		t.Fatalf("a queued receipt with no place: exit %d, %v, %q", code, m, stderr)
	}
}

// TestAnAcceptedPromptWhoseAnswerIsLostIsNeitherUndoneNorResent (§7 W2 16,
// the review's gap): craze new --wait's host — a real one the command
// started — and its prompt accepted by the session (A runs it), whose answer
// is lost with its connection, the client then reaching another incarnation
// (B). The outcome is unknown: exit 1, `the prompt may not have reached it`,
// naming craze wait; the session the command started is not stopped; and
// the prompt is sent to neither again.
func TestAnAcceptedPromptWhoseAnswerIsLostIsNeitherUndoneNorResent(t *testing.T) {
	for _, wait := range []bool{true, false} {
		t.Run(map[bool]string{true: "--wait", false: "no wait"}[wait], func(t *testing.T) {
			r := newWaitRig(t, 0, answerTurn(textEv("A ran it")))
			sB, engB := r.newEngine(0, answerTurn(textEv("B's")))
			// Dialled while the rig's host is the registry's only one.
			conn, tp := r.dialTapped()
			ws := t.TempDir()
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			spawnAsChild(t, nil)
			spawnBounds(t, serveStep, serveStep, serveStep)
			ref, err := spawnNow(t, spawnFor(t, r.env, ws))
			if err != nil {
				t.Fatal(err)
			}
			redial := r.gate()
			tp.holdRedials(redial)
			lost := tp.loseReply(`"result":{"turn":"turn-1"`)
			cmd, out := jobCmd()
			job := newPromptJob(cmd, newWhat, "mine", wait, time.Now(), 0, true, 0, make(chan os.Signal))
			defer job.close()
			job.spawned = &ref
			job.session = resultSession{id: conn.sessionID, provider: conn.entry.Provider}
			done := make(chan error, 1)
			go func() {
				if wait {
					done <- job.waitFor(conn)
				} else {
					done <- job.sendOnly(conn)
				}
			}()
			select {
			case <-lost:
			case <-time.After(stubWatchdog):
				t.Fatal("the prompt's answer never came")
			}
			r.srv.SetEngine(engB)
			redial.open()
			select {
			case err = <-done:
			case <-time.After(stubWatchdog):
				t.Fatal("the command did not return")
			}
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 1 || !strings.Contains(ee.msg, "the prompt may not have reached it") ||
				!strings.Contains(ee.msg, "craze wait "+waitRigShort) {
				t.Fatalf("the lost answer: %v; %q", err, out)
			}
			if got := r.s.sent(); !slices.Equal(got, []string{"mine"}) {
				t.Fatalf("A ran %q, want the prompt once", got)
			}
			if got := sB.sent(); len(got) != 0 {
				t.Fatalf("the prompt was sent again, to B: %q", got)
			}
			if ref.child.Exited() || !processAlive(ref.entry.PID) {
				t.Fatal("the session the command started was stopped though its prompt may have reached it")
			}
		})
	}
}
