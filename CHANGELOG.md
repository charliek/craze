# Changelog

Each release gets its own section headed `## vX.Y.Z — YYYY-MM-DD`, newest first.

## v0.0.1 — 2026-09-16

First release. craze is a Linux and macOS terminal UI that speaks ACP to an
agent you already have: **Cursor** (`cursor-agent acp`), **Grok**
(`grok agent stdio`), or **gx**, a third-party fork of the Grok CLI. craze owns
the chrome; the provider still runs the agent. It is a proof of concept —
usable, but the version number is honest.

### Install

Homebrew (`brew install charliek/tap/craze`) and apt
(`https://apt.stridelabs.ai`), both from this release. Apple Silicon and Linux
amd64/arm64 are tested; Intel macOS is cross-compiled and shipped best-effort.

### The terminal UI

- A transcript of rendered entries — markdown, tool rows rewritten in place,
  diffs, todos and streamed assistant text — rather than a debug log.
- A boxed composer that keeps the whole draft on screen, with slash commands
  that open on a token anywhere in the draft.
- Questions, plans and permission requests arrive as cards you answer inline.
- Sub-agent rows for both providers, in spawn order, with a read-only view of
  any child's own transcript.
- Status rows and a clickable mode chip; mouse selection and copy from the
  transcript; wheel scroll separate from PgUp/PgDn.
- Seven themes with a picker that repaints live as you move through it, and a
  themed terminal background (OSC 10/11) that is handed back on exit —
  `background = false` or `--no-background` turns it off.

### Sessions

- `--continue` resumes the last session; `--resume` opens a picker; `/rename`
  names one. The terminal's tab title follows the session.
- A message queue: type while a turn runs, send now, or interject mid-turn.
- Plan mode offers to implement the plan when a plan-mode turn ends.

### Providers

- Cursor, Grok and gx, chosen with `--provider` or a startup picker that only
  offers the ones that actually resolve on your machine.
- Cursor's plugin commands are found and expanded client-side, because its ACP
  server never advertises them.
- Skills are scanned by each agent's own rules rather than one guess at both.

### Headless

- `craze prompt` drives an ACP turn without a TUI, for scripting and tests.
- `craze frame` renders a deterministic frame, which is what the golden suite
  compares.

### Known limits

- Proof of concept: multi-project harness integration is later work.
- Intel macOS is untested at runtime.
- A terminal that honours OSC 11 but not OSC 10 (or that ignores the reset)
  can be left recoloured after an abnormal exit; `background = false` avoids
  the whole mechanism.
