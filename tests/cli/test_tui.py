from __future__ import annotations

import errno
import fcntl
import json
import os
import pty
import re
import select
import signal
import struct
import subprocess
import sys
import termios
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest
from conftest import host_env_names
from sse_fixture import CANARY, UNUSED_ENV_KEY, SSEFixture, write_native_config


def _open_pty() -> tuple[int, int]:
    try:
        return pty.openpty()
    except OSError as err:
        pytest.skip(f"no pty: {err}")


def _set_winsize(fd: int, rows: int = 24, cols: int = 80) -> None:
    packed = struct.pack("HHHH", rows, cols, 0, 0)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, packed)


def _cmdline_pids(needle: str) -> list[int]:
    """Every pid whose argv[0] is exactly needle.

    argv[0], not "needle appears somewhere in argv": craze's own process
    names the fake agent's path too, as --agent-bin's value, so a substring
    search would also catch craze itself while it is still running -- which
    a caller hunting for the agent's own pid to signal must never do. Only
    Linux has /proc; everywhere else ps -o pid=,args= is the one portable
    view of another process's argv paired with its pid, and an absolute
    path's own first token is its argv[0] there too. check=True on purpose:
    a caller waiting for the list to empty reads [] as proof the child is
    gone, so a ps that failed must raise rather than quietly turn a leak
    check green.
    """
    encoded = needle.encode()
    if sys.platform != "linux":
        out = subprocess.run(
            ["ps", "-axww", "-o", "pid=,args="],
            capture_output=True,
            check=True,
        ).stdout
        pids = []
        for line in out.splitlines():
            pid_str, _, args = line.strip().partition(b" ")
            if pid_str.isdigit() and args.split(b" ", 1)[0] == encoded:
                pids.append(int(pid_str))
        return pids
    try:
        entries = Path("/proc").glob("[0-9]*/cmdline")
    except OSError:
        return []
    pids = []
    for path in entries:
        try:
            data = path.read_bytes()
        except OSError:
            continue
        if data.split(b"\0", 1)[0] == encoded:
            pids.append(int(path.parent.name))
    return pids


def _cmdline_has(needle: str) -> bool:
    return bool(_cmdline_pids(needle))


