"""tmux smoke driver — plan 004 §3.15 layer 4, extended by 005 §5 (V6).

This is the only layer that drives the *real* TUI in a *real* terminal: craze
runs inside a detached tmux pane of an exact size, keys go in through
``send-keys`` and the screen comes back out through ``capture-pane``. Every
other layer (goldens, ``craze frame``, the PTY tests) renders the model without
a terminal emulator in the loop, so this is what proves the alt screen, the
height contract and the key handling against something that redraws.

The mouse comes in the same way. ``send-keys`` writes bytes straight into the
pane's tty, so a drag is the three SGR reports (``ESC [ < b ; x ; y M|m``) an
xterm would have sent, delivered with ``send-keys -H``; bubbletea parses mouse
sequences off stdin unconditionally, so what craze receives is exactly what a
hand on a mouse produces. tmux's own ``mouse`` option is not involved — that one
only decides what tmux does with events from the *outer* terminal, and there is
no outer terminal here.

It is deliberately **not in CI** and not collected by ``make test-cli``: the
file name does not match pytest's ``python_files``, so it is only collected
when it is named on the command line, and even then every case skips unless
``tmux`` is on PATH and ``CRAZE_TMUX`` is set.

Run it either way, after ``make build``::

    cd tests/cli && CRAZE_TMUX=1 uv run pytest -v tmux_smoke.py
    cd tests/cli && CRAZE_TMUX=1 uv run python tmux_smoke.py --cases echo,todos

Captures land in ``<out>/<case>-<cols>x<rows>-<step>.txt``, with ``<out>``
defaulting to ``./smoke-captures/`` (git-ignored), relative to the repo root
(``CRAZE_SMOKE_OUT`` or ``--out`` override it).
"""

from __future__ import annotations

import argparse
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

import pytest

from conftest import ROOT
from test_tui import _wait_fake_gone

# ------------------------------------------------------------------ settings

SIZES = ((100, 30), (80, 24))
THEME = "craze-dark"
DEFAULT_OUT = ROOT / "smoke-captures"

START_TIMEOUT = 20.0
WAIT_TIMEOUT = 15.0
# TMUX_TIMEOUT bounds one tmux command. It is not how long a case may take —
# it is how long a single `capture-pane` or `send-keys` may hang before the
# suite calls it a failure instead of waiting for ever.
TMUX_TIMEOUT = 20.0
EXIT_TIMEOUT = 10.0
# One keystroke per send-keys call, spaced out: bubbletea v1 coalesces
# consecutive printable bytes from one read into a single KeyRunes message, so
# "1", " ", "3" arriving together would reach the question card as one key.
KEY_GAP = 0.15

# 005 §3.2 put the mode chip at the start of status row 2, which is the bottom
# of the frame. Every script that gets a session shows one of these; `authfail`
# never gets one, so its row 2 starts with the permission chip instead. Grok
# names its default mode `default`, so that chip is here too.
MODE_CHIPS = ("\u25c6 agent", "\u25c6 plan", "\u25c6 ask", "\u25c6 default")
PERMISSION_CHIPS = ("\u25b8\u25b8 ", "\u25b8 ")

TMUX = shutil.which("tmux")
ENABLED = os.environ.get("CRAZE_TMUX", "") not in ("", "0")
SKIP_REASON = (
    "tmux smoke is opt-in: needs tmux on PATH and CRAZE_TMUX=1 (never in CI)"
)


# -------------------------------------------------------------- the step DSL


class Send:
    """Keystrokes, one tmux send-keys call each."""

    def __init__(self, *keys: str) -> None:
        self.keys = keys


@dataclass(frozen=True)
class Wait:
    """Poll the pane until ``text`` shows up, then capture it as ``step``."""

    text: str
    step: str


@dataclass(frozen=True)
class Gone:
    """Poll the pane until ``text`` is no longer on it, then capture it."""

    text: str
    step: str


@dataclass(frozen=True)
class Absent:
    """Capture the pane as it stands and assert ``text`` is *not* on it.

    Unlike ``Gone`` this does not wait, so it is only sound where a preceding
    ``Wait`` has pinned a state the screen cannot leave on its own — a card up
    with the agent blocked on the answer, say. Anywhere else "not there yet" and
    "not there" would be the same thing and the step would prove nothing.
    """

    text: str
    step: str


@dataclass(frozen=True)
class Ends:
    """Poll the pane until some line *ends* with ``text``, then capture it.

    A substring is not enough for the titled rule: 005 §3.1 puts the session
    title at the right *end* of the composer's top rule, and only a line that
    finishes with it proves that. capture-pane strips trailing blanks, so the
    end of a captured line is the last cell craze drew on it.
    """

    text: str
    step: str


@dataclass(frozen=True)
class Drag:
    """One left-button drag over the pane, from cell to cell, 0-based."""

    x1: int
    y1: int
    x2: int
    y2: int


@dataclass(frozen=True)
class Watch:
    """Two expectations on one transient sequence.

    ``first`` is a state the pane passes *through* on its way to ``until``; the
    poll records it if it is ever painted. Used for the sub-agent row, which
    §3.15 wants to see running before it is done.
    """

    first: str
    first_step: str
    until: str
    until_step: str


Step = Send | Wait | Gone | Absent | Ends | Watch | Drag

