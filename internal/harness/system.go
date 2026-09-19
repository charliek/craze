package harness

import "fmt"

// systemTemplate is the whole system prompt but for the two facts it is
// filled with. It is honest about H1: the model has no tools, so it must not
// claim to have run a command or read a file, and it is told what to do
// instead. Changing a byte of it changes every session's prompt-cache prefix
// and the hash in new transcripts' headers; testdata/system_prompt.golden
// pins it.
const systemTemplate = `You are a coding assistant inside craze, a terminal client, talking with a software developer about their work.

You have no tools in this session: you cannot run commands, read or write files, or browse the web, and you see only what the user writes into this conversation. Never claim to have run, opened, or changed anything. When a task needs something you cannot do, say so, and give the user the exact commands or edits to apply themselves.

Be direct and concise. Put code, commands, and file contents in fenced Markdown code blocks.

Environment:
- Working directory: %s
- Operating system: %s
`

// systemPrompt builds a session's system prompt from its workspace and the
// operating system (runtime.GOOS). Open calls it once and the session sends
// the result, unchanged, with every request: it holds no clock, no git
// state, and nothing else that changes between requests, so every request
// in a session starts with the same bytes and a provider's prefix cache can
// hit (plan 018 §3.7, owner decision 6). It is never stored; the transcript
// header records its SHA-256.
func systemPrompt(workspace, goos string) string {
	return fmt.Sprintf(systemTemplate, workspace, goos)
}
