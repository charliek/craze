package chatgptauth_test

import (
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/llm"
)

// *TokenSource is the llm.Auth the ChatGPT plan's driver takes (plan 033
// §3.10, X105), structurally: the package itself imports nothing of the
// harness (P31), so the check lives in this external test.
var _ llm.Auth = (*chatgptauth.TokenSource)(nil)