# Named tmux keys, to keep the tables readable. Everything else is sent with
# `send-keys -l`, so a bare "a" is the letter and never a key name.
ENTER = "Enter"
ESC = "Escape"
TAB = "Tab"
RIGHT = "Right"
LEFT = "Left"
UP = "Up"
DOWN = "Down"
QUIT = "C-d"
STRONG = "C-l"
BSPACE = "BSpace"
NAMED_KEYS = frozenset(
    {ENTER, ESC, TAB, RIGHT, LEFT, UP, DOWN, QUIT, STRONG, BSPACE}
)

# SGR mouse button codes. A drag is press, one motion with the button-held bit
# set, then the release; the release repeats the button code and ends in a
# lowercase `m`, which is how bubbletea tells it from a press.
SGR_LEFT = 0
SGR_LEFT_MOTION = 32


@dataclass(frozen=True)
class Case:
    """One run of craze and what the plan says its screen must show."""

    prompt: str | None = "go"
    steps: tuple[Step, ...] = ()
    no_force: bool = False
    clean_exit: bool = True
    note: str = ""
    # script is the CRAZE_FAKE_SCRIPT to run when it is not the case's own name.
    # Cut two added cases that exercise craze rather than the wire (the model
    # dialog, a drag), and those ride on an existing script.
    script: str | None = None
    # chip is the mode chip every capture of this case must show, which is 005
    # §7's "`◆ agent` on every script". None is for the two cases where no one
    # chip holds all the way through: `planmode` starts in plan mode and ends in
    # agent, and `authfail` never gets a session to have a mode at all.
    chip: str | None = MODE_CHIPS[0]
    # clipboard is what the copy must have handed the platform clipboard tool,
    # checked against the stub run.sh puts on PATH.
    clipboard: str | None = None
    # provider is which craze provider to start. The grok sub-agent scripts
    # need grok: the cursor dialect drops every _x.ai/session_notification,
    # so under cursor there is no row to watch.
    provider: str = "cursor"
    # env is extra environment for the pane. The queue cases set
    # CRAZE_FAKE_STEP so the first turn is slow enough to type into.
    env: tuple[tuple[str, str], ...] = ()