class PTYCraze:
    def __init__(
        self,
        craze_bin: Path,
        fake_agent_bin: Path | None,
        workspace: Path,
        script: str = "echo",
        provider: str = "cursor",
        no_mouse: bool = False,
        step: str | None = None,
        extra_args: list[str] | None = None,
        env_extra: dict[str, str] | None = None,
        setctty: bool = False,
    ) -> None:
        self.fake_agent_bin = fake_agent_bin
        self.buf = bytearray()
        self._closed = threading.Event()
        self._master_open = False
        self._answers_queries = setctty
        self._answered_to = 0
        master, slave = _open_pty()
        _set_winsize(slave)
        _set_winsize(master)
        env = os.environ.copy()
        env["TERM"] = "xterm-256color"
        env["CRAZE_FAKE_SCRIPT"] = script
        env["HOME"] = str(workspace)
        env.pop("CRAZE_AGENT_BIN", None)
        env.pop("CRAZE_PROVIDER", None)
        # The craze directory follows HOME here, so a case seeds its config
        # and its session index under <workspace>/.craze. A case that wants
        # CRAZE_HOME passes it in env_extra.
        env.pop("CRAZE_HOME", None)
        # Never inherited: a developer with CRAZE_FAKE_STEP exported would
        # otherwise change the timing of every case that did not ask for it.
        env.pop("CRAZE_FAKE_STEP", None)
        # Nor CI. termenv reports "not a terminal" whenever CI is set
        # (termenv.go isTTY), which drops the colour profile to Ascii, and
        # craze paints no colour -- and sets no terminal colours -- when it
        # cannot paint at all. This harness exists to drive craze the way a
        # real terminal does, and a real terminal has no CI in its environment,
        # so popping it is what makes a CI run behave like a developer's.
        env.pop("CI", None)
        # Nor any herdr or roost variable: craze reports its status to the host
        # those name, and this suite may be running inside a real one. conftest
        # already strips them from os.environ; this holds for a PTYCraze built
        # outside that fixture too. A host-status test passes its own in
        # env_extra, which is applied after this.
        for name in host_env_names(env):
            del env[name]
        # The native fixture's env_keys name: set, it would outrank the inline
        # canary key and a native test could pass without sending the key it
        # configured.
        env.pop(UNUSED_ENV_KEY, None)
        if step:
            env["CRAZE_FAKE_STEP"] = step
        env.update(env_extra or {})
        argv = [str(craze_bin)]
        if provider:
            # An empty provider leaves the flag off entirely, which is the
            # only way to exercise what an *implicit* provider does: an
            # explicit --provider filters the session index and is still
            # allowed to write the persisted default.
            argv += ["--provider", provider]
        if fake_agent_bin is not None:
            # None means an in-process provider (native, plan 018): there is
            # no binary to spawn, and --agent-bin with one is a usage error
            # (internal/cli/provider.go's refuseInProcess).
            argv += ["--agent-bin", str(fake_agent_bin)]
        argv += [
            "--workspace",
            str(workspace),
            "--theme",
            "tokyo-night",
        ]
        if no_mouse:
            argv.append("--no-mouse")
        argv += extra_args or []
        if setctty:
            # start_new_session setsid()s and then dup2()s the slave, and a
            # dup2 is not an open: the slave never becomes the session's
            # controlling terminal, so closing the master signals nothing.
            # A terminal hangs up only its controlling session, so this mode
            # makes craze one. A fresh interpreter does it, never a
            # preexec_fn: that callback runs after fork in a process that
            # already has threads (FakeHerdr's server, this class's readers),
            # which CPython documents as a deadlock hazard. execv keeps the
            # pid, so proc.pid is still craze and still its own group.
            argv = [
                sys.executable,
                "-c",
                "import os,fcntl,termios,sys; os.setsid(); "
                "fcntl.ioctl(0, termios.TIOCSCTTY, 0); os.execv(sys.argv[1], sys.argv[1:])",
                *argv,
            ]
        try:
            self.proc = subprocess.Popen(
                argv,
                stdin=slave,
                stdout=slave,
                stderr=slave,
                env=env,
                cwd=str(workspace),
                # The helper does its own setsid, which must come before its
                # ioctl.
                start_new_session=not setctty,
                close_fds=True,
            )
        except Exception:
            os.close(master)
            os.close(slave)
            raise
        os.close(slave)
        self.master = master
        self._master_open = True
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

    def _read_loop(self) -> None:
        while not self._closed.is_set():
            try:
                ready, _, _ = select.select([self.master], [], [], 0.05)
            except OSError:
                break
            if not ready:
                continue
            try:
                chunk = os.read(self.master, 4096)
            except OSError as err:
                if err.errno in (errno.EIO, errno.EBADF):
                    break
                continue
            if not chunk:
                break
            before = len(self.buf)
            self.buf.extend(chunk)
            if self._answers_queries:
                # Back by one query's length less one: a query split across
                # two reads is still found whole.
                self._answer_queries(max(0, before - len(_CPR_QUERY) + 1))

    def _answer_queries(self, start: int) -> None:
        """Answer the background-colour query the way a real terminal does.

        Only a craze that owns its terminal asks: termenv queries a tty only
        when it is the foreground of its controlling one. It writes the OSC 11
        query and then a cursor-position query, reads both answers in that
        order, and waits five seconds for them before carrying on without.
        Unanswered, every setctty run would start five seconds late.
        """
        start = max(start, self._answered_to)
        while (i := self.buf.find(_CPR_QUERY, start)) >= 0:
            reply = _CPR_REPLY
            if self.buf.rfind(_BG_QUERY, self._answered_to, i) >= 0:
                reply = _BG_REPLY + reply
            self._answered_to = start = i + len(_CPR_QUERY)
            try:
                os.write(self.master, reply)
            except OSError:
                # craze is gone, and with it whoever asked.
                return

    def screen(self) -> str:
        return bytes(self.buf).decode("utf-8", "replace")

    def mark(self) -> int:
        """Offset into the accumulated output; pairs with wait_contains_since."""
        return len(self.buf)

    def wait_contains(self, needle: str, timeout: float = 10) -> str:
        return self._wait(needle, 0, timeout)

    def wait_contains_since(self, needle: str, mark: int, timeout: float = 10) -> str:
        """wait_contains over bytes emitted after mark only.

        The screen is accumulated history, so a plain wait_contains can pass
        on bytes that painted long before the action under test. Scoping to
        the mark makes the assertion about output the action caused.
        """
        return self._wait(needle, mark, timeout)

    def _wait(self, needle: str, mark: int, timeout: float) -> str:
        """Match against the output with terminal escapes stripped.

        Adjacent spans carry their own SGR sequences, so a needle that
        crosses a style boundary (`○ explore`: glyph then label) never
        appears contiguously in the raw bytes.
        """
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            last = bytes(self.buf[mark:]).decode("utf-8", "replace")
            if needle in _ANSI.sub("", last):
                return last
            if self.proc.poll() is not None:
                raise AssertionError(
                    f"craze exited {self.proc.returncode} before {needle!r}: {last[-3000:]}"
                )
            time.sleep(0.05)
        raise AssertionError(f"timeout waiting for {needle!r}: {last[-4000:]}")

    def write(self, data: bytes) -> None:
        os.write(self.master, data)

    def wait_exit(self, timeout: float = 5) -> int:
        try:
            return self.proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired as err:
            raise AssertionError(f"craze did not exit: {self.screen()[-3000:]}") from err

    def hangup(self, timeout: float = 5) -> int:
        """Close the terminal under craze, the way a closed tab does, and
        return its exit code.

        Only a setctty craze is hung up by this: anything else has no
        controlling terminal, and closing the master signals nothing. The
        screen is frozen from here on, so nothing after it can be waited for
        on the terminal.
        """
        # The reader stopped first: it selects on the master, and once the fd
        # is closed its number can be reused by anything this process opens.
        self._closed.set()
        self._reader.join(timeout=1)
        self._close_master()
        return self.wait_exit(timeout)

    def _close_master(self) -> None:
        """Close the master exactly once, whichever of hangup and close runs."""
        if not self._master_open:
            return
        self._master_open = False
        try:
            os.close(self.master)
        except OSError:
            pass

    def close(self) -> None:
        self._closed.set()
        if self.proc.poll() is None:
            try:
                os.killpg(self.proc.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                self.proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(self.proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                self.proc.wait(timeout=1)
        self._close_master()

    def __enter__(self) -> PTYCraze:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


# CSI, plus OSC: craze sets the terminal's own colours (OSC 11/10) and tab
# title (OSC 2), and an OSC string left in the text would otherwise survive
# into what wait_contains matches.
_ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")

# termenv's terminal queries and a dark terminal's answers to them.
_BG_QUERY = b"\x1b]11;?"
_CPR_QUERY = b"\x1b[6n"
_BG_REPLY = b"\x1b]11;rgb:0000/0000/0000\x1b\\"
_CPR_REPLY = b"\x1b[1;1R"


def _wait_fake_gone(fake_agent_bin: Path, timeout: float = 3) -> None:
    needle = str(fake_agent_bin)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not _cmdline_has(needle):
            return
        time.sleep(0.05)
    raise AssertionError(f"fake-agent still running: {needle}")


def quit_craze(tui: PTYCraze, timeout: float = 5) -> None:
    """Ctrl+D, then assert a clean exit. Moved here from test_host_status.py
    (issue #23) so test_tui.py's own cases can use it too; it closes over no
    module-level WAIT, so it gets its own default, matching wait_exit's.
    """
    tui.write(b"\x04")
    code = tui.wait_exit(timeout=timeout)
    assert code == 0, tui.screen()[-3000:]


def _wait_output(tui: PTYCraze, needle: str, timeout: float = 5) -> str:
    """wait_contains that expects craze to have exited already.

    Moved here from test_host_status.py (issue #23) for the same reason as
    quit_craze, with its own default timeout rather than that module's WAIT.
    """
    deadline = time.monotonic() + timeout
    text = ""
    while time.monotonic() < deadline:
        text = _ANSI.sub("", tui.screen())
        if needle in text:
            return text
        time.sleep(0.05)
    raise AssertionError(f"timeout waiting for {needle!r}: {text[-3000:]!r}")


def test_tui_echo_and_quit(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor")
        tui.write(b"hello\r")
        tui.wait_contains("echo: hello")
        tui.write(b"\x04")
        code = tui.wait_exit()
        assert code == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)


def test_tui_help_esc_then_quit(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor")
        tui.write(b"/help\r")
        deadline = time.monotonic() + 10
        text = ""
        while time.monotonic() < deadline:
            text = tui.screen()
            if "shift+tab" in text or "/exit" in text:
                break
            if tui.proc.poll() is not None:
                raise AssertionError(
                    f"craze exited {tui.proc.returncode} during /help: {text[-3000:]}"
                )
            time.sleep(0.05)
        else:
            raise AssertionError(f"help overlay missing: {text[-4000:]}")
        tui.write(b"\x1b")
        time.sleep(0.1)
        tui.write(b"\x04")
        code = tui.wait_exit()
        assert code == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)


def test_tui_authfail_exits_nonzero(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """A session that never started must not exit 0.

    craze deliberately stays up and shows the error — you can read it, and a
    login is the fix — but the process has to tell a script that nothing ran.
    """
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path, script="authfail") as tui:
        tui.wait_contains("authentication failed")
        tui.write(b"\x04")
        code = tui.wait_exit()
        assert code != 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)


def test_tui_clean_exit_prints_no_agent_stderr(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """F1/#23: a clean /exit never shows the agent's own stderr.

    The pair is what makes the negative sound: `plugin dir skipped:` (craze's
    own lane, from the missing --plugin-dir) is present, proving post-exit
    bytes survive this harness at all, so the canary's absence is the agent
    lane being discarded on a clean run, not a pty that dropped bytes.
    """
    canary = "CANARY-CLEAN-EXIT"
    with PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        env_extra={"CRAZE_FAKE_STDERR": canary},
        extra_args=["--plugin-dir", "definitely-not-here"],
    ) as tui:
        tui.wait_contains("cursor")
        tui.write(b"hello\r")
        tui.wait_contains("echo: hello")
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)
    text = _ANSI.sub("", tui.screen())
    assert "plugin dir skipped:" in text, text[-3000:]
    assert canary not in text, text[-3000:]


