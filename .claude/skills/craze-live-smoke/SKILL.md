---
name: craze-live-smoke
description: Drive the real craze TUI live in a dedicated tmux server to verify a change by eye — a live smoke, tmux smoke, mac-mini smoke, ssh/256-colour colour check (decoding SGR codes), input-burst or typing check, or a layout check against the fake agent — with scratch HOME/CRAZE_HOME, bracketed-paste typing, bounded waits, and no paid turn beyond the owner's budget. Use when a change must be seen working against real cursor-agent, grok, gx or native, on Linux or the mac-mini.
---

# craze live smoke

A live smoke runs the real `craze` binary in a detached tmux pane of a fixed size, types into it, and reads the
screen back with `capture-pane`. The helper is `smoke.sh` beside this file (`smoke.sh --help`):

```shell
S=.claude/skills/craze-live-smoke/smoke.sh
$S start v1 env HOME=$H CRAZE_HOME=$H/.craze XDG_RUNTIME_DIR=$H/run $B/craze --provider native
$S wait  v1 'bypass permissions' 30       # poll the pane, with a ceiling
$S type  v1 'go up the tree and the end of it'
$S key   v1 BSpace BSpace                 # named keys go through send-keys
$S snap  v1 /tmp/craze-smoke-v1.txt --ansi
$S stop  v1                               # Escape, C-d, kill the server, confirm it is gone
```

Every tmux call `smoke.sh` makes, local or on the mac-mini, is bounded at 20 s (`CRAZE_SMOKE_BOUND_S`) by
`timeout`, else `gtimeout` (Homebrew coreutils), else a perl fork + alarm that ends the call's whole process group;
with none of them it refuses (exit 3) rather than make an unbounded call. A call that hits the bound (TERM, then
KILL after 2 s) exits 124, from any command, `wait` and `snap` included. `stop` exits 0 only once a probe reports
the server (mac: the session) absent (`no server running`, no socket file, `can't find session`); an already-absent
one counts as gone, but a `Permission denied` or any other connection error does not (exit 1).

## Rules

1. **A dedicated tmux server, never the default one, never a broad kill.** On Linux `smoke.sh` gives each
   smoke its own server, `tmux -L craze-<name>` (started with `-f /dev/null`), and `stop` ends it with
   `kill-server`. On the mac-mini use **only** `tmux -L smoke`, in a session of your own, and kill only that
   session (`smoke.sh --host mac … stop`). Never `pkill`/`killall`: kill only PIDs you started. A `stop`
   that exits non-zero means the smoke may still be running: look, do not assume it ended.
2. **A scratch environment.** Scratch `HOME`, `CRAZE_HOME` and `XDG_RUNTIME_DIR` (`mkdir -m 0700`), a dummy
   key such as `fw_dummy_0000…` when a provider needs one. Never the owner's `~/.craze` or a real key. Build
   first (`make build`) and **copy** the binaries into the scratch dir (craze re-execs its resolved file;
   never a symlink). Never print environment variables, config files or whole host logs.
3. **Type with a bracketed paste** (`smoke.sh type`, which is
   `printf '%s' "$t" | tmux … load-buffer -b x - ; tmux … paste-buffer -p -t <s> -b x -d`). `send-keys -l`
   drops words that are key names (`up`, `end`, `home`, `tab`…): bubbletea groups a fast burst of runes into
   one key message. **Named keys** (`Enter`, `Escape`, `C-d`, `BSpace`, `PgDn`) go through `smoke.sh key`.
   To quit from any state: Escape, then C-d (`stop` does this). To clear a draft use backspaces, not Escape:
   Escape keeps a dismissed slash menu closed for the same token.
4. **Waits.** Poll with `smoke.sh wait <name> <regex> <secs>`, always with a ceiling. Against cursor, wait
   about 12 s after the composer appears before typing a slash token: its command catalog arrives seconds
   after `session/new`.
5. **Never press Enter unless the smoke is meant to send.** A real model turn is allowed only within the
   owner's budget — a couple of dollars per run unless a fix needs more — and the report must say when one was
   spent (what, which model, roughly what it cost). Prefer the fake agent and dummy keys.
6. **Reading results.** The TUI runs in the alternate screen, so there is no scrollback: read a native
   session's transcript with `dump.py` beside this file. A transcript is named `<UTC stamp>_<session id>.jsonl`
   (the stamp is `yyyymmddThhmmssZ`), so glob for the id:
   `python3 dump.py $CRAZE_HOME/native/sessions/<slug>/*_<session id>.jsonl`. Menus need wide panes
   (`CRAZE_SMOKE_COLS=250`); page through them with `PgDn`.
7. **Colour checks.** `smoke.sh snap <name> <file> --ansi` keeps the SGR codes (`capture-pane -p -e`). Decode
   them rather than eyeballing: `38;5;N` is a 256-colour foreground, `38;2;R;G;B` truecolor, `30–37`/`90–97`
   the 16-colour palette. To see what a user over ssh gets, start craze with `TERM=xterm-256color` and
   `COLORTERM` unset (ssh does not forward it; tmux 3.7c sets it itself), or `TERM=xterm` for 16 colours.

## The mac-mini

Read `~/.claude/plans/craze/mac-mini/README.md` first; `~/.claude/plans/craze/mac-mini/mac-health.sh` must print
`MAC_HEALTH_OK` before any mac smoke. `smoke.sh --host mac` drives the smoke server through
`~/.claude/plans/craze/mac-mini/mac-tmux.sh` (its panes belong to the GUI login session, so cursor-agent can
reach the login keychain; over plain ssh it cannot). Build a committed sha there with `mac-build.sh` from the
same folder. Every remote tmux call must be bounded (`smoke.sh` bounds each at 20 s, as above, and refuses to run
without a bounded runner): a tmux server blocked on a macOS privacy prompt hangs its clients for ever — see the
runbook's "privacy-prompt trap". If the runbook or its scripts are missing, `smoke.sh --host mac` exits 3: ask
the owner.

## The fake agent

For layout checks that need no real agent, `tests/cli/tmux_smoke.py` stays the in-repo driver of the fake agent
(`cd tests/cli && CRAZE_TMUX=1 uv run pytest -v tmux_smoke.py`; opt-in, never in CI).
