package chatgptauth_test

import (
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/llm"
)

// *TokenSource is the llm.Auth the ChatGPT plan's driver takes (plan 033
// §3.10, X105), structurally: the package itself imports nothing of the
// harness (P31), so the check lives in this external test.
var _ llm.Auth = (*chatgptauth.TokenSource)(nil)

// And the two optional sides package llm looks for on an Auth (auth.go): the
// sentinels its scrubber keeps through every error it rebuilds, and the usage
// latch its driver sets on the plan's usage limit (plan 033 §3.12, P33). llm's
// interfaces are its own; these are their method sets.
var (
	_ interface{ Sentinels() []error } = (*chatgptauth.TokenSource)(nil)
	_ interface{ LatchUsageLimit() }   = (*chatgptauth.TokenSource)(nil)
)
