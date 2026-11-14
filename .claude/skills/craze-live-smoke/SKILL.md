---
name: craze-live-smoke
description: Drive the real craze TUI live in a dedicated tmux server to verify a change by eye — a live smoke, tmux smoke, mac-mini smoke, ssh/256-colour colour check (decoding SGR codes), input-burst or typing check, or a layout check against the fake agent — with scratch HOME/CRAZE_HOME, bracketed-paste typing, bounded waits, and no paid turn beyond the owner's budget. Use when a change must be seen working against real cursor-agent, grok, gx or native, on Linux or the mac-mini.
---

# craze live smoke

A live smoke runs the real `craze` binary in a detached tmux pane of a fixed size, types into it, and reads the
screen back with `capture-pane`. The helper is `smoke.sh` beside this file (`smoke.sh --help`). Every example
here runs from the root of the checkout under test (`make build` done) and sets up and removes its own scratch:

```shell
D=$PWD/.claude/skills/craze-live-smoke    # absolute: the pane's directory is not yours
H=$(mktemp -d /tmp/czs.XXXX) R=$(mktemp -d /tmp/czr.XXXX)   # scratch home; a short runtime dir
mkdir -m 0700 $H/home $H/run $H/ws $H/bin && cp bin/craze $H/bin/
$D/smoke.sh start v1 -c $H/ws env HOME=$H/home CRAZE_HOME=$H/home/.craze CRAZE_RUNTIME_DIR=$R \
    XDG_RUNTIME_DIR=$H/run FIREWORKS_API_KEY=fw_dummy_0000 $H/bin/craze --provider native
$D/smoke.sh wait  v1 'shift\+tab' 30       # the composer's mode hint: the session is ready
$D/smoke.sh type  v1 'go up the tree and the end of it'
$D/smoke.sh key   v1 BSpace BSpace         # named keys go through send-keys
$D/smoke.sh snap  v1 $H/v1.txt --ansi
$D/smoke.sh stop  v1                       # Escape, C-d, kill the server, confirm it is gone
rm -rf $H $R
```

Every tmux call `smoke.sh` makes, local or on the mac-mini, is bounded at 20 s (`CRAZE_SMOKE_BOUND_S`) by
`timeout`, else `gtimeout` (Homebrew coreutils), else a perl fork + alarm that ends the call's whole process group;
with none of them it refuses (exit 3) rather than make an unbounded call. A call that hits the bound (TERM, then
KILL after 2 s) exits 124, from any command, `wait` and `snap` included; `wait` captures into a temporary file,
so a hung server cannot hold it past the bound, and `snap` writes `<file>.part` and moves it into place only on
success, so a failed capture leaves an earlier snap alone. `stop` exits 0 only once a probe reports the server
(mac: the session) absent (`no server running`, no socket file, `can't find session`; a probe that catches the
server mid-teardown, `server exited unexpectedly`, is asked again for up to ~3 s), and removes the dead server's
socket file; an already-absent one counts as gone, but a `Permission denied` or any other connection error does
not (exit 1). With no arguments `smoke.sh` prints its help to stderr and exits 2.

`smoke.sh` starts its tmux server under a UTF-8 locale (`LC_ALL`: C.UTF-8 on Linux, en_US.UTF-8 on macOS, picked
from `locale -a`), which the panes inherit: under `C`, tmux draws every non-ASCII character as `_`.

## Rules

1. **A dedicated tmux server, never the default one, never a broad kill.** On Linux `smoke.sh` gives each
   smoke its own server, `tmux -L craze-<name>` (started with `-f /dev/null`), and `stop` ends it with
   `kill-server`. On the mac-mini use **only** `tmux -L smoke`, in a session of your own, and kill only that
   session (`smoke.sh --host mac … stop`). Never `pkill`/`killall`: kill only PIDs you started. A `stop`
   that exits non-zero means the smoke may still be running: look, do not assume it ended. Smokes that share
   a `CRAZE_SMOKE_SOCK` share one server, so stopping any of them ends them all: leave it unset to keep
   smokes independent.
