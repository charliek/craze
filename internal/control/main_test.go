package control_test

import (
	"os"
	"testing"
)

// The web search's variables (plan 051 §B), spelled out because this
// package's tests may not import the harness (depguard's control-test rule):
// modeltable.ExaKeyEnv and websearch.TestEndpointEnv.
const (
	exaKeyEnv      = "EXA_API_KEY"
	exaEndpointEnv = "CRAZE_TEST_EXA_MCP"
	// exaFence is the web search's endpoint for the package: a loopback port
	// nothing serves.
	exaFence = "http://127.0.0.1:9/mcp"
)

// TestMain fences the web search off for the package, as internal/agent's
// TestMain does: a test here opens a real native session from the process
// environment (models_test.go), and the web search's key is a credential of
// every native table, so it is unset by name, and its endpoint is aimed at a
// loopback port nothing serves, so a session offered the web tools gets a
// refused connection instead of reaching Exa. A test that wants either sets it
// itself (t.Setenv).
func TestMain(m *testing.M) {
	_ = os.Unsetenv(exaKeyEnv)
	_ = os.Setenv(exaEndpointEnv, exaFence)
	os.Exit(m.Run())
}

// TestTheWebSearchIsFenced: TestMain leaves the package no web search key and
// aims the web search's endpoint at the fence, whatever the shell that ran the
// tests exported — so the real native session a test here opens neither holds
// an exported key nor reaches Exa.
func TestTheWebSearchIsFenced(t *testing.T) {
	if v, ok := os.LookupEnv(exaKeyEnv); ok {
		t.Fatalf("%s is set for the package (%d bytes); TestMain unsets it", exaKeyEnv, len(v))
	}
	if got := os.Getenv(exaEndpointEnv); got != exaFence {
		t.Fatalf("%s = %q, want the fence %q set by TestMain", exaEndpointEnv, got, exaFence)
	}
}
