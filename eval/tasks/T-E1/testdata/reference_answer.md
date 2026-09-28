Native keeps the prefix stable by building the system prompt once per session from inputs that cannot change mid-session, and by putting everything that does change into messages after it.

**Where the prompt is built**

- `systemPrompt` (`internal/harness/system.go:42-44`) calls the tool profile's `System` func. For the opencode profile that is `systemFunc` over the embedded `system.txt` (`internal/harness/tool/opencode/opencode.go:86-104`). Its only inputs are `tool.SystemEnv{Workspace, OS}` (`internal/harness/tool/profile.go:36-42`) — no clock, no git state, no model name.
- `withPromptExtras` (`system.go:176-200`) appends the instruction files (AGENTS.md and friends) and the skills catalog after the profile text, under fixed budgets (32 KiB per document, 96 KiB total, 24 KiB of catalog, `system.go:78-96`). With nothing to add it returns the profile text unchanged, and it refuses any result the profile text is not a prefix of.
- `openTools` freezes the result in `ts.system` (`internal/harness/tools.go:287-294`); the session copies it to `s.system` (`harness.go:547`, and `resume.go:79` on resume). Every turn request and the compaction summarizer send that same string (`harness.go:381-386`, `compact.go:676`). This is decision D-30 in `discovery/native-harness/08-decisions.md`.
- The tool specs are frozen at Open as well (`ts.specs`/`ts.wire`, serialised deterministically by `SpecsJSON`) and always offered in the same order, which is part of the cached prefix.

**What varies**

- Between sessions: the workspace path, the OS, the instruction files and the catalog as they were at Open.
- Within a session: nothing in the system prompt. Mode changes arrive as `<system-reminder>` user messages spliced after the prompt (`reminders.go:65`, `steer.go:270-302`); the transcript stores the reminder's variant and re-renders it byte for byte. History is replayed in path order (`store/transcript.go:349-359`), so each request extends the last one — until a compaction succeeds: after it the history is the `<compacted_context>` summary message (`summary.go:26`) plus the kept tail, so the next request shares only the system prompt and tools with the one before.
- A model switch (`SetModel`, `harness.go:700`) keeps the prompt; a model whose tool profile differs is refused with `ErrProfileMismatch`.

**Sub-agents**

A same-profile child gets `BaseSystem: parent.system` (`subagents.go:816`), and `withChildRole` (`child.go:163-169`) appends its role section, so the parent's prompt is a byte prefix of the child's. A child on another profile renders its own prompt (`tools.go:289-293`). The child's tool list is smaller (`childWithheld`), so its tools array differs from the parent's.

**Resume**

Resume re-renders the prompt from the header's tool profile and the current extras (`resume.go:68-82`) and records its SHA-256 (`promptDigest`). The transcript stores only that hash, never the text (`store/entry.go:589-590`). If the hash changed (an edited AGENTS.md, say) it is recorded in the resume entry's contract; nothing refuses or warns, but the first request after resume will miss the cache.
