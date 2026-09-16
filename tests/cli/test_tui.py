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


def _open_pty() -> tuple[int, int]:
    try:
        return pty.openpty()
    except OSError as err:
        pytest.skip(f"no pty: {err}")


def _set_winsize(fd: int, rows: int = 24, cols: int = 80) -> None:
    packed = struct.pack("HHHH", rows, cols, 0, 0)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, packed)


def _cmdline_has(needle: str) -> bool:
    encoded = needle.encode()
    if sys.platform != "linux":
        # Only Linux has /proc; everywhere else ps is the one portable view of
        # another process's argv. check=True on purpose: every caller reads a
        # False as proof the child is gone, so a ps that failed must raise
        # rather than quietly turn a leak check green.
        out = subprocess.run(
            ["ps", "-axww", "-o", "args="],
            capture_output=True,
            check=True,
        ).stdout
        return encoded in out
    try:
        entries = Path("/proc").glob("[0-9]*/cmdline")
    except OSError:
        return False
    for path in entries:
        try:
            data = path.read_bytes()
        except OSError:
            continue
        if encoded in data:
            return True
    return False


class PTYCraze:
    def __init__(
        self,
        craze_bin: Path,
        fake_agent_bin: Path,
        workspace: Path,
        script: str = "echo",
        provider: str = "cursor",
        no_mouse: bool = False,
        step: str | None = None,
        extra_args: list[str] | None = None,
    ) -> None:
        self.fake_agent_bin = fake_agent_bin
        self.buf = bytearray()
        self._closed = threading.Event()
        master, slave = _open_pty()
        _set_winsize(slave)
        _set_winsize(master)
        env = os.environ.copy()
        env["TERM"] = "xterm-256color"
        env["CRAZE_FAKE_SCRIPT"] = script
        env["HOME"] = str(workspace)
        env.pop("CRAZE_AGENT_BIN", None)
        env.pop("CRAZE_PROVIDER", None)
        env.pop("CRAZE_CONFIG", None)
        # Never inherited: a developer with CRAZE_FAKE_STEP exported would
        # otherwise change the timing of every case that did not ask for it.
        env.pop("CRAZE_FAKE_STEP", None)
        if step:
            env["CRAZE_FAKE_STEP"] = step
        argv = [str(craze_bin)]
        if provider:
            # An empty provider leaves the flag off entirely, which is the
            # only way to exercise what an *implicit* provider does: an
            # explicit --provider filters the session index and is still
            # allowed to write the persisted default.
            argv += ["--provider", provider]
        argv += [
            "--agent-bin",
            str(fake_agent_bin),
            "--workspace",
            str(workspace),
            "--theme",
            "tokyo-night",
        ]
        if no_mouse:
            argv.append("--no-mouse")
        argv += extra_args or []
        try:
            self.proc = subprocess.Popen(
                argv,
                stdin=slave,
                stdout=slave,
                stderr=slave,
                env=env,
                cwd=str(workspace),
                start_new_session=True,
                close_fds=True,
            )
        except Exception:
            os.close(master)
            os.close(slave)
            raise
        os.close(slave)
        self.master = master
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
            self.buf.extend(chunk)

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
        try:
            os.close(self.master)
        except OSError:
            pass

    def __enter__(self) -> PTYCraze:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


_ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")


def _wait_fake_gone(fake_agent_bin: Path, timeout: float = 3) -> None:
    needle = str(fake_agent_bin)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not _cmdline_has(needle):
            return
        time.sleep(0.05)
    raise AssertionError(f"fake-agent still running: {needle}")


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
