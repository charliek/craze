package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/rundir"
)

func initReviewRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "init", "--quiet")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s, %v", raw, err)
	}
}
func TestReviewCommandRemovesInputsBeforeCheck(t *testing.T) {
	env, ws, _ := unattendedHome(t)
	initReviewRepo(t, ws)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv("CRAZE_SESSION_PROVIDER", "")
	files := []string{filepath.Join(ws, "brief"), filepath.Join(ws, "focus")}
	for i, p := range files {
		if e := os.WriteFile(p, []byte([]string{"first", "last"}[i]), 0600); e != nil {
			t.Fatal(e)
		}
	}
	path := filepath.Join(ws, "reports", "review.md")
	args := []string{"review", "--provider", "cursor", "--allow-same-provider", "-C", ws, "--prompt-file", files[0], "--prompt-file", "-", "--prompt-file", files[1], "--prompt-file", files[0], "--remove-prompt-file", "--max-message", "4", "--out", path}
	out, stderr, code := executeIn(args, strings.NewReader("middle"))
	text, err := os.ReadFile(path)
	if err != nil || code != 0 || out != "echo\n" || !strings.Contains(string(text), "first\n\nmiddle\n\nlast\n\nfirst") || strings.Contains(string(text), "\n---\n") {
		t.Fatalf("exit %d out %q file %q %v stderr %q", code, out, text, err, stderr)
	}
	for _, p := range files {
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			t.Fatalf("input survived: %v", e)
		}
	}
	hosts, e := rundir.Hosts(env)
	if e != nil || len(hosts) > 0 {
		t.Fatalf("%+v %v", hosts, e)
	}
}
func TestReviewTerminalStatuses(t *testing.T) {
	for _, tc := range []struct {
		script, mode, status, kind string
		code                       int
	}{
		{"empty", "fail", "failed", "empty", 3}, {"dirty_tree", "fail", "failed", "dirty_tree", 3}, {"dirty_tree", "warn", "completed", "", 0}, {"dirty_tree", "off", "completed", "", 0},
		{"stall", "fail", "failed", "stalled", 3},
	} {
		t.Run(tc.script+tc.mode, func(t *testing.T) {
			_, ws, _ := unattendedHome(t)
			initReviewRepo(t, ws)
			t.Setenv("CRAZE_FAKE_SCRIPT", tc.script)
			path := filepath.Join(ws, "out.md")
			args := []string{"review", "--json", "--provider", "cursor", "--allow-same-provider", "-C", ws, "--dirty-check", tc.mode, "--out", path, "--criteria", "correctness"}
			if tc.script == "stall" {
				args = append(args, "--stall-timeout", "100ms")
			}
			out, stderr, code := executeIn(args, strings.NewReader(""))
			var r reviewResultJSON
			if code != tc.code || json.Unmarshal([]byte(out), &r) != nil || r.Status != tc.status || len(r.Attempts) != 1 || tc.kind != "" && (r.Error == nil || string(r.Error.Kind) != tc.kind) {
				t.Fatalf("exit %d %q %q", code, out, stderr)
			}
			if tc.script == "dirty_tree" {
				file, e := os.ReadFile(path)
				if e != nil || !strings.Contains(string(file), "review retained") || (tc.mode != "off") != (len(r.DirtyFiles) > 0) {
					t.Fatal(string(file), e, r)
				}
			}
		})
	}
}