# The per-case expectations are §3.15's (004) and §5 V6's (005), verbatim.
# Scripts neither names still run: they get the height contract, the mode chip,
# the clean exit, and a wait on their own reply text so the turn is actually
# finished when craze is quit.
CASES: dict[str, Case] = {
    "echo": Case(steps=(Wait("echo: go", "reply"),)),
    "followup": Case(steps=(Wait("first reply", "reply"),)),
    "tool": Case(steps=(Wait("after tool", "reply"),)),
    "tasks": Case(steps=(Wait("done tasks", "reply"),)),
    "effort": Case(steps=(Wait("echo: go", "reply"),)),
    "todos": Case(steps=(Wait("TASKS", "tasks-panel"), Wait("done todos", "reply"))),
    "todos-notify": Case(
        steps=(Wait("TASKS", "tasks-panel"), Wait("done todos", "reply"))
    ),
    "diff": Case(steps=(Wait("+1 \u22121", "diff"), Wait("done diff", "reply"))),
    "bigdiff": Case(
        steps=(Wait("diff too large", "diff"), Wait("done bigdiff", "reply"))
    ),
    "bash": Case(steps=(Wait("exit 127", "exit-code"), Wait("done bash", "reply"))),
    "task": Case(
        steps=(
            Watch("\u25cf agent", "agent-running", "\u2713 agent", "agent-done"),
            Wait("8.0s", "agent-duration"),
        ),
        note=(
            "the running row needs cmd/craze-fake-agent/server.go's taskRunFor pause "
            "to survive a render frame; without it the model holds the state but no "
            "terminal ever paints it"
        ),
    ),
    "task-late": Case(
        steps=(
            Watch("\u25cf agent", "agent-running", "\u2713 agent", "agent-done"),
            Wait("8.0s", "agent-duration"),
        ),
        note="same as `task`: taskRunFor is what makes the running row observable",
    ),
    "markdown": Case(steps=(Wait("\u2022", "bullets"),)),
    # 005 §3.1 put the session title at the right end of the composer's top
    # rule, so the screen is where it is checked now; the reply is waited on
    # first because the title update rides ahead of it.
    "title": Case(
        steps=(Wait("echo: go", "reply"), Ends(" Fake Title \u2500", "title-rule")),
        note="the session title is the right end of the composer's top rule",
    ),
    "permission": Case(
        no_force=True,
        steps=(
            Wait("[a]llow", "permission-line"),
            Send("A"),
            Wait("decision:opt-always", "allowed-always"),
        ),
    ),
    "ask": Case(
        steps=(
            Wait("question 1/2", "question-card"),
            Send("1"),
            Send(" ", "3", ENTER),
            Wait("asked:answered:", "answered"),
        )
    ),
    "plan": Case(
        steps=(
            Wait("PLAN", "plan-card"),
            Send("a"),
            Wait("planned:accepted", "accepted"),
        )
    ),
    "hang": Case(
        prompt="hang",
        steps=(
            Wait("esc to interrupt", "working"),
            Send(ESC),
            # The spinner going away is "Esc cancels"; the word is the second half.
            Gone("esc to interrupt", "cancelled-idle"),
            Wait("cancelled", "cancelled"),
        ),
        note=(
            "the note comes from internal/tui/app.go's "
            "EventDone{StopReason:\"cancelled\"} branch; without it Esc's only "
            "visible effect is the spinner going away"
        ),
    ),
    # authfail never gets a session, so there is no prompt to send and no mode
    # to put in a chip.
    "authfail": Case(
        prompt=None,
        clean_exit=False,
        chip=None,
        steps=(Wait("authentication failed", "auth-error"),),
        note=(
            "craze shows the error and stays up, but Model.startErr has to ride out "
            "on the final model or tui.Run reports a clean quit for a session that "
            "never started"
        ),
    ),
    "noauth": Case(steps=(Wait("echo: go", "reply"),)),
    # ------------------------------------------------------------ 005 §5 (V6)
    # The plan-mode exit on a real terminal. The fake starts this script in plan
    # mode, so the offer is reachable; the placeholder is waited on rather than
    # the reply alone because the offer needs both of the turn's endings and
    # those two race, so what the composer draws is the proof both landed.
    #
    # Enter then goes in on an *empty* composer, and the fake answers
    # "implementing" only if session/set_mode reached it before session/prompt —
    # a screen that says "WRONG ORDER" is craze having raced its own chain.
    "planmode": Case(
        prompt="plan it",
        chip=None,
        steps=(
            Wait("\u25c6 plan", "plan-chip"),
            Wait("planned: plan it", "reply"),
            Wait(
                "enter implements this plan  \u00b7  type to refine",
                "offer-placeholder",
            ),
            Send(ENTER),
            Wait("implementing: Implement the plan above.", "implementing"),
            Wait("\u25c6 agent", "agent-chip"),
        ),
        note=(
            "the offer is armed by internal/tui/app.go's turnSeq bookkeeping and "
            "drawn by composer.go's planOfferPlaceholder; WRONG ORDER on the screen "
            "means the set_mode/prompt chain was batched instead"
        ),
    ),
    # The plan-mode turn a real cursor-agent sends, which `planmode` alone does
    # not model: assistant text, then a cursor/create_plan card, then more
    # assistant text, and only then the turn's ending. The card therefore lands
    # before the events that arm the offer, which is exactly what used to retire
    # the offer for good — so `offer-after-card` is the step that fails without
    # the fix. `no-offer-while-carded` is the weaker half: the fake is blocked on
    # the answer, so what it pins is that a card on its own is not a turn ending.
    "planmode-card": Case(
        prompt="plan it",
        chip=None,
        steps=(
            Wait("PLAN Print current time", "plan-card"),
            Absent("enter implements this plan", "no-offer-while-carded"),
            Send("a"),
            Wait("planned: plan it", "reply"),
            Wait(
                "enter implements this plan  \u00b7  type to refine",
                "offer-after-card",
            ),
            Send(ENTER),
            Wait("implementing: Implement the plan above.", "implementing"),
            Wait("\u25c6 agent", "agent-chip"),
        ),
        note=(
            "the card must not kill the offer: internal/tui/cards.go's pushCard "
            "leaves it standing, and internal/tui/app.go's planOffering is what "
            "hides it while a card is open"
        ),
    ),
    # 005 §3.4's dialog, opened by /model and driven with the keys the golden
    # uses: tab twice reaches the fast row, one right turns it on, Enter applies
    # just that step. Status row 1 carrying "· fast" is the proof the advertised
    # value went out to the agent and came back on a snapshot.
    "model-dialog": Case(
        script="echo",
        prompt=None,
        steps=(
            Send("/model"),
            Send(ENTER),
            Wait("type to filter \u00b7 \u2191\u2193 \u00b7 tab effort/fast", "dialog"),
            Send(TAB, TAB, RIGHT),
            Wait("fast  off  [on]", "fast-row-armed"),
            Send(ENTER),
            Wait("fast \u2192 on", "fast-note"),
            Wait("Default (medium \u00b7 fast)", "fast-status-row"),
        ),
        note=(
            "the fast toggle's value is a string ('true'), not a JSON boolean \u2014 see "
            "\u00a710's V1 plan correction \u2014 so a dialog that applied but left row 1 "
            "unchanged means the round-trip through configOptions broke"
        ),
    ),
    # /help is a dialog in the same frame as /model now, so a real terminal is
    # where its geometry is actually painted: the box is centred, the status rows
    # underneath it survive (the chip probe every capture runs), and the content
    # is clipped with a ▼ rather than overflowing.
    "help-dialog": Case(
        script="echo",
        prompt=None,
        steps=(
            Send("/help"),
            Send(ENTER),
            Wait("sending and editing", "help-open"),
            Wait("▼", "help-clipped"),
            Send(ESC),
            Gone("sending and editing", "help-closed"),
        ),
    ),
    # 005 §3.5 through a real terminal's mouse: the drag covers the user line and
    # the reply, so it copies two rows and says so in status row 2. The payload
    # is checked against the stub clipboard tool, which is what proves the copy
    # actually left craze rather than only that the note was painted.
    "drag": Case(
        script="echo",
        steps=(
            Wait("echo: go", "reply"),
            Drag(0, 0, 7, 1),
            Wait("copied 2 lines", "copied"),
        ),
        clipboard="\u276f go\necho: go",
        note=(
            "the release is what finalises the selection and copies; an X10-style "
            "release reports no button, so internal/tui/app.go tracks the button "
            "that went down"
        ),
    ),
    # ------------------------------------------------------------ 007 §3.9
    # The grok sub-agent scripts, watched on the band and driven into the
    # read-only view. They run under --provider grok (the cursor dialect drops
    # the notifications, so under cursor there is no row to watch) and grok's
    # default mode chip leads status row 2.
    "grok-subagent": Case(
        provider="grok",
        chip="\u25c6 default",
        steps=(
            Watch("\u25cb explore", "agent-running", "\u2713 explore", "agent-done"),
            Wait("2.9s", "agent-duration"),
            Send(DOWN, ENTER),
            Wait("esc to return", "view"),
            Send(UP),
            Wait("(grok-4.6)", "chip"),
            Send(LEFT),
            Gone("esc to return", "view-closed"),
        ),
        note=(
            "the running row needs the fake's taskRunFor pauses to survive a "
            "render frame, like `task`; the chip wait after \u2191 is on the live "
            "pane, so it is also the proof that scrolling inside the view did not "
            "leave it"
        ),
    ),
    "grok-subagent-two": Case(
        provider="grok",
        chip="\u25c6 default",
        steps=(
            Wait("List python files", "row-1"),
            Wait("Report README first line", "row-2"),
            Wait("4.7k tok", "progress"),
            Send(DOWN, ENTER),
            Wait("List the python files.", "view-1"),
            Send(TAB),
            Wait("Report the first line.", "view-2"),
            Send(ESC),
            Gone("esc to return", "view-closed"),
        ),
        note=(
            "rows sit in spawn order and the selection starts on the first, so "
            "Enter opens sub-1 and Tab moves to sub-2; the two prompts are the "
            "only text that tells the views apart"
        ),
    ),
    "grok-subagent-fail": Case(
        provider="grok",
        chip="\u25c6 default",
        steps=(
            Watch("\u25cb explore", "agent-running", "\u2717 explore", "agent-failed"),
            Wait("subagent failed", "reply"),
        ),
    ),
    "grok-subagent-cancel": Case(
        provider="grok",
        chip="\u25c6 default",
        steps=(
            Wait("\u25cb general-purpose", "agent-running"),
            Send(ESC),
            Wait("\u2013 general-purpose", "agent-cancelled"),
            Send(DOWN, ENTER),
            Wait("@general-purpose \u00b7 cancelled", "view"),
            Send(ESC),
            Gone("esc to return", "view-closed"),
        ),
        note=(
            "the fake holds the parent until session/cancel and finishes the "
            "child cancelled 400 ms later, so the row flip is the wire order, "
            "not craze inventing an end"
        ),
    ),
    "grok-subagent-late": Case(
        provider="grok",
        chip="\u25c6 default",
        steps=(
            Wait("\u25cb explore", "agent-running"),
            Gone("esc to interrupt", "parent-idle"),
            Wait("\u2713 explore", "agent-done"),
        ),
        note=(
            "the parent ends the turn while the child still runs, and the finish "
            "that lands 400 ms later is what flips the row \u2014 done is not EOF "
            "here either"
        ),
    ),
    # The cursor view rides on the task script the way the model dialog rides
    # on echo: cursor streams no sub-agent transcript, so the view is what the
    # cursor/task receipt carried, in the same chrome grok's view uses.
    "task-view": Case(
        script="task",
        steps=(
            Watch("\u25cf agent", "agent-running", "\u2713 agent", "agent-done"),
            Wait("8.0s", "agent-duration"),
            Send(DOWN, ENTER),
            Wait("\u2713 @task \u00b7 completed \u00b7 receipt only", "view"),
            Wait("esc to return", "banner"),
            Send(ESC),
            Gone("esc to return", "view-closed"),
        ),
        note=(
            "the band row lingers ten seconds after the TUI saw it finish, which "
            "is the window the down/enter pair has to land in"
        ),
    ),
    # 008: the queue band in a real terminal. The first turn is slow enough to
    # type into and the ones behind it finish at once, so the drain is
    # observable without waiting on a clock.
    "queue": Case(
        script="long-turn",
        prompt="go the long way",
        env=(("CRAZE_FAKE_STEP", "6s,1ms"),),
        steps=(
            Wait("esc to interrupt", "working"),
            Send("Reply with PINEAPPLE", ENTER),
            Wait("#1 Reply with PINEAPPLE", "queued-one"),
            Send("Reply with MANGO", ENTER),
            Wait("#2 Reply with MANGO", "queued-two"),
            Wait("⧗ 2 queued", "queue-count"),
            Send(UP),
            Wait("❯ #2 Reply with MANGO", "selected"),
            Wait("[send now] [edit] [cancel]", "actions"),
            Send(BSPACE),
            Gone("MANGO", "cancelled-row"),
            Wait("⧗ 1 queued", "one-left"),
            Wait("❯ Reply with PINEAPPLE", "drained"),
        ),
        note=(
            "Enter queues while a turn runs, ↑ selects, BSpace cancels a row, "
            "and the head is sent when the turn settles"
        ),
    ),
    "grok-interject": Case(
        script="grok-long-turn",
        provider="grok",
        chip="◆ default",
        prompt="go the long way",
        env=(("CRAZE_FAKE_STEP", "6s,1ms"),),
        steps=(
            Wait("esc to interrupt", "working"),
            Send("Also say BANANA", STRONG),
            Wait("↳ Also say BANANA", "interjection"),
            Wait("DONE step1 Also say BANANA step2", "merged-reply"),
        ),
        note=(
            "ctrl+l on grok merges the draft into the running turn: the ↳ entry "
            "comes from the agent's broadcast and the reply carries the word"
        ),
    ),
}

