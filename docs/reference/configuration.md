# Configuration

craze persists one setting today: the theme. Session flags (`--workspace`,
`--model`, `--force`, `--ask` / `--plan`, `--agent-bin`) are per-invocation.

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
```

Unrelated keys in that file are preserved. A file craze cannot parse is never
clobbered, and the save that refused to overwrite it is reported in the
transcript. A malformed file does not prevent startup; the theme falls back to
the default.

`craze frame` never reads this file. See [Testing](../development/testing.md#craze-frame).

## Environment

| Variable | Purpose |
|----------|---------|
| `CRAZE_CONFIG` | Replace the config path (`~/.craze/config.toml`) |
| `CRAZE_AGENT_BIN` | Agent binary when `--agent-bin` is unset |

If neither `--agent-bin` nor `CRAZE_AGENT_BIN` is set, craze looks for
`cursor-agent`, then `agent`, on `PATH`.