func TestIncompleteReviewPreservesPriorFileAndDirtyEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.md")
	if err := os.WriteFile(path, []byte("previous review"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cut := range []bool{false, true} {
		r := reviewResultJSON{Status: statusFailed, Error: kindError("empty review", errkind.Empty)}
		if cut {
			r.MessageCut, r.Message, r.DirtyFiles, r.Error = true, "tail", []string{"changed"}, kindError("workspace changed", errkind.DirtyTree)
		}
		var out, diag bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&diag)
		err := (reviewFlags{out: path, json: true}).render(cmd, r)
		var result reviewResultJSON
		if err == nil || json.Unmarshal(out.Bytes(), &result) != nil || result.OutPath != "" || result.MessageCut != cut || result.Message != r.Message || result.Error.Kind != r.Error.Kind || len(result.DirtyFiles) != len(r.DirtyFiles) {
			t.Fatal(err, out.String())
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "previous review" {
			t.Fatal(string(data), err)
		}
	}
}

type closedReviewOutput struct{}

func (closedReviewOutput) Write([]byte) (int, error) { return 0, syscall.EPIPE }
func TestReviewWritesCompleteFileBeforeBrokenPipe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "full.md")
	cmd := &cobra.Command{}
	cmd.SetOut(closedReviewOutput{})
	f := reviewFlags{out: path, maxMessage: 2}
	err := f.render(cmd, reviewResultJSON{Status: "completed", Message: "complete review"})
	out, e := os.ReadFile(path)
	if failureKind(err) != errkind.Internal || e != nil || string(out) != "complete review" {
		t.Fatal(string(out), err, e)
	}
}
func TestReviewBundleAndInvalidInputs(t *testing.T) {
	ws := t.TempDir()
	initReviewRepo(t, ws)
	for _, args := range [][]string{{"--role", "panel"}, {"--bundle-only"}, {"--bundle-only", "--changes-since", "--bad"}, {"--prompt-file", "-", "--prompt-file", "-"}, {"--model", "x"}, {"--target", "sol", "--role", "review"}} {
		out, _, code := executeIn(append([]string{"review", "--json", "-C", ws}, args...), bytes.NewReader(nil))
		if code != 2 || !json.Valid([]byte(out)) {
			t.Fatalf("%q exit %d, %s", args, code, out)
		}
	}
	out, stderr, code := executeIn([]string{"review", "--bundle-only", "--changes-since", "HEAD", "-C", ws}, bytes.NewReader(nil))
	if code != 0 || !strings.Contains(out, "No changes since HEAD.") || stderr != "" {
		t.Fatal(code, out, stderr)
	}
}
func TestReviewRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		k            errkind.Kind
		start, retry bool
	}{
		{errkind.Config, true, true}, {errkind.Config, false, false}, {errkind.Quota, false, true}, {errkind.ModelNotFound, false, true}, {errkind.Stalled, false, false}, {errkind.ContextTooLarge, false, false}, {errkind.StartFailed, true, true}, {errkind.Empty, false, false},
	} {
		a := attemptFailure(tc.k, "failure", tc.start)
		if mayRetry(a) != tc.retry {
			t.Fatal(tc)
		}
		a.cleanupErr = errors.New("live child")
		if mayRetry(a) {
			t.Fatal("fallback on unverified shutdown")
		}
	}
}

func TestReviewOverloadFallbackRequiresSubmittedTurn(t *testing.T) {
	a := attemptFailure(errkind.Provider, "overloaded", true)
	a.turn.Error.Retryable = true
	if mayRetry(a) {
		t.Fatal("retried an unclassified provider startup failure")
	}
	a.startFailure = false
	if !mayRetry(a) {
		t.Fatal("missed retryable mid-turn overload")
	}
}

func TestReviewOutputKeepsMoreThanTheOrdinaryWaiterTail(t *testing.T) {
	chunk := strings.Repeat("x", 64<<10)
	events := []agent.Event{textEv("head-of-review\n")}
	for range 33 {
		events = append(events, textEv(chunk))
	}
	events = append(events, textEv("\nend-of-review"))
	rig := newWaitRig(t, 0, answerTurn(events...))
	conn := rig.dial(nil)
	res, failure := goWait(conn, waitStart{submit: submitPrompt(conn, "review")}, waitOptions{fullMessage: true}).result(t)
	wantResult(t, res, failure, statusCompleted)
	want := "head-of-review\n" + strings.Repeat(chunk, 33) + "\nend-of-review"
	if res.MessageCut || res.message() != want {
		t.Fatalf("full result was cut: bytes %d", len(res.message()))
	}
	path := filepath.Join(t.TempDir(), "full.md")
	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	f := reviewFlags{out: path, maxMessage: 4}
	if err := f.render(cmd, reviewResultJSON{Status: statusCompleted, Message: res.message()}); err != nil {
		t.Fatal(err)
	}
	data, e := os.ReadFile(path)
	if e != nil || string(data) != want || stdout.String() != "head\n" {
		t.Fatalf("file bytes %d, stdout %q, %v", len(data), stdout.String(), e)
	}
}

func TestReviewCommandDeadlineDuringFallbackStartup(t *testing.T) {
	_, ws, _ := unattendedHome(t)
	shorten(t, &reviewFallbackFloor, 10*time.Millisecond)
	dir := t.TempDir()
	marker := filepath.Join(dir, "second-started")
	delayed := filepath.Join(dir, "slow-cursor")
	if err := os.WriteFile(delayed, []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\nexec sleep 30\n", marker)), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SCRIPT", "codex-quota")
	t.Setenv("CODEX_API_KEY", "")
	writeCrazeConfig(t, fmt.Sprintf("[agents]\ncodex=%q\ncursor=%q\n[[targets.sol.routes]]\nprovider='codex'\nmodel='gpt-6.2-sol'\nbilling='subscription'\n[[targets.sol.routes]]\nprovider='cursor'\nmodel='m-1'\nbilling='subscription'\n", fakeAgentPath(t), delayed))
	out, diag, code := executeIn([]string{"review", "--target", "sol", "--allow-same-provider", "--effort", "low", "--timeout", "3s", "--json", "-C", ws, "inspect"}, strings.NewReader(""))
	var doc reviewResultJSON
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(out, err)
	}
	if _, err := os.Stat(marker); err != nil || code != 5 || !doc.Stopping || doc.Status != reviewTimedOut || len(doc.Attempts) != 2 {
		t.Fatalf("exit %d: %+v, %s, %v", code, doc, diag, err)
	}
}