CASE_IDS = tuple(CASES)


# ------------------------------------------------------------------ the pane


class SmokeFailure(AssertionError):
    """A per-case expectation that the screen did not meet."""


# CLIP_STUB stands in for the platform clipboard tool. craze's copy is OSC 52
# first and then the native tool, and the native half would otherwise shell out
# to the developer's own wl-copy or xclip and replace whatever they had on their
# clipboard — the same reason `go test` stubs nativeCopy. Stubbing it here keeps
# the real code path (the binary is looked up and executed for real) and turns
# the payload into something a case can assert on.
#
# atotto/clipboard picks its tool at init from the environment, so all four names
# it might choose are installed: wl-copy/wl-paste when WAYLAND_DISPLAY is set,
# otherwise xclip, otherwise xsel. A paste invocation is the one carrying an
# output flag; everything else is a copy.
CLIP_STUB = """#!/bin/sh
# generated by tests/cli/tmux_smoke.py
for arg in "$@"; do
    case "$arg" in
        -out|--output|--no-newline) exec cat "$CRAZE_SMOKE_CLIPBOARD" ;;
    esac
done
exec cat > "$CRAZE_SMOKE_CLIPBOARD"
"""
CLIP_STUB_NAMES = ("wl-copy", "wl-paste", "xclip", "xsel")