def test_tui_authfail_prints_agent_stderr(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """#23's deterministic start-failure companion to
    test_tui_authfail_exits_nonzero: a session that never started still shows
    the agent's stderr, and exits exactly 1.

    authfail's error lands before the user can press anything (§2.7 fact 6),
    with nothing racing errMsg the way Ctrl+D's requestQuit does elsewhere, so
    unlike TestTUIKeepsTheAgentStderrOffTheTerminal in Go this one can assert
    the exit code too. No `plugin dir skipped:` line is expected:
    discoverPlugins runs only after authenticate, which authfail never
    reaches — the positive control here is the exit code, not a second line.
    """
    canary = "CANARY-AUTHFAIL"
    with PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="authfail",
        env_extra={"CRAZE_FAKE_STDERR": canary},
    ) as tui:
        tui.wait_contains("authentication failed")
        tui.write(b"\x04")
        code = tui.wait_exit()
        assert code == 1, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)
    text = _ANSI.sub("", tui.screen())
    assert canary in text, text[-3000:]
    assert "plugin dir skipped:" not in text, text[-3000:]


def test_tui_agent_death_prints_stderr_and_exits_clean(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """#23 end to end: an agent that dies on its own is not craze failing.

    The stored ErrAgentExited travels session.Close -> finishRun's bool ->
    flush's failed, so the agent's stderr (including the startup canary)
    survives past the exit — and craze still exits 0, because a dead agent is
    not craze's own failure. CRAZE_FAKE_LINGER keeps the fake from dying of
    its own closed stdin on some other path than the SIGTERM this test sends;
    `Working` is long-turn's proof the prompt was read and a turn is genuinely
    open, so the kill lands mid-session rather than racing session/prompt.
    """
    canary = "CANARY-AGENT-DIED"
    with PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        step="30s",
        env_extra={"CRAZE_FAKE_STDERR": canary, "CRAZE_FAKE_LINGER": "1"},
    ) as tui:
        tui.wait_contains("cursor")
        tui.write(b"go the long way\r")
        tui.wait_contains("Working")
        pids = _cmdline_pids(str(fake_agent_bin))
        assert len(pids) == 1, pids
        os.kill(pids[0], signal.SIGTERM)
        tui.wait_contains("error: ")
        tui.write(b"\x04")
        code = tui.wait_exit()
        assert code == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)
    text = _ANSI.sub("", tui.screen())
    assert canary in text, text[-3000:]


