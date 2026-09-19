package harness

import "github.com/charliek/craze/internal/harness/tool"

// systemPrompt builds a session's system prompt: the session's tool
// profile's (plan 019 §3.6), from its workspace and the operating system
// (runtime.GOOS). The profile writes the text for its own tools — the
// opencode profile's is opencode's default prompt reduced to what is true of
// craze, with H1's environment block — and the session fixes it when it
// opens, as it fixes the profile.
//
// Open calls it once and the session sends the result, unchanged, with
// every request: it holds no clock, no git state, and nothing else that
// changes between requests, so every request in a session starts with the
// same bytes and a provider's prefix cache can hit (plan 018 §3.7, owner
// decision 6, D-30). Changing a byte of it changes every session's
// prompt-cache prefix and the hash in new transcripts' headers;
// testdata/system_prompt.golden pins the opencode profile's. It is never
// stored; the transcript header records its SHA-256.
func systemPrompt(p tool.Profile, workspace, goos string) string {
	return p.System(tool.SystemEnv{Workspace: workspace, OS: goos})
}