class TmuxPane:
    """A detached tmux session of an exact size running one craze."""

    def __init__(
        self,
        *,
        craze: Path,
        fake_agent: Path,
        name: str,
        script: str,
        cols: int,
        rows: int,
        work: Path,
        out_dir: Path,
        no_force: bool,
        chip: str | None,
        provider: str = "cursor",
        env: tuple[tuple[str, str], ...] = (),
    ) -> None:
        self.name = name
        self.script = script
        self.cols = cols
        self.rows = rows
        self.work = work
        self.out_dir = out_dir
        self.fake_agent = fake_agent
        self.chip = chip
        self.socket = f"craze-smoke-{os.getpid()}"
        self.session = f"{name}-{cols}x{rows}"
        self.home = work / "home"
        self.ws = work / "ws"
        self.exit_file = work / "exit.txt"
        self.stderr_file = work / "stderr.txt"
        self.clipboard_file = work / "clipboard.txt"
        self.home.mkdir(parents=True, exist_ok=True)
        self.ws.mkdir(parents=True, exist_ok=True)
        self.captured: list[Path] = []

        stub_bin = work / "bin"
        stub_bin.mkdir(parents=True, exist_ok=True)
        for stub in CLIP_STUB_NAMES:
            path = stub_bin / stub
            path.write_text(CLIP_STUB)
            path.chmod(0o755)

        argv = [
            str(craze),
            "--provider",
            provider,
            "--agent-bin",
            str(fake_agent),
            "--workspace",
            str(self.ws),
            "--theme",
            THEME,
        ]
        if no_force:
            argv.append("--no-force")
        # A script, not `tmux new-session ... env`, so HOME is isolated the
        # same way on every tmux build and the exit status survives the pane.
        self.run_sh = work / "run.sh"
        self.run_sh.write_text(
            "#!/bin/sh\n"
            "# generated by tests/cli/tmux_smoke.py\n"
            f"export HOME={shlex.quote(str(self.home))}\n"
            f"export PATH={shlex.quote(str(stub_bin))}:$PATH\n"
            f"export CRAZE_SMOKE_CLIPBOARD={shlex.quote(str(self.clipboard_file))}\n"
            "export TERM=xterm-256color\n"
            f"export CRAZE_FAKE_SCRIPT={shlex.quote(script)}\n"
            + "".join(
                f"export {k}={shlex.quote(v)}\n" for k, v in env
            )
            + "unset CRAZE_AGENT_BIN\n"
            "unset CRAZE_PROVIDER\n"
            "unset CRAZE_CONFIG\n"
            f"{' '.join(shlex.quote(a) for a in argv)} 2>{shlex.quote(str(self.stderr_file))}\n"
            f"printf '%s\\n' \"$?\" > {shlex.quote(str(self.exit_file))}\n"
        )

    # -- tmux plumbing

    def _tmux(self, *args: str, check: bool = True) -> str:
        # `-f /dev/null` as well as the private `-L` socket: the socket keeps the
        # suite out of the developer's server, but without `-f` the server it
        # starts still reads their ~/.tmux.conf. A config with
        # `set-clipboard on` plus a `pane-set-clipboard` hook pipes the pane's
        # OSC 52 straight into the real clipboard, and the hook runs in tmux's
        # environment, not the pane's, so the stubs on the pane PATH would not
        # catch it — the assertion would pass while the developer's clipboard
        # changed. tmux only reads the file when it starts the server, so
        # passing the flag on every command is harmless.
        # Every tmux call is bounded: without a timeout a wedged tmux blocks
        # the polling loop for ever and WAIT_TIMEOUT never gets a chance to
        # fire, so a stuck case hangs the suite instead of failing it.
        try:
            proc = subprocess.run(
                [TMUX, "-f", "/dev/null", "-L", self.socket, *args],
                capture_output=True,
                text=True,
                timeout=TMUX_TIMEOUT,
            )
        except subprocess.TimeoutExpired as err:
            raise SmokeFailure(
                f"tmux {' '.join(args)} did not return within {TMUX_TIMEOUT}s"
            ) from err
        if check and proc.returncode != 0:
            raise SmokeFailure(
                f"tmux {' '.join(args)} failed ({proc.returncode}): {proc.stderr.strip()}"
            )
        return proc.stdout

    def start(self) -> None:
        self._tmux(
            "new-session",
            "-d",
            "-s",
            self.session,
            "-x",
            str(self.cols),
            "-y",
            str(self.rows),
            "-c",
            str(self.work),
            f"sh {shlex.quote(str(self.run_sh))}",
        )
        # No status line, so the pane is the whole window and the frame craze
        # draws is the frame this asserts on.
        self._tmux("set-option", "-t", self.session, "status", "off")
        size = self._tmux(
            "display-message", "-p", "-t", self.session, "-F", "#{pane_width}x#{pane_height}"
        ).strip()
        if size != f"{self.cols}x{self.rows}":
            raise SmokeFailure(f"pane is {size}, wanted {self.cols}x{self.rows}")

    def close(self) -> None:
        self._tmux("kill-server", check=False)

    def __enter__(self) -> TmuxPane:
        # A failure inside start() — the size assertion, a tmux option — must
        # not leak the server it may already have started.
        try:
            self.start()
        except BaseException:
            self.close()
            raise
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # -- screen

    def lines(self) -> list[str]:
        return self._tmux("capture-pane", "-p", "-t", self.session, check=False).split("\n")[
            : self.rows
        ]

    def screen(self) -> str:
        return "\n".join(self.lines())

    def send(self, *keys: str) -> None:
        for key in keys:
            if key in NAMED_KEYS:
                self._tmux("send-keys", "-t", self.session, key)
            else:
                self._tmux("send-keys", "-t", self.session, "-l", key)
            time.sleep(KEY_GAP)

    def drag(self, spec: Drag) -> None:
        """One left-button drag, as three SGR reports into the pane's tty.

        This is a real gesture, not an injected message: the bytes are what an
        xterm in 1006 mode sends, and bubbletea parses them off stdin. The three
        reports are spaced like keystrokes so each one arrives in its own read —
        a report split across reads would still be reassembled, but a failure
        then points at one report instead of at the batch.
        """
        self._sgr(SGR_LEFT, spec.x1, spec.y1, "M")
        self._sgr(SGR_LEFT_MOTION, spec.x2, spec.y2, "M")
        self._sgr(SGR_LEFT, spec.x2, spec.y2, "m")

    def _sgr(self, button: int, x: int, y: int, final: str) -> None:
        # SGR coordinates are 1-based; bubbletea takes the one back off. -H
        # rather than -l because the sequence starts with ESC, which tmux would
        # otherwise have to be trusted to pass through as a byte and not as a key.
        seq = f"\x1b[<{button};{x + 1};{y + 1}{final}".encode()
        self._tmux("send-keys", "-t", self.session, "-H", *[f"{b:02x}" for b in seq])
        time.sleep(KEY_GAP)

    def alive(self) -> bool:
        return not self.exit_file.exists()

    # -- assertions

    def check_frame(self, lines: list[str]) -> None:
        """The height contract and the mode chip, measured on a real terminal.

        craze computes one frame per Update and pads it to the window, so the
        pane must be exactly as tall as it was made and nothing may wrap: a
        line wider than the pane would push the bottom of the frame off the
        screen, which is what the chip check catches.
        """
        if len(lines) != self.rows:
            raise SmokeFailure(
                f"pane has {len(lines)} rows, wanted {self.rows}:\n" + "\n".join(lines)
            )
        widest = max((len(ln) for ln in lines), default=0)
        if widest > self.cols:
            raise SmokeFailure(f"a line is {widest} cells wide, wanted <= {self.cols}")
        # Status row 2 is the bottom of the frame, except for the sub-agent rows
        # §3.9 puts underneath it (4 plus an overflow). 005 §3.2 moved the mode
        # chip to the front of that row, so the chip is now both the "the frame
        # reached the bottom" probe and §7's "◆ agent on every script"; when the
        # session never advertised a mode, the permission chip leads instead.
        tail = lines[-6:]
        if not any(ln.startswith(MODE_CHIPS + PERMISSION_CHIPS) for ln in tail):
            raise SmokeFailure(
                "the frame does not reach the bottom of the pane:\n" + "\n".join(lines)
            )
        if self.chip and not any(ln.startswith(self.chip) for ln in tail):
            raise SmokeFailure(
                f"status row 2 does not start with {self.chip!r}:\n" + "\n".join(lines)
            )

    def capture(self, step: str, lines: list[str] | None = None) -> Path:
        rows = self.lines() if lines is None else lines
        self.check_frame(rows)
        path = self.out_dir / f"{self.name}-{self.cols}x{self.rows}-{step}.txt"
        path.write_text("\n".join(rows) + "\n")
        self.captured.append(path)
        return path

    def wait_for(self, text: str, step: str, timeout: float = WAIT_TIMEOUT) -> list[str]:
        deadline = time.monotonic() + timeout
        lines: list[str] = []
        while time.monotonic() < deadline:
            lines = self.lines()
            if text in "\n".join(lines):
                self.capture(step, lines)
                return lines
            if not self.alive():
                raise SmokeFailure(
                    f"craze exited before {text!r} appeared:\n" + "\n".join(lines)
                )
            time.sleep(0.05)
        raise SmokeFailure(f"timed out waiting for {text!r}:\n" + "\n".join(lines))

    def wait_gone(self, text: str, step: str, timeout: float = WAIT_TIMEOUT) -> list[str]:
        deadline = time.monotonic() + timeout
        lines: list[str] = []
        while time.monotonic() < deadline:
            lines = self.lines()
            if text not in "\n".join(lines):
                self.capture(step, lines)
                return lines
            if not self.alive():
                raise SmokeFailure(
                    f"craze exited while {text!r} was still up:\n" + "\n".join(lines)
                )
            time.sleep(0.05)
        raise SmokeFailure(f"timed out waiting for {text!r} to go away:\n" + "\n".join(lines))

    def check_absent(self, text: str, step: str) -> list[str]:
        lines = self.lines()
        self.capture(step, lines)
        if text in "\n".join(lines):
            raise SmokeFailure(f"{text!r} should not be on the pane:\n" + "\n".join(lines))
        return lines

    def wait_ends(self, text: str, step: str, timeout: float = WAIT_TIMEOUT) -> list[str]:
        deadline = time.monotonic() + timeout
        lines: list[str] = []
        while time.monotonic() < deadline:
            lines = self.lines()
            if any(ln.endswith(text) for ln in lines):
                self.capture(step, lines)
                return lines
            if not self.alive():
                raise SmokeFailure(
                    f"craze exited before a line ended with {text!r}:\n" + "\n".join(lines)
                )
            time.sleep(0.05)
        raise SmokeFailure(
            f"timed out waiting for a line ending in {text!r}:\n" + "\n".join(lines)
        )

    def watch(self, spec: Watch, timeout: float = WAIT_TIMEOUT) -> None:
        """Poll flat out for a state the screen only passes through."""
        deadline = time.monotonic() + timeout
        seen_first = False
        lines: list[str] = []
        while time.monotonic() < deadline:
            lines = self.lines()
            joined = "\n".join(lines)
            if not seen_first and spec.first in joined:
                seen_first = True
                self.capture(spec.first_step, lines)
            if spec.until in joined:
                self.capture(spec.until_step, lines)
                if not seen_first:
                    raise SmokeFailure(
                        f"{spec.until!r} was reached without {spec.first!r} ever being painted"
                    )
                return
            if not self.alive():
                raise SmokeFailure(
                    f"craze exited before {spec.until!r} appeared:\n" + joined
                )
        raise SmokeFailure(f"timed out waiting for {spec.until!r}:\n" + "\n".join(lines))

    def quit_and_wait(self) -> int:
        self.send(QUIT)
        deadline = time.monotonic() + EXIT_TIMEOUT
        while time.monotonic() < deadline:
            if self.exit_file.exists():
                raw = self.exit_file.read_text().strip()
                if raw:
                    return int(raw)
            time.sleep(0.05)
        raise SmokeFailure(f"craze did not exit on ctrl+d:\n{self.screen()}")