def test_tui_subagent_view_enter_esc(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """Enter opens a sub-agent in the main area; Esc returns; nothing cancels.

    The banner only paints while the view holds the main area. Esc's return
    is proven by main-transcript bytes re-emitted after a mark taken inside
    the view: the screen is accumulated history, so only output the return
    itself caused counts.
    """
    with PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, script="grok-subagent", provider="grok"
    ) as tui:
        tui.wait_contains("grok")
        tui.write(b"go\r")
        tui.wait_contains("○ explore")
        tui.write(b"\x1b[B\r")
        tui.wait_contains("esc to return")
        mark = tui.mark()
        tui.write(b"\x1b")
        tui.wait_contains_since("DONE: main.py README.md", mark)
        tui.write(b"\x04")
        code = tui.wait_exit()
        assert code == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)


def _raw_since(tui: PTYCraze, mark: int) -> str:
    return bytes(tui.buf[mark:]).decode("utf-8", "replace")


def _assert_mode_switch(raw: str, enable: str) -> None:
    """Both disables, then the enable, in that order.

    bubbletea's DisableMouse writes 1002l then 1003l; neither escape disables
    the other, so a switch that skipped one would leave the previous mode on
    and the assertion has to be about the order, not about presence.
    """
    cell_off = raw.find("\x1b[?1002l")
    all_off = raw.find("\x1b[?1003l")
    on = raw.find(enable)
    assert cell_off >= 0, f"no 1002l before {enable!r}: {raw[-400:]!r}"
    assert all_off >= 0, f"no 1003l before {enable!r}: {raw[-400:]!r}"
    assert on >= 0, f"no {enable!r}: {raw[-400:]!r}"
    assert cell_off < on and all_off < on, (
        f"the disables must precede {enable!r}: "
        f"1002l@{cell_off} 1003l@{all_off} enable@{on}"
    )


