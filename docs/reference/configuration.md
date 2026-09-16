# Configuration

craze persists the theme and the last successfully started **provider**.
Session flags (`--workspace`, `--model`, `--force`, `--ask` / `--plan`,
`--agent-bin`) are per-invocation.

## Theme precedence

An explicit `--theme` beats the config file, which beats `craze-dark`.

| Source | When it wins |
|--------|----------------|
| `--theme <name>` | Flag was passed (including `--theme ""`) |
| `~/.craze/config.toml` | No `--theme` on the command line, and the file names a theme |
| `craze-dark` | Neither of the above |

## Presets

Seven presets. craze never paints a full-screen background, so your
terminal's own background shows through.

| Name | Notes |
|------|-------|
| `craze-dark` | Default |
| `craze-light` | |
| `tokyo-night` | Aliases: `tokyonight`, `tokyo` |
| `dark` | |
| `light` | |
| `catppuccin` | |
| `gruvbox` | |

`craze` and `crazedark` are aliases for `craze-dark`; `crazelight` is an alias
for `craze-light`.

`Ctrl+G` or `/theme` opens a names-only picker that repaints the live screen as
the cursor moves; `Enter` keeps the theme and saves it, `Esc` puts back the one
that was active when the picker opened. `/theme <name>` sets and saves one
directly.

## Config file

The saved theme lives in `~/.craze/config.toml`. `CRAZE_CONFIG` overrides the
whole path.

```toml
theme = "gruvbox"
provider = "grok"
```

Unrelated keys in that file are preserved. A file craze cannot parse is never
clobbered, and the save that refused to overwrite it is reported in the
transcript. A malformed file does not prevent startup; the theme falls back to
the default.

`craze frame` never reads this file, and never reads `$CRAZE_PROVIDER`. See
[Testing](../development/testing.md#craze-frame).

## Provider precedence

Ids are `cursor`, `grok`, and `gx`. An empty string (`--provider ""`,
`$CRAZE_PROVIDER=""`, `provider = ""`) is unset, not unknown.

`gx` is [`charliek/grok-build`](https://github.com/charliek/grok-build), a
third-party fork of the Grok CLI that speaks the same ACP dialect as
`grok` — same wire protocol, same auth, same skills and plugins — so
everything on this page and in the rest of the docs about Grok's behaviour
applies to it too. It currently shares grok's `~/.grok` home (config, auth,
sessions, skills); that is the fork's current behaviour, not a guarantee
craze makes.

gx is gated separately from the other two ids: it appears in the startup
picker only when a binary for it resolves (see [Environment](#environment)
below for lookup order); cursor and grok are always offered, so the picker
can never be empty. `--provider gx` works regardless of whether a binary
resolves, and fails at spawn — `agent binary not found: …` — if it does
not; this matches how `craze prompt --provider gx` and `craze frame
--provider gx` already behave, since neither consults the picker. Setting
`--agent-bin` or `$CRAZE_AGENT_BIN` also makes gx appear in the picker,
because with either set a gx session genuinely would spawn that binary; the
override is exclusive, so one pointing at nothing hides gx even when `gx`
is itself on `PATH`.

For the TUI and `craze prompt`, the first hit wins:

1. `--provider <id>` when the flag was passed (`Changed`).
2. `$CRAZE_PROVIDER` if non-empty.
3. `provider` in `~/.craze/config.toml` if it is a known id.
4. `cursor`.

Unknown `--provider` exits 2. Unknown env or config id warns and falls back to
cursor; the fallback is not written back. After a successful `Start`, the TUI
and `craze prompt` save the provider that actually started (an explicit picker
choice overwrites a stale unknown id; `Esc` on the fallback default does not).

`--agent-bin` / `$CRAZE_AGENT_BIN` override the **binary**. They do not select
the dialect.

## Environment

| Variable | Purpose |
|----------|---------|
| `CRAZE_CONFIG` | Replace the config path (`~/.craze/config.toml`) |
| `CRAZE_AGENT_BIN` | Agent binary when `--agent-bin` is unset |
| `CRAZE_PROVIDER` | Provider id when `--provider` is unset (`cursor`, `grok`, or `gx`) |
| `XAI_API_KEY` | Grok API key; used when initialize advertises `xai.api_key` |
| `GROK_CODE_XAI_API_KEY` | Legacy alias for `XAI_API_KEY` |

If neither `--agent-bin` nor `CRAZE_AGENT_BIN` is set, Cursor looks for
`cursor-agent`, then `agent`, on `PATH`. Grok looks for `grok` only. gx looks
for `gx` only.