# ------------------------------------------------------------------ the case


def run_case(
    name: str,
    cols: int,
    rows: int,
    *,
    craze: Path,
    fake_agent: Path,
    work: Path,
    out_dir: Path,
) -> list[Path]:
    case = CASES[name]
    out_dir.mkdir(parents=True, exist_ok=True)
    try:
        return _run_case(name, case, cols, rows, craze, fake_agent, work, out_dir)
    except AssertionError as err:
        # The note names what was already known about this expectation, so a
        # failure points at the unit that owns it instead of at the driver.
        if case.note:
            raise SmokeFailure(f"{err}\n\nnote: {case.note}") from err
        raise


def _run_case(
    name: str,
    case: Case,
    cols: int,
    rows: int,
    craze: Path,
    fake_agent: Path,
    work: Path,
    out_dir: Path,
) -> list[Path]:
    with TmuxPane(
        craze=craze,
        fake_agent=fake_agent,
        name=name,
        script=case.script or name,
        cols=cols,
        rows=rows,
        work=work,
        out_dir=out_dir,
        no_force=case.no_force,
        chip=case.chip,
        provider=case.provider,
        env=case.env,
    ) as pane:
        # The status rows carry no status word; the provider segment of row 1
        # is what says a frame was drawn, and it only exists once it was.
        pane.wait_for(case.provider, "start", timeout=START_TIMEOUT)
        if case.prompt is not None:
            pane.send(case.prompt, ENTER)
        for step in case.steps:
            if isinstance(step, Send):
                pane.send(*step.keys)
            elif isinstance(step, Wait):
                pane.wait_for(step.text, step.step)
            elif isinstance(step, Gone):
                pane.wait_gone(step.text, step.step)
            elif isinstance(step, Absent):
                pane.check_absent(step.text, step.step)
            elif isinstance(step, Ends):
                pane.wait_ends(step.text, step.step)
            elif isinstance(step, Drag):
                pane.drag(step)
            else:
                pane.watch(step)
        check_clipboard(pane, case)
        code = pane.quit_and_wait()
        if case.clean_exit and code != 0:
            err = pane.stderr_file.read_text() if pane.stderr_file.exists() else ""
            raise SmokeFailure(f"craze exited {code}, wanted 0: {err[-2000:]}")
        if not case.clean_exit and code == 0:
            raise SmokeFailure("craze exited 0, wanted a nonzero exit")
        _wait_fake_gone(fake_agent)
        return pane.captured


