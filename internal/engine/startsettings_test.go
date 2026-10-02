package engine_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// The start settings against the engine's admission (plan 032 §3.11, P6, A13):
// a session's --effort and --fast are set inside its Start, so the engine —
// which admits nothing until Start has returned — cannot let a prompt reach
// the agent ahead of them. The schedule is forced on the fake agent: every
// session/set_config_option is held on its set gate (CRAZE_FAKE_SET_GATE) until
// the test releases it, while the fake reads on and records every call in the
// order craze wrote it (CRAZE_FAKE_DUMP_CALLS). A prompt is submitted while
// each set is known to be on the wire and unanswered.

// callRecord points the fake's call record at a file of the test's and
// answers its reader.
func callRecord(t *testing.T) func() []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calls")
	t.Setenv("CRAZE_FAKE_DUMP_CALLS", path)
	return func() []string {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

// waitCall waits, bounded, for the record to hold call.
func waitCall(t *testing.T, calls func() []string, call string) {
	t.Helper()
	deadline := time.Now().Add(watchdog)
	for !slices.Contains(calls(), call) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent never read %q: %q", call, calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// setGate is the fake's set gate: each release lets one held set be answered.
func setGate(t *testing.T, ctx context.Context) func() {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "set-gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SET_GATE", fifo)
	return func() {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err != nil {
				done <- err
				return
			}
			_, err = w.Write([]byte{1})
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("releasing the set gate: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("releasing the set gate: %v", ctx.Err())
		}
	}
}

// index is call's place in calls, failing the test when it is not there once.
func index(t *testing.T, calls []string, call string) int {
	t.Helper()
	var at []int
	for i, c := range calls {
		if c == call {
			at = append(at, i)
		}
	}
	if len(at) != 1 {
		t.Fatalf("%q is in the record %d times: %q", call, len(at), calls)
	}
	return at[0]
}

// TestAPromptCannotOvertakeTheStartSettings: with each set held on the wire,
// Start has not returned and a prompt is refused, not queued behind it; once
// both are answered and Start has returned the prompt is admitted, and the
// agent read it after both sets. The negative control is the same record of a
// setting made after Start — by a client's Set, once a prompt is on the wire:
// the record shows that prompt first, so it does show a prompt overtaking a
// setting when one does.
func TestAPromptCannotOvertakeTheStartSettings(t *testing.T) {
	t.Run("start settings", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
		defer cancel()
		calls := callRecord(t)
		release := setGate(t, ctx)
		on := true
		sess := agent.New(agent.Options{
			Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=effort"},
			Workspace: t.TempDir(), Stderr: io.Discard, Effort: "low", Fast: &on,
		})
		e, err := engine.New(sess, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		p := readPrimary(t, e, false)
		started := make(chan error, 1)
		go func() { started <- e.Start(ctx) }()
		for _, set := range []string{"effort=low", "fast=true"} {
			waitCall(t, calls, "session/set_config_option "+set)
			if res, err := e.Submit(engine.Command{}, "first", engine.SubmitQueue, ""); !errors.Is(err, engine.ErrNotAccepting) {
				t.Fatalf("a prompt while %s is on the wire: %+v, %v; want refused", set, res, err)
			}
			select {
			case err := <-started:
				t.Fatalf("Start returned (%v) with %s unanswered", err, set)
			default:
			}
			release()
		}
		select {
		case err := <-started:
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("Start did not return once both sets were answered")
		}
		res, err := e.Submit(engine.Command{}, "first", engine.SubmitQueue, "")
		if err != nil || res.Turn == "" {
			t.Fatalf("the first prompt after Start: %+v, %v", res, err)
		}
		p.waitFor(t, 0, endedTurn(res.Turn))
		got := calls()
		prompt := index(t, got, "session/prompt")
		if index(t, got, "session/set_config_option effort=low") > prompt || index(t, got, "session/set_config_option fast=true") > prompt {
			t.Fatalf("the agent read the prompt before a start setting: %q", got)
		}
		cfg := map[string]string{}
		for _, o := range e.State().Config {
			cfg[o.ID] = o.Current
		}
		if cfg["effort"] != "low" || cfg["fast"] != "true" {
			t.Fatalf("the session is at %v", cfg)
		}
	})
	t.Run("a setting after Start", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
		defer cancel()
		calls := callRecord(t)
		release := setGate(t, ctx)
		sess := agent.New(agent.Options{
			Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=effort"},
			Workspace: t.TempDir(), Stderr: io.Discard,
		})
		e, err := engine.New(sess, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		p := readPrimary(t, e, false)
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		res, err := e.Submit(engine.Command{}, "first", engine.SubmitQueue, "")
		if err != nil || res.Turn == "" {
			t.Fatalf("submit: %+v, %v", res, err)
		}
		// Admitted is not written: the turn's own goroutine writes the
		// prompt, so the setting is made once the agent has read it.
		waitCall(t, calls, "session/prompt")
		set := make(chan error, 1)
		go func() {
			_, err := e.Set(ctx, engine.Command{}, engine.Setting{Kind: engine.SettingConfig, ID: "effort", Value: "low"})
			set <- err
		}()
		waitCall(t, calls, "session/set_config_option effort=low")
		release()
		if err := <-set; err != nil {
			t.Fatalf("Set: %v", err)
		}
		p.waitFor(t, 0, endedTurn(res.Turn))
		got := calls()
		if index(t, got, "session/prompt") > index(t, got, "session/set_config_option effort=low") {
			t.Fatalf("the record does not show the prompt ahead of the late setting: %q", got)
		}
	})
}