def _wait_raw(tui: PTYCraze, needle: str, mark: int, timeout: float = 10) -> str:
    """wait for a literal escape sequence, without stripping escapes."""
    deadline = time.monotonic() + timeout
    last = ""
    while time.monotonic() < deadline:
        last = _raw_since(tui, mark)
        if needle in last:
            return last
        if tui.proc.poll() is not None:
            raise AssertionError(f"craze exited {tui.proc.returncode}: {last[-2000:]}")
        time.sleep(0.05)
    raise AssertionError(f"timeout waiting for {needle!r}: {last[-2000:]!r}")


def test_tui_queue_then_drain(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Enter during a running turn queues; the queue drains one per turn.

    The first turn is slow enough to type into and the ones behind it finish
    at once, so the drain is observable without the test waiting on a clock.
    """
    with PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, script="long-turn", step="2s,1ms"
    ) as tui:
        tui.wait_contains("cursor")
        tui.write(b"go the long way\r")
        tui.wait_contains("Working")
        tui.write(b"Reply with PINEAPPLE\r")
        tui.wait_contains("#1 Reply with PINEAPPLE")
        tui.write(b"Reply with MANGO\r")
        tui.wait_contains("#2 Reply with MANGO")
        tui.wait_contains("⧗ 2 queued")
        # Both are sent, one per settled turn, and the session comes back to
        # idle with the band empty.
        #
        # The *order* is not this test's to hold: both turns finish inside a
        # few milliseconds here, so a mark-scoped wait cannot separate them.
        # internal/tui's queue-drain golden and the `--json` follow-up test
        # own the ordering; this one owns "it works in a real terminal".
        mark = tui.mark()
        tui.wait_contains_since("❯ Reply with PINEAPPLE", mark, timeout=30)
        tui.wait_contains_since("❯ Reply with MANGO", mark, timeout=30)
        tui.wait_contains_since("message  / for commands", mark, timeout=30)
        screen = _ANSI.sub("", tui.screen())
        assert "⧗ " not in screen.split("❯ Reply with MANGO")[-1], screen[-1500:]
        tui.write(b"\x04")
        assert tui.wait_exit() == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)


def test_tui_queue_switches_the_terminal_to_all_motion(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """Hover needs motion reports with no button down.

    1002 (cell motion) does not send them and 1003 (all motion) does, and
    neither escape disables the other — so every transition goes through
    1000l/1002l/1003l first. The terminal is where that is visible.
    """
    with PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, script="long-turn", step="30s"
    ) as tui:
        tui.wait_contains("cursor")
        tui.write(b"go the long way\r")
        tui.wait_contains("Working")
        mark = tui.mark()
        tui.write(b"Reply with PINEAPPLE\r")
        tui.wait_contains_since("#1 Reply with PINEAPPLE", mark)
        got = _wait_raw(tui, "\x1b[?1003h", mark)
        _assert_mode_switch(got, "\x1b[?1003h")

        # The last row leaving puts cell motion back, the same way round.
        mark = tui.mark()
        tui.write(b"\x1b[A")  # ↑ selects the row
        tui.wait_contains_since("❯ #1 Reply with PINEAPPLE", mark)
        mark = tui.mark()
        tui.write(b"\x7f")  # backspace cancels it
        got = _wait_raw(tui, "\x1b[?1002h", mark)
        _assert_mode_switch(got, "\x1b[?1002h")
        tui.write(b"\x03\x03")
        tui.wait_exit()
    _wait_fake_gone(fake_agent_bin)


def test_tui_no_mouse_never_changes_the_motion_mode(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """--no-mouse asked the terminal for no reporting at all.

    The queue must not turn any of it on behind the flag's back.
    """
    with PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        step="30s",
        no_mouse=True,
    ) as tui:
        tui.wait_contains("cursor")
        tui.write(b"go the long way\r")
        tui.wait_contains("Working")
        mark = tui.mark()
        tui.write(b"Reply with PINEAPPLE\r")
        tui.wait_contains_since("#1 Reply with PINEAPPLE", mark)
        time.sleep(0.3)
        raw = _raw_since(tui, mark)
        for seq in ("\x1b[?1003h", "\x1b[?1002h", "\x1b[?1003l"):
            assert seq not in raw, repr(raw[-400:])
        tui.write(b"\x03\x03")
        tui.wait_exit()
    _wait_fake_gone(fake_agent_bin)


def _seed_index(home: Path, workspace: Path, session_id: str, provider: str, title: str) -> Path:
    """Write one session-index row, the way a previous craze run would have.

    The index is the config file's sibling, so HOME is what decides where it
    lands — the same HOME the TUI under test runs with.
    """
    stamp = (
        datetime.now(timezone.utc)
        .replace(tzinfo=None)
        .isoformat(timespec="microseconds")
        + "Z"
    )
    row = {
        "sessionId": session_id,
        "provider": provider,
        "cwd": str(workspace),
        "title": title,
        "pinned": False,
        "createdAt": stamp,
        "updatedAt": stamp,
    }
    craze_dir = home / ".craze"
    craze_dir.mkdir(parents=True, exist_ok=True)
    index = craze_dir / "sessions.jsonl"
    index.write_text(json.dumps(row) + "\n", encoding="utf-8")
    return index


def test_tui_continue_with_no_index_exits_1(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """Nothing to continue is exit 1 and a message, not an empty craze.

    It runs on a pty because the non-tty refusal (exit 2) comes first, so this
    is the only way to see the code the flag itself returns.
    """
    with PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, extra_args=["--continue"]
    ) as tui:
        code = tui.wait_exit(timeout=10)
        assert code == 1, tui.screen()[-3000:]
        assert "no session to continue in" in _ANSI.sub("", tui.screen())


def test_tui_continue_and_resume_exit_2(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """Asking for both is a usage error, and usage errors are exit 2."""
    with PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, extra_args=["--continue", "--resume"]
    ) as tui:
        code = tui.wait_exit(timeout=10)
        assert code == 2, tui.screen()[-3000:]
        assert "mutually exclusive" in _ANSI.sub("", tui.screen())


_OSC_SET_TOKYO = "\x1b]11;rgb:1a/1b/26\x07\x1b]10;rgb:a9/b1/d6\x07"
_OSC_RESET = "\x1b]111\x07\x1b]110\x07"


def _wait_raw_endswith(tui: PTYCraze, suffix: str, timeout: float = 5) -> str:
    """wait for the raw output to *end* with a sequence.

    Deliberately not _wait_raw: that one takes a mark and matches anywhere,
    and it treats an exited process as a failure. The reset pair is written on
    the way out, after bubbletea closes the alt screen, so the process having
    exited is the normal case here -- and it has to be the *last* bytes on the
    wire, which is the ordering this asserts.
    """
    deadline = time.monotonic() + timeout
    raw = ""
    while time.monotonic() < deadline:
        raw = tui.screen()
        if raw.endswith(suffix):
            return raw
        time.sleep(0.05)
    raise AssertionError(f"output does not end with {suffix!r}: {raw[-400:]!r}")


def test_tui_themes_the_terminal_colours_and_resets_them(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The themed background is a property of the terminal, not of a frame.

    craze sets the terminal's own default background and foreground (OSC 11
    and 10) from the theme before the first frame and hands them back (OSC 111
    and 110) after bubbletea has restored everything else, so a pty is the
    only place the whole lifecycle is visible. Every off switch — the flag,
    `background = false`, and a profile with no colour at all — has to leave
    all four of those sequences unwritten, not merely balance them.
    """
    workspace = tmp_path / "on"
    workspace.mkdir()
    with PTYCraze(craze_bin, fake_agent_bin, workspace) as tui:
        tui.wait_contains("cursor")
        raw = tui.screen()
        assert _OSC_SET_TOKYO in raw, repr(raw[:400])
        # Before the first frame: the pair precedes the alt screen bubbletea
        # opens as its very first act.
        assert "\x1b[?1049h" in raw, repr(raw[:400])
        assert raw.index(_OSC_SET_TOKYO) < raw.index("\x1b[?1049h"), repr(raw[:400])

        os.killpg(tui.proc.pid, signal.SIGTERM)
        tui.wait_exit()
        # Last thing out, after bubbletea's own restore and the title clear.
        _wait_raw_endswith(tui, _OSC_RESET)
    _wait_fake_gone(fake_agent_bin)

    for name, extra_args, config, env_extra in (
        ("off-flag", ["--no-background"], None, None),
        ("off-config", None, "background = false\n", None),
        # NO_COLOR drops the profile to Ascii: craze paints no SGR colour, so
        # it must not repaint the terminal either.
        ("off-no-color", None, None, {"NO_COLOR": "1"}),
    ):
        workspace = tmp_path / name
        workspace.mkdir()
        if config is not None:
            path = workspace / ".craze" / "config.toml"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(config, encoding="utf-8")
        with PTYCraze(
            craze_bin,
            fake_agent_bin,
            workspace,
            extra_args=extra_args,
            env_extra=env_extra,
        ) as tui:
            tui.wait_contains("cursor")
            os.killpg(tui.proc.pid, signal.SIGTERM)
            tui.wait_exit()
            time.sleep(0.2)
            raw = tui.screen()
            # None of OSC 10, 11, 110 or 111 — they all start "\x1b]1".
            assert "\x1b]1" not in raw, f"{name}: {raw[:400]!r} … {raw[-400:]!r}"
        _wait_fake_gone(fake_agent_bin)


def test_tui_continue_replays_and_leaves_the_config_alone(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """--continue restores the transcript and never rewrites the default provider.

    The config file says grok and the row says cursor: the row wins (the index
    decides which agent can load a session), the provider in config.toml is
    not filtered against and not overwritten, and the transcript comes back
    with the `restored` note that closes a replay.
    """
    config = tmp_path / ".craze" / "config.toml"
    config.parent.mkdir(parents=True, exist_ok=True)
    before = 'provider = "grok"\ntheme = "tokyo-night"\n'
    config.write_text(before, encoding="utf-8")
    _seed_index(tmp_path, tmp_path, "sess-load-1", "cursor", "yesterday's thread")

    with PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="load",
        provider="",
        extra_args=["--continue"],
    ) as tui:
        tui.wait_contains("the workspace holds main.py and README.md")
        tui.wait_contains("restored")
        # The stored title is on the composer rule, seeded before Start.
        tui.wait_contains("yesterday's thread")
        tui.write(b"\x04")
        assert tui.wait_exit() == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)

    assert config.read_text(encoding="utf-8") == before


