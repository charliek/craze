from __future__ import annotations

import errno
import fcntl
import os
import pty
import select
import signal
import struct
import subprocess
import termios
import threading
import time
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
    def __init__(self, craze_bin: Path, fake_agent_bin: Path, workspace: Path) -> None:
        self.fake_agent_bin = fake_agent_bin
        self.buf = bytearray()
        self._closed = threading.Event()
        master, slave = _open_pty()
        _set_winsize(slave)
        _set_winsize(master)
        env = os.environ.copy()
        env["TERM"] = "xterm-256color"
        env["CRAZE_FAKE_SCRIPT"] = "echo"
        env.pop("CRAZE_AGENT_BIN", None)
        try:
            self.proc = subprocess.Popen(
                [
                    str(craze_bin),
                    "--agent-bin",
                    str(fake_agent_bin),
                    "--workspace",
                    str(workspace),
                ],
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

    def wait_contains(self, needle: str, timeout: float = 10) -> str:
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            last = self.screen()
            if needle in last:
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
        tui.wait_contains("idle")
        tui.write(b"hello\r")
        tui.wait_contains("echo: hello")
        tui.write(b"q")
        code = tui.wait_exit()
        assert code == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)


def test_tui_help_esc_then_quit(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("idle")
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
        tui.write(b"q")
        code = tui.wait_exit()
        assert code == 0, tui.screen()[-3000:]
    _wait_fake_gone(fake_agent_bin)
