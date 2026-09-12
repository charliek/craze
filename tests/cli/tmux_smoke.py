"""tmux smoke driver — plan 004 §3.15 layer 4.

This is the only layer that drives the *real* TUI in a *real* terminal: craze
runs inside a detached tmux pane of an exact size, keys go in through
``send-keys`` and the screen comes back out through ``capture-pane``. Every
other layer (goldens, ``craze frame``, the PTY tests) renders the model without
a terminal emulator in the loop, so this is what proves the alt screen, the
height contract and the key handling against something that redraws.

It is deliberately **not in CI** and not collected by ``make test-cli``: the
file name does not match pytest's ``python_files``, so it is only collected
when it is named on the command line, and even then every case skips unless
``tmux`` is on PATH and ``CRAZE_TMUX`` is set.

Run it either way, after ``make build``::

    cd tests/cli && CRAZE_TMUX=1 uv run pytest -v tmux_smoke.py
    cd tests/cli && CRAZE_TMUX=1 uv run python tmux_smoke.py --scripts echo,todos

Captures land in ``<out>/<script>-<cols>x<rows>-<step>.txt``, with ``<out>``
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
EXIT_TIMEOUT = 10.0
# One keystroke per send-keys call, spaced out: bubbletea v1 coalesces
# consecutive printable bytes from one read into a single KeyRunes message, so
# "1", " ", "3" arriving together would reach the question card as one key.
KEY_GAP = 0.15

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


Step = Send | Wait | Gone | Watch

# Named tmux keys, to keep the tables readable. Everything else is sent with
# `send-keys -l`, so a bare "a" is the letter and never a key name.
ENTER = "Enter"
ESC = "Escape"
QUIT = "C-d"
NAMED_KEYS = frozenset({ENTER, ESC, QUIT})


@dataclass(frozen=True)
class Case:
    """One fake script and what §3.15 says its screen must show."""

    prompt: str | None = "go"
    steps: tuple[Step, ...] = ()
    no_force: bool = False
    clean_exit: bool = True
    note: str = ""


# The per-script expectations are §3.15's, verbatim. Scripts §3.15 does not
# name still run: they get the height contract, the clean exit, and a wait on
# their own reply text so the turn is actually finished when craze is quit.
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
        steps=(Wait("echo: go", "reply"), Wait("Fake Title ─", "title-rule")),
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
    # authfail never gets a session, so there is no prompt to send.
    "authfail": Case(
        prompt=None,
        clean_exit=False,
        steps=(Wait("authentication failed", "auth-error"),),
        note=(
            "craze shows the error and stays up, but Model.startErr has to ride out "
            "on the final model or tui.Run reports a clean quit for a session that "
            "never started"
        ),
    ),
    "noauth": Case(steps=(Wait("echo: go", "reply"),)),
}

SCRIPTS = tuple(CASES)


# ------------------------------------------------------------------ the pane


class SmokeFailure(AssertionError):
    """A per-script expectation that the screen did not meet."""


class TmuxPane:
    """A detached tmux session of an exact size running one craze."""

    def __init__(
        self,
        *,
        craze: Path,
        fake_agent: Path,
        script: str,
        cols: int,
        rows: int,
        work: Path,
        out_dir: Path,
        no_force: bool,
    ) -> None:
        self.script = script
        self.cols = cols
        self.rows = rows
        self.work = work
        self.out_dir = out_dir
        self.fake_agent = fake_agent
        self.socket = f"craze-smoke-{os.getpid()}"
        self.session = f"{script}-{cols}x{rows}"
        self.home = work / "home"
        self.ws = work / "ws"
        self.exit_file = work / "exit.txt"
        self.stderr_file = work / "stderr.txt"
        self.home.mkdir(parents=True, exist_ok=True)
        self.ws.mkdir(parents=True, exist_ok=True)
        self.captured: list[Path] = []

        argv = [
            str(craze),
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
            "export TERM=xterm-256color\n"
            f"export CRAZE_FAKE_SCRIPT={shlex.quote(script)}\n"
            "unset CRAZE_AGENT_BIN\n"
            f"{' '.join(shlex.quote(a) for a in argv)} 2>{shlex.quote(str(self.stderr_file))}\n"
            f"printf '%s\\n' \"$?\" > {shlex.quote(str(self.exit_file))}\n"
        )

    # -- tmux plumbing

    def _tmux(self, *args: str, check: bool = True) -> str:
        proc = subprocess.run(
            [TMUX, "-L", self.socket, *args],
            capture_output=True,
            text=True,
        )
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
        self.start()
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

    def alive(self) -> bool:
        return not self.exit_file.exists()

    # -- assertions

    def check_frame(self, lines: list[str]) -> None:
        """The height contract, measured on a real terminal.

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
        # Status row 2 (the permission chip) is the bottom of the frame, except
        # for the sub-agent rows §3.9 puts underneath it (4 plus an overflow).
        tail = lines[-6:]
        if not any(ln.startswith("\u25b8") for ln in tail):
            raise SmokeFailure(
                "the frame does not reach the bottom of the pane:\n" + "\n".join(lines)
            )

    def capture(self, step: str, lines: list[str] | None = None) -> Path:
        rows = self.lines() if lines is None else lines
        self.check_frame(rows)
        path = self.out_dir / f"{self.script}-{self.cols}x{self.rows}-{step}.txt"
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
    script: str,
    cols: int,
    rows: int,
    *,
    craze: Path,
    fake_agent: Path,
    work: Path,
    out_dir: Path,
) -> list[Path]:
    case = CASES[script]
    out_dir.mkdir(parents=True, exist_ok=True)
    try:
        return _run_case(script, case, cols, rows, craze, fake_agent, work, out_dir)
    except AssertionError as err:
        # The note names what was already known about this expectation, so a
        # failure points at the unit that owns it instead of at the driver.
        if case.note:
            raise SmokeFailure(f"{err}\n\nnote: {case.note}") from err
        raise