def check_clipboard(pane: TmuxPane, case: Case) -> None:
    """What the copy handed the clipboard tool, once the note says it happened.

    The note is painted from the message the copy command returns, so by the
    time a `copied` wait has matched the stub has already been run and reaped.
    """
    if case.clipboard is None:
        return
    if not pane.clipboard_file.exists():
        raise SmokeFailure(
            "craze said it copied but never ran the clipboard tool:\n" + pane.screen()
        )
    got = pane.clipboard_file.read_text()
    if got != case.clipboard:
        raise SmokeFailure(f"clipboard holds {got!r}, wanted {case.clipboard!r}")


# ---------------------------------------------------------------- pytest use


@pytest.mark.skipif(not (TMUX and ENABLED), reason=SKIP_REASON)
@pytest.mark.parametrize("cols,rows", SIZES, ids=[f"{c}x{r}" for c, r in SIZES])
@pytest.mark.parametrize("case", CASE_IDS)
def test_tmux_smoke(
    case: str,
    cols: int,
    rows: int,
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
) -> None:
    run_case(
        case,
        cols,
        rows,
        craze=craze_bin,
        fake_agent=fake_agent_bin,
        work=tmp_path,
        out_dir=Path(os.environ.get("CRAZE_SMOKE_OUT") or DEFAULT_OUT),
    )