2. **A scratch environment.** Scratch `HOME`, `CRAZE_HOME` and `XDG_RUNTIME_DIR` (`mkdir -m 0700`), plus a
   short `CRAZE_RUNTIME_DIR` (`mktemp -d /tmp/czr.XXXX`): craze refuses a control socket path over 100 bytes
   (`internal/rundir`), and a scratch `XDG_RUNTIME_DIR` too long for one is passed over silently for the
   owner's own `/run/user/<uid>/craze`, while an unusable `CRAZE_RUNTIME_DIR` is an error. A scratch
   workspace, given to `smoke.sh start -c <dir>`: without it the pane starts in your own directory, and a
   craze started there works in it (the owner's checkout included). A smoke is $0 by default: a dummy key
   such as `fw_dummy_0000…` when a provider needs one, and the fake model for a native turn (below). Never
   the owner's `~/.craze`, with one exception: a smoke that needs a real native turn (rule 5) funds it by
   copying `~/.craze/native/providers.toml` into the scratch `CRAZE_HOME/native/` (`cp`, then `chmod 0600`)
   and nothing else from `~/.craze`. Never cat, print or grep that copy; when the smoke ends, delete the
   scratch home with it and confirm it is gone. Build first (`make build`) and **copy** the binaries into
   the scratch dir (craze re-execs its resolved file; never a symlink). Never print environment variables,
   config files or whole host logs.
3. **Type with a bracketed paste** (`smoke.sh type`, which is
   `printf '%s' "$t" | tmux … load-buffer -b smoke - ; tmux … paste-buffer -p -d -b smoke -t <s>`): the text
   arrives as one paste, the default for text. `send-keys -l` used to drop words that are key names (`up`,
   `end`, `home`, `tab`…), because bubbletea groups a fast burst of runes into one key message; craze now
   splits a burst into its keys (SF-40, `internal/tui/burst.go`), so a burst or typing check types as keys
   with `smoke.sh key <name> -l 'text'`. **Named keys** (`Enter`, `Escape`, `C-d`, `BSpace`, `PgDn`) go
   through `smoke.sh key`. To quit from any state: Escape, then C-d (`stop` does this). To clear a draft use
   backspaces (`smoke.sh key <name> -N 80 BSpace`), not Escape: Escape keeps a dismissed slash menu closed for
   the same token.
4. **Waits.** Poll with `smoke.sh wait <name> <regex> <secs>`, always with a ceiling (at least `<secs>`;
   whole seconds, rounded up, on a bash older than 5). The composer is ready when its status row shows the
   mode hint: wait on `'shift\+tab'`, not `bypass permissions`, which an error screen shows too. Against
   cursor, wait about 12 s after the composer appears before typing a slash token: its command catalog
   arrives seconds after `session/new`.
5. **Never press an Enter that submits a prompt unless the smoke is meant to send.** A real model turn (funded
   only as rule 2 says) is allowed only within the owner's budget — a couple of dollars per run unless a fix
   needs more — and the report must say when one was spent (what, which model, roughly what it cost). Prefer
   the fake agent, the fake model and dummy keys.
6. **Reading results.** The TUI runs in the alternate screen, so there is no scrollback: read a native
   session's transcript with `dump.py` beside this file (`python3 $D/dump.py`, by its full path). A transcript
   is `$CRAZE_HOME/native/sessions/<workspace slug>/<UTC stamp>_<provider session id>.jsonl` (the slug is the
   workspace path less its leading `/`, each `/` turned into `-`, wrapped in `--`: `/tmp/ws` is `--tmp-ws--`;
   the stamp is `yyyymmddThhmmssZ`), so glob for the id:
   `python3 $D/dump.py $CRAZE_HOME/native/sessions/*/*_<id>.jsonl`. The id is the provider's, not the craze
   id `craze ps` prints: take it from `$CRAZE_HOME/sessions.jsonl` (a row's `sessionId`) or from
   `craze ps --json` (`.row.providerSessionId`, run with the smoke's own environment). `craze ps` starts a hub
   that outlives `stop` and exits on its own after about a minute idle, so prefer `sessions.jsonl`, and after a
   `craze ps` give the hub that minute before removing the scratch dirs. Menus need wide panes
   (`CRAZE_SMOKE_COLS=250`); page through them with `PgDn`.
7. **Colour checks.** `smoke.sh snap <name> <file> --ansi` keeps the SGR codes (`capture-pane -p -e`). Decode
   them rather than eyeballing: `38;5;N` is a 256-colour foreground, `38;2;R;G;B` truecolor, `30–37`/`90–97`
   the 16-colour palette. To see what a user over ssh gets, start craze with
   `env -u COLORTERM TERM=xterm-256color …`, or `env -u COLORTERM TERM=xterm …` for 16 colours. ssh does not
   forward `COLORTERM`, but a pane does: the tmux server hands it the environment of the client that started
   it (yours, `COLORTERM=truecolor` included, on tmux 3.4), and tmux 3.7c sets it itself, so only an unset in
   the pane's own command takes it away.

## The mac-mini

Read `~/.claude/plans/craze/mac-mini/README.md` first; `~/.claude/plans/craze/mac-mini/mac-health.sh` must print
`MAC_HEALTH_OK` before any mac smoke. `smoke.sh --host mac` drives the smoke server through
`~/.claude/plans/craze/mac-mini/mac-tmux.sh` (its panes belong to the GUI login session, so cursor-agent can
reach the login keychain; over plain ssh it cannot). That `~` is the owner's home from the password database, not
`$HOME`, so a scratch `HOME` exported around `smoke.sh` does not move it; `CRAZE_SMOKE_MAC_DIR` names another
folder. Build a committed sha there with `mac-build.sh` from the
same folder. Every remote tmux call must be bounded (`smoke.sh` bounds each at 20 s, as above, and refuses to run
without a bounded runner): a tmux server blocked on a macOS privacy prompt hangs its clients for ever — see the
runbook's "privacy-prompt trap". If the runbook or its scripts are missing, `smoke.sh --host mac` exits 3: ask
the owner.

**ssh to the mac-mini does not carry the remote exit status back** (`ssh mac-mini 'exit 3'` returns 0, checked
2026-10-04), so judge every mac step by its output, never its exit code: real `ok` lines from `mac-test.sh` (its
`MAC_EXIT=0` alone proves nothing), the `MAC_BUILD_OK` line, a capture. `smoke.sh --host mac stop` reads the smoke
server's session list for that reason.

## The fake agent

For layout checks that need no real agent, `tests/cli/tmux_smoke.py` stays the in-repo driver of the fake agent
(opt-in, never in CI):

```shell
H=$(mktemp -d /tmp/czs.XXXX) && mkdir -m 0700 $H/bin && cp bin/craze bin/craze-fake-agent $H/bin/
(cd tests/cli && LC_ALL=C.UTF-8 CRAZE_TMUX=1 CRAZE_BIN=$H/bin/craze \
    CRAZE_FAKE_AGENT_BIN=$H/bin/craze-fake-agent CRAZE_SMOKE_OUT=$H/captures uv run pytest -rA tmux_smoke.py)
rm -rf $H
```

`CRAZE_BIN` and `CRAZE_FAKE_AGENT_BIN` default to the checkout's `bin/` (where `make build` puts both), shown
here pointing at scratch copies; `CRAZE_SMOKE_OUT` (the captures) defaults to `<repo>/smoke-captures/`, which is
git-ignored. Run it under a UTF-8 locale (`LC_ALL=C.UTF-8` on Linux, `en_US.UTF-8` on macOS); its tmux server
gets one of its own either way (under `C`, tmux draws non-ASCII as `_`, and the title, permission, hang and
help-dialog cases fail). Use `-rA` for a line per case: the suite's `addopts = "-q"` swallows `-v`.

## A $0 native turn

A native turn needs no real model: `fakemodel.py` beside this file serves `tests/cli/sse_fixture.py`'s
OpenAI-compatible endpoint on 127.0.0.1 and writes a scratch `CRAZE_HOME/native` (`providers.toml`, with the
fixture's fake key, and `models.toml`, whose one model is `Fixture Model`) pointing craze at it. It scripts one
turn: a `read` of `notes.txt` in the workspace, then an answer quoting it. Every 0.5 s it rewrites its
`--state` file (`pid=`, `port=`, `requests=`, `remaining=`: the scripted steps left, `0` once the turn has run);
a request past the script gets an error, not a guess. It runs until SIGTERM (`fakemodel.py --help`;
`--fixtures <dir>` when `tests/cli` is not the one beside this checkout). Pressing Enter here costs nothing
(rule 5):

```shell
D=$PWD/.claude/skills/craze-live-smoke
H=$(mktemp -d /tmp/czs.XXXX) R=$(mktemp -d /tmp/czr.XXXX)
mkdir -m 0700 $H/home $H/run $H/ws $H/bin && cp bin/craze $H/bin/
printf 'The secret word is banana-42.\n' >$H/ws/notes.txt
python3 $D/fakemodel.py --native-dir $H/home/.craze/native --state $H/model.state & M=$!
for _ in $(seq 50); do [ -s $H/model.state ] && break; sleep 0.2; done   # its config is written
$D/smoke.sh start n1 -c $H/ws env HOME=$H/home CRAZE_HOME=$H/home/.craze CRAZE_RUNTIME_DIR=$R \
    XDG_RUNTIME_DIR=$H/run $H/bin/craze --provider native
$D/smoke.sh wait n1 'shift\+tab' 30
$D/smoke.sh type n1 'Read notes.txt and tell me the secret word.'
$D/smoke.sh key  n1 Enter
$D/smoke.sh wait n1 'banana-42 is the secret word' 30
grep -x 'remaining=0' $H/model.state                       # both scripted steps were taken
id=$(python3 -c 'import json,sys; print(json.loads(open(sys.argv[1]).readlines()[-1])["sessionId"])' \
    $H/home/.craze/sessions.jsonl)
python3 $D/dump.py $H/home/.craze/native/sessions/*/*_$id.jsonl  # CALL read, RESULT read, TEXT …
$D/smoke.sh stop n1
kill $M; wait $M; rm -rf $H $R
```