def test_tui_native_one_turn_leaves_config_and_index_alone(
    craze_bin: Path, tmp_path: Path
) -> None:
    """Plan 018 C11: a native turn in the real TUI never persists.

    The provider is hidden (§3.4): the status bar shows it (it still has to
    be usable), but startedMsg's SaveProvider skips it, so the config file's
    provider is exactly what it was before, and no sessions.jsonl is created
    at all -- writeIndex skips a hidden provider's snapshot too. No fake
    agent is spawned here (`--agent-bin` and an in-process provider are a
    usage error, refuseInProcess), so the only server on the other end of
    this turn is the loopback SSE fixture standing in for a real provider.
    """
    craze_home = tmp_path / "craze-home"
    craze_home.mkdir()
    config_path = craze_home / "config.toml"
    before = 'provider = "grok"\ntheme = "tokyo-night"\n'
    config_path.write_text(before, encoding="utf-8")

    workspace = tmp_path / "ws"
    workspace.mkdir()

    with SSEFixture() as fixture:
        fixture.set_ok(text_parts=["native fixture reply"])
        write_native_config(craze_home / "native", fixture.base_url)

        with PTYCraze(
            craze_bin,
            None,
            workspace,
            provider="native",
            env_extra={"CRAZE_HOME": str(craze_home)},
        ) as tui:
            tui.wait_contains("native")
            tui.write(b"hi\r")
            tui.wait_contains("native fixture reply")
            quit_craze(tui)
        # The configured key really went out, so its absence below means
        # something.
        assert any(CANARY in r.authorization for r in fixture.requests), fixture.requests
        text = _ANSI.sub("", tui.screen())
        assert CANARY not in text, text[-3000:]

    assert config_path.read_text(encoding="utf-8") == before
    assert not (craze_home / "sessions.jsonl").exists()


def test_tui_continue_prefers_the_cursor_row_over_a_native_default(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """§3.4's exception: --continue starts the index row's own provider, not
    the resolved default, so a hidden default never refuses --agent-bin (or
    stops the load) for a session that is really cursor's.

    CRAZE_PROVIDER=native stands in for "whatever the environment or config
    resolved" (§3.4's "a native default from env or config"); runTUI skips
    refuseInProcess entirely under --continue/--resume (internal/cli/tui.go),
    and resolveLoad hands the seeded cursor row to LoadSession instead of the
    resolved provider, so the fake agent still runs and still gets
    session/load for that id -- proof cursor, not native, is what actually
    started.
    """
    _seed_index(tmp_path, tmp_path, "sess-load-1", "cursor", "yesterday's thread")

    with PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="load",
        provider="",
        extra_args=["--continue"],
        env_extra={"CRAZE_PROVIDER": "native"},
    ) as tui:
        tui.wait_contains("the workspace holds main.py and README.md")
        tui.wait_contains("restored")
        tui.wait_contains("yesterday's thread")
        tui.write(b"\x04")
        assert tui.wait_exit() == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)