def _run_case(
    script: str,
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
        script=script,
        cols=cols,
        rows=rows,
        work=work,
        out_dir=out_dir,
        no_force=case.no_force,
    ) as pane:
        # The status rows carry no status word; `cursor` is the provider
        # segment of row 1, which only exists once the frame is drawn.
        pane.wait_for("cursor", "start", timeout=START_TIMEOUT)
        if case.prompt is not None:
            pane.send(case.prompt, ENTER)
        for step in case.steps:
            if isinstance(step, Send):
                pane.send(*step.keys)
            elif isinstance(step, Wait):
                pane.wait_for(step.text, step.step)
            elif isinstance(step, Gone):
                pane.wait_gone(step.text, step.step)
            else:
                pane.watch(step)
        code = pane.quit_and_wait()
        if case.clean_exit and code != 0:
            err = pane.stderr_file.read_text() if pane.stderr_file.exists() else ""
            raise SmokeFailure(f"craze exited {code}, wanted 0: {err[-2000:]}")
        if not case.clean_exit and code == 0:
            raise SmokeFailure("craze exited 0, wanted a nonzero exit")
        _wait_fake_gone(fake_agent)
        return pane.captured


# ---------------------------------------------------------------- pytest use


@pytest.mark.skipif(not (TMUX and ENABLED), reason=SKIP_REASON)
@pytest.mark.parametrize("cols,rows", SIZES, ids=[f"{c}x{r}" for c, r in SIZES])
@pytest.mark.parametrize("script", SCRIPTS)
def test_tmux_smoke(
    script: str,
    cols: int,
    rows: int,
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
) -> None:
    run_case(
        script,
        cols,
        rows,
        craze=craze_bin,
        fake_agent=fake_agent_bin,
        work=tmp_path,
        out_dir=Path(os.environ.get("CRAZE_SMOKE_OUT") or DEFAULT_OUT),
    )


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
    ap.add_argument("--scripts", default=",".join(SCRIPTS))
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

    scripts = [s.strip() for s in args.scripts.split(",") if s.strip()]
    unknown = [s for s in scripts if s not in CASES]
    if unknown:
        raise SystemExit(f"unknown script(s): {', '.join(unknown)}")

    failures: list[tuple[str, str]] = []
    root = Path(tempfile.mkdtemp(prefix="craze-smoke-"))
    try:
        for cols, rows in _parse_sizes(args.sizes):
            for script in scripts:
                name = f"{script} {cols}x{rows}"
                work = root / f"{script}-{cols}x{rows}"
                work.mkdir(parents=True, exist_ok=True)
                try:
                    run_case(
                        script,
                        cols,
                        rows,
                        craze=craze,
                        fake_agent=fake_agent,
                        work=work,
                        out_dir=out_dir,
                    )
                except AssertionError as err:
                    failures.append((name, str(err)))
                    print(f"FAIL  {name}")
                    print("      " + str(err).replace("\n", "\n      "))
                else:
                    print(f"ok    {name}")
    finally:
        if not args.keep:
            shutil.rmtree(root, ignore_errors=True)

    print(f"\ncaptures in {out_dir}")
    if failures:
        print(f"{len(failures)} failed: " + ", ".join(n for n, _ in failures))
        return 1
    print("all scripts passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
