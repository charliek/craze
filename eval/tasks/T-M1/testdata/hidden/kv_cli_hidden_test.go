package main

// Trusted tests for the renamed kv command (T-M1): the command is built with `go build`
// and run as a user would, so they hold whatever shape the agent gave main.go.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	hiddenKVOnce sync.Once
	hiddenKVBin  string
	hiddenKVErr  error
)

func hiddenKVBinary(t *testing.T) string {
	t.Helper()
	hiddenKVOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kv-hidden-")
		if err != nil {
			hiddenKVErr = err
			return
		}
		hiddenKVBin = filepath.Join(dir, "kv")
		out, err := exec.Command("go", "build", "-o", hiddenKVBin, ".").CombinedOutput()
		if err != nil {
			hiddenKVErr = errors.New(err.Error() + ": " + string(out))
		}
	})
	if hiddenKVErr != nil {
		t.Fatalf("building kv: %v", hiddenKVErr)
	}
	return hiddenKVBin
}

// hiddenKVRun runs kv with args; its stdout, stderr and exit code.
func hiddenKVRun(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(hiddenKVBinary(t), args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("kv %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), code
}

func TestHiddenKVNamespaceFlag(t *testing.T) {
	file := filepath.Join(t.TempDir(), "data.json")
	if _, errOut, code := hiddenKVRun(t, "-file", file, "-namespace", "users", "put", "ada", "admin"); code != 0 {
		t.Fatalf("put with -namespace exited %d: %s", code, errOut)
	}
	out, errOut, code := hiddenKVRun(t, "-file", file, "-namespace", "users", "get", "ada")
	if code != 0 || strings.TrimSpace(out) != "admin" {
		t.Fatalf("get with -namespace = %q (exit %d, stderr %q), want admin", out, code, errOut)
	}
	if _, _, code := hiddenKVRun(t, "-file", file, "-bucket", "users", "get", "ada"); code == 0 {
		t.Fatal("the old -bucket flag is still accepted")
	}
}

func TestHiddenKVListNamespaces(t *testing.T) {
	file := filepath.Join(t.TempDir(), "data.json")
	if _, errOut, code := hiddenKVRun(t, "-file", file, "-namespace", "users", "put", "ada", "admin"); code != 0 {
		t.Fatalf("put exited %d: %s", code, errOut)
	}
	if _, errOut, code := hiddenKVRun(t, "-file", file, "put", "k", "v"); code != 0 {
		t.Fatalf("put without -namespace exited %d: %s", code, errOut)
	}
	out, errOut, code := hiddenKVRun(t, "-file", file, "list-namespaces")
	if code != 0 {
		t.Fatalf("list-namespaces exited %d: %s", code, errOut)
	}
	if got := strings.Fields(out); strings.Join(got, ",") != "default,users" {
		t.Fatalf("list-namespaces = %q, want default and users", out)
	}
	if _, _, code := hiddenKVRun(t, "-file", file, "list-buckets"); code == 0 {
		t.Fatal("the old list-buckets command still succeeds")
	}
}

func TestHiddenKVMissingKeyMessage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "data.json")
	if _, errOut, code := hiddenKVRun(t, "-file", file, "-namespace", "users", "put", "ada", "admin"); code != 0 {
		t.Fatalf("put exited %d: %s", code, errOut)
	}
	_, errOut, code := hiddenKVRun(t, "-file", file, "-namespace", "users", "get", "bob")
	if code == 0 {
		t.Fatal("get of a missing key succeeded")
	}
	if low := strings.ToLower(errOut); !strings.Contains(low, "namespace") || strings.Contains(low, "bucket") {
		t.Fatalf("missing-key message %q should speak of the namespace, not a bucket", errOut)
	}
}