@pytest.mark.skipif(not (TMUX and ENABLED), reason=SKIP_REASON)
def test_user_tmux_config_is_not_loaded(
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The suite's server must not read the developer's tmux configuration.

    ``-L`` gives the suite its own socket but not its own config, and a
    ``~/.tmux.conf`` with ``set-clipboard on`` plus a ``pane-set-clipboard`` hook
    pipes the pane's OSC 52 into the real clipboard — from tmux's environment,
    which the pane's stub PATH never reaches, so the stub assertion would still
    pass while the developer's clipboard changed. A user option stands in for the
    hook here because it is the same question (was the file read?) and needs no
    clipboard tool to answer. The control is the argv without ``-f``: it proves
    the poisoned file is real and would have been loaded.
    """
    option = "@craze-smoke-poison"
    home = tmp_path / "poison-home"
    (home / ".config" / "tmux").mkdir(parents=True)
    conf = f"set -g {option} yes\n"
    (home / ".tmux.conf").write_text(conf)
    # tmux 3.1+ reads the XDG path too; poison both so the test does not depend
    # on which one this tmux prefers.
    (home / ".config" / "tmux" / "tmux.conf").write_text(conf)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("XDG_CONFIG_HOME", str(home / ".config"))

    pane = TmuxPane(
        craze=craze_bin,
        fake_agent=fake_agent_bin,
        name="tmux-conf",
        script="echo",
        cols=80,
        rows=24,
        work=tmp_path,
        out_dir=tmp_path,
        no_force=False,
        chip=None,
    )
    try:
        # `sleep`, not craze: what is under test is the server's configuration,
        # and the pane's contents do not matter.
        pane._tmux("new-session", "-d", "-s", pane.session, "sleep 30")
        got = pane._tmux("show-options", "-gv", option, check=False).strip()
    finally:
        pane.close()
    if got:
        raise SmokeFailure(f"the suite's tmux server loaded ~/.tmux.conf ({option}={got})")

    socket = f"craze-poison-{os.getpid()}"
    control = [TMUX, "-L", socket]
    try:
        subprocess.run([*control, "new-session", "-d", "-s", "control", "sleep 30"], check=True)
        seen = subprocess.run(
            [*control, "show-options", "-gv", option], capture_output=True, text=True
        ).stdout.strip()
    finally:
        subprocess.run([*control, "kill-server"], capture_output=True)
    if seen != "yes":
        raise SmokeFailure("the poisoned config never took effect, so the check above proves nothing")


# ------------------------------------------------------------------ CLI use


def _resolve_bins() -> tuple[Path, Path]:
    craze = Path(os.environ.get("CRAZE_BIN") or ROOT / "bin" / "craze")
    fake = Path(os.environ.get("CRAZE_FAKE_AGENT_BIN") or ROOT / "bin" / "craze-fake-agent")
    for path in (craze, fake):
        if not path.is_file():
            raise SystemExit(f"{path} is missing; run `make build`")
    return craze, fake


def _parse_sizes(raw: str) -> list[tuple[int, int]]:
    out = []
    for part in raw.split(","):
        cols, _, rows = part.strip().partition("x")
        out.append((int(cols), int(rows)))
    return out


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", default=os.environ.get("CRAZE_SMOKE_OUT") or str(DEFAULT_OUT))
    # --scripts is the old spelling, kept so a recorded command line still runs;
    # the values are case names, which for every 004 case is the script's name.
    ap.add_argument("--cases", "--scripts", dest="cases", default=",".join(CASE_IDS))
    ap.add_argument("--sizes", default=",".join(f"{c}x{r}" for c, r in SIZES))
    ap.add_argument("--keep", action="store_true", help="keep the per-case temp dirs")
    args = ap.parse_args(argv)

    if not TMUX:
        print("SKIP: tmux is not on PATH")
        return 0
    if not ENABLED:
        print("SKIP: set CRAZE_TMUX=1 to run the smoke suite")
        return 0

    craze, fake_agent = _resolve_bins()
    out_dir = Path(args.out).expanduser()
    out_dir.mkdir(parents=True, exist_ok=True)

    cases = [s.strip() for s in args.cases.split(",") if s.strip()]
    unknown = [s for s in cases if s not in CASES]
    if unknown:
        raise SystemExit(f"unknown case(s): {', '.join(unknown)}")

    failures: list[tuple[str, str]] = []
    root = Path(tempfile.mkdtemp(prefix="craze-smoke-"))
    try:
        for cols, rows in _parse_sizes(args.sizes):
            for case in cases:
                label = f"{case} {cols}x{rows}"
                work = root / f"{case}-{cols}x{rows}"
                work.mkdir(parents=True, exist_ok=True)
                try:
                    run_case(
                        case,
                        cols,
                        rows,
                        craze=craze,
                        fake_agent=fake_agent,
                        work=work,
                        out_dir=out_dir,
                    )
                except AssertionError as err:
                    failures.append((label, str(err)))
                    print(f"FAIL  {label}")
                    print("      " + str(err).replace("\n", "\n      "))
                else:
                    print(f"ok    {label}")
    finally:
        if not args.keep:
            shutil.rmtree(root, ignore_errors=True)

    print(f"\ncaptures in {out_dir}")
    if failures:
        print(f"{len(failures)} failed: " + ", ".join(n for n, _ in failures))
        return 1
    print("all cases passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
