"""craze reporting its status to herdr (plan 015 §3.3, §3.7).

A thread serves a unix socket the way herdr's control socket answers: one
newline-delimited JSON request per connection, one reply line. The tests point
craze's herdr variables at that socket -- never at a real herdr; conftest strips
every inherited HERDR_*/ROOST_* variable first -- and assert on the decoded
lines in the order they arrived.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import socket
import tempfile
import threading
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest
from test_tui import _ANSI, PTYCraze, _wait_fake_gone

PANE = "w9:p42"
SOURCE = "custom:craze"
AGENT = "craze"

# Generous: the macOS CI leg is slow, and every wait below ends as soon as its
# condition holds.
WAIT = 20.0
LONG_WAIT = 45.0


class FakeHerdr:
    """A herdr control socket that records every request and answers ok.

    Connections are served one at a time, which is how craze's herdr reporter
    sends -- one worker, one dial per request, each waiting for its reply -- so
    the recorded order is the order craze wrote. The directory comes from
    mkdtemp under /tmp because a unix socket path is capped at 104 bytes on
    macOS, and pytest's tmp_path there is longer than that.
    """

    def __init__(self) -> None:
        self.dir = tempfile.mkdtemp(prefix="h", dir="/tmp")
        self.path = os.path.join(self.dir, "s")
        assert len(self.path) < 100, self.path
        self.lines: list[dict[str, Any]] = []
        # The monotonic arrival time and the raw bytes of each request,
        # parallel to lines.
        self.arrivals: list[float] = []
        self.raw: list[bytes] = []
        self.connections = 0
        self._cond = threading.Condition()
        self._stop = threading.Event()
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.bind(self.path)
        self._sock.listen(16)
        self._sock.settimeout(0.05)
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()

    def env(self, **overrides: str | None) -> dict[str, str]:
        """herdr's gate, met against this socket; None drops a variable."""
        env: dict[str, str | None] = {
            "HERDR_ENV": "1",
            "HERDR_SOCKET_PATH": self.path,
            "HERDR_PANE_ID": PANE,
        }
        env.update(overrides)
        return {k: v for k, v in env.items() if v is not None}

    def _serve(self) -> None:
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except TimeoutError:
                continue
            except OSError:
                return
            with self._cond:
                self.connections += 1
                self._cond.notify_all()
            with conn:
                conn.settimeout(10)
                self._handle(conn)

    def _handle(self, conn: socket.socket) -> None:
        data = b""
        try:
            while b"\n" not in data:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                data += chunk
        except OSError:
            return
        if not data:
            return
        try:
            obj = json.loads(data.split(b"\n", 1)[0])
        except ValueError:
            # Recorded, so the assertion that trips on it shows the bytes.
            obj = {"method": "<undecodable>", "raw": data.decode("utf-8", "replace")}
        with self._cond:
            self.raw.append(data)
            self.lines.append(obj)
            self.arrivals.append(time.monotonic())
            self._cond.notify_all()
        reply = {"id": obj.get("id"), "result": {"type": "ok"}}
        try:
            conn.sendall(json.dumps(reply).encode() + b"\n")
        except OSError:
            pass

    def wait_for(
        self, pred: Callable[[list[dict[str, Any]]], bool], what: str, timeout: float = WAIT
    ) -> list[dict[str, Any]]:
        with self._cond:
            if not self._cond.wait_for(lambda: pred(self.lines), timeout):
                raise AssertionError(f"timeout waiting for {what}: {self.lines!r}")
            return list(self.lines)

    def snapshot(self) -> list[dict[str, Any]]:
        with self._cond:
            return list(self.lines)

    def close(self) -> None:
        self._stop.set()
        self._sock.close()
        self._thread.join(timeout=5)
        shutil.rmtree(self.dir, ignore_errors=True)

    def __enter__(self) -> FakeHerdr:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def states(lines: list[dict[str, Any]]) -> list[str]:
    return [ln["params"]["state"] for ln in lines if ln["method"] == "pane.report_agent"]


def _has_ready_idle(lines: list[dict[str, Any]]) -> bool:
    return states(lines)[:1] == ["idle"]


def _has_ready_metadata(lines: list[dict[str, Any]]) -> bool:
    """The ready idle and the metadata line that follows it have both arrived."""
    return [ln["method"] for ln in lines[:2]] == ["pane.report_agent", "pane.report_metadata"]


def report(state: str, seq: int, message: str | None = None) -> dict[str, Any]:
    params: dict[str, Any] = {
        "pane_id": PANE,
        "source": SOURCE,
        "agent": AGENT,
        "state": state,
        "seq": seq,
    }
    if message is not None:
        params["message"] = message
    return {"id": f"craze:{seq}", "method": "pane.report_agent", "params": params}


def metadata(seq: int, provider: str | None, model: str | None) -> dict[str, Any]:
    return {
        "id": f"craze:{seq}:metadata",
        "method": "pane.report_metadata",
        "params": {
            "pane_id": PANE,
            "source": SOURCE,
            "agent": AGENT,
            "tokens": {"provider": provider, "model": model},
        },
    }


def release(seq: int) -> dict[str, Any]:
    return {
        "id": f"craze:{seq}",
        "method": "pane.release_agent",
        "params": {"pane_id": PANE, "source": SOURCE, "agent": AGENT, "seq": seq},
    }


def seq_of(line: dict[str, Any]) -> int:
    seq = line["params"]["seq"]
    assert isinstance(seq, int), line
    return seq


def assert_framing(herdr: FakeHerdr) -> None:
    """One JSON object per connection, ending in exactly one newline."""
    for raw in herdr.raw:
        assert raw.endswith(b"\n") and raw.count(b"\n") == 1, raw


def quit_craze(tui: PTYCraze) -> None:
    tui.write(b"\x04")
    code = tui.wait_exit(timeout=WAIT)
    assert code == 0, tui.screen()[-3000:]


def test_herdr_echo_turn_sequence(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """H2: a plain turn is exactly idle, metadata, working, idle, nulls, release.

    The ready idle is waited for before the prompt, or the prompt's working
    would supersede it inside the hub's idle debounce.
    """
    with FakeHerdr() as herdr, PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, env_extra=herdr.env()
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        herdr.wait_for(_has_ready_idle, "the ready idle")
        tui.write(b"hello\r")
        tui.wait_contains("echo: hello", timeout=WAIT)
        herdr.wait_for(lambda ls: states(ls) == ["idle", "working", "idle"], "the turn's idle")
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    lines = herdr.snapshot()
    assert [ln["method"] for ln in lines] == [
        "pane.report_agent",
        "pane.report_metadata",
        "pane.report_agent",
        "pane.report_agent",
        "pane.report_metadata",
        "pane.release_agent",
    ], lines
    ready, working, idle, rel = (seq_of(lines[i]) for i in (0, 2, 3, 5))
    assert ready < working < idle < rel, lines
    assert lines == [
        report("idle", ready),
        metadata(ready, "cursor", "default"),
        report("working", working),
        report("idle", idle),
        metadata(rel, None, None),
        release(rel),
    ]
    assert_framing(herdr)


def test_herdr_permission_card_blocks(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """A permission card is blocked, with the card's header as the message."""
    with FakeHerdr() as herdr, PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="permission",
        extra_args=["--no-force"],
        env_extra=herdr.env(),
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        herdr.wait_for(_has_ready_idle, "the ready idle")
        tui.write(b"run it\r")
        tui.wait_contains("[a]llow", timeout=WAIT)
        herdr.wait_for(lambda ls: "blocked" in states(ls), "blocked")
        tui.write(b"a")
        tui.wait_contains("decision:opt-once", timeout=WAIT)
        herdr.wait_for(
            lambda ls: states(ls) == ["idle", "working", "blocked", "working", "idle"],
            "working then idle after the answer",
        )
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    reports = [ln for ln in herdr.snapshot() if ln["method"] == "pane.report_agent"]
    blocked = reports[2]
    message = blocked["params"].get("message")
    assert isinstance(message, str) and message.startswith("permission "), blocked
    assert "\n" not in message, blocked
    assert blocked == report("blocked", seq_of(blocked), message)
    for ln in reports[:2] + reports[3:]:
        assert "message" not in ln["params"], ln


def test_herdr_queue_drain_has_no_idle_between_turns(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """D5: a queued message drained after a turn never reaches herdr as idle.

    The second turn is a second long, so an idle published between the two
    would outlive the hub's 250 ms debounce and land on the socket.
    """
    with FakeHerdr() as herdr, PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        step="2s,500ms",
        env_extra=herdr.env(),
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        herdr.wait_for(_has_ready_idle, "the ready idle")
        tui.write(b"go the long way\r")
        tui.wait_contains("Working", timeout=WAIT)
        tui.write(b"Reply with MANGO\r")
        tui.wait_contains("#1 Reply with MANGO", timeout=WAIT)
        mark = tui.mark()
        tui.wait_contains_since("❯ Reply with MANGO", mark, timeout=LONG_WAIT)
        drained = time.monotonic()
        # The idle that ends the second turn: it arrives after the drained
        # prompt was on screen.
        herdr.wait_for(
            lambda ls: _last_state_after(herdr, ls, "idle", drained),
            "the idle after the drained turn",
            timeout=LONG_WAIT,
        )
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    assert states(herdr.snapshot()) == ["idle", "working", "idle"], herdr.snapshot()


def _last_state_after(
    herdr: FakeHerdr, lines: list[dict[str, Any]], state: str, after: float
) -> bool:
    """The newest state line is state, and it arrived after the given moment.

    Called under the server's lock, from wait_for, so lines and arrivals agree.
    """
    for i in range(len(lines) - 1, -1, -1):
        if lines[i]["method"] == "pane.report_agent":
            return lines[i]["params"]["state"] == state and herdr.arrivals[i] > after
    return False


def test_herdr_sigterm_still_releases(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """H6: SIGTERM is an exit like any other, and herdr is released."""
    with FakeHerdr() as herdr:
        with PTYCraze(craze_bin, fake_agent_bin, tmp_path, env_extra=herdr.env()) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            # The metadata line too: a SIGTERM between the state line and it
            # would rightly send no nulls, and this case is about the release.
            herdr.wait_for(_has_ready_metadata, "the ready idle and its metadata")
            tui.close()
            # close() escalates to SIGKILL after 2 s; a SIGTERM exit is not that.
            assert tui.proc.returncode != -9, tui.screen()[-3000:]
        _wait_fake_gone(fake_agent_bin)
        lines = herdr.snapshot()

    assert [ln["method"] for ln in lines] == [
        "pane.report_agent",
        "pane.report_metadata",
        "pane.report_metadata",
        "pane.release_agent",
    ], lines
    ready, rel = seq_of(lines[0]), seq_of(lines[3])
    assert rel > ready, lines
    assert lines[2:] == [metadata(rel, None, None), release(rel)]


@pytest.mark.parametrize(
    "case",
    [
        "--no-host-status",
        "host_status = false",
        "HERDR_ENV unset",
        "HERDR_SOCKET_PATH unset",
        "HERDR_PANE_ID unset",
    ],
)
def test_herdr_off_switches_send_nothing(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, case: str
) -> None:
    """H3: every off switch leaves the socket untouched through a whole turn.

    The socket is checked after craze has exited, which is after the release a
    reporting craze would have sent, so "nothing" is the whole run's answer.
    """
    with FakeHerdr() as herdr:
        extra_args: list[str] = []
        env = herdr.env()
        if case == "--no-host-status":
            extra_args = ["--no-host-status"]
        elif case == "host_status = false":
            config = tmp_path / "host-off.toml"
            config.write_text("host_status = false\n", encoding="utf-8")
            env["CRAZE_CONFIG"] = str(config)
        else:
            env = herdr.env(**{case.split()[0]: None})
        with PTYCraze(
            craze_bin, fake_agent_bin, tmp_path, extra_args=extra_args, env_extra=env
        ) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            tui.write(b"hello\r")
            tui.wait_contains("echo: hello", timeout=WAIT)
            quit_craze(tui)
        _wait_fake_gone(fake_agent_bin)
        assert herdr.connections == 0 and herdr.snapshot() == [], herdr.snapshot()


def _wait_output(tui: PTYCraze, needle: str, timeout: float = WAIT) -> str:
    """wait_contains that expects craze to have exited already."""
    deadline = time.monotonic() + timeout
    text = ""
    while time.monotonic() < deadline:
        text = _ANSI.sub("", tui.screen())
        if needle in text:
            return text
        time.sleep(0.05)
    raise AssertionError(f"timeout waiting for {needle!r}: {text[-3000:]!r}")


def test_herdr_unreachable_socket_warns_once_after_exit(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """H4: a socket nobody listens on costs one stderr line, after the TUI."""
    nowhere = tempfile.mkdtemp(prefix="h", dir="/tmp")
    try:
        env = {
            "HERDR_ENV": "1",
            "HERDR_SOCKET_PATH": os.path.join(nowhere, "s"),
            "HERDR_PANE_ID": PANE,
        }
        with PTYCraze(craze_bin, fake_agent_bin, tmp_path, env_extra=env) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            tui.write(b"hello\r")
            tui.wait_contains("echo: hello", timeout=WAIT)
            # The failed sends so far are held, not drawn over the frame.
            assert "host status" not in _ANSI.sub("", tui.screen())
            quit_craze(tui)
            _wait_output(tui, "host status: herdr: ")
            # The reader stops at the pty's end of file, so the count below is
            # over everything craze wrote.
            tui._reader.join(timeout=WAIT)
            text = _ANSI.sub("", tui.screen())
        _wait_fake_gone(fake_agent_bin)
    finally:
        shutil.rmtree(nowhere, ignore_errors=True)
    warnings = [ln for ln in text.splitlines() if "host status: herdr:" in ln]
    assert len(warnings) == 1, warnings


_ENVSET = re.compile(r"envset: ([A-Z_ ]+?) :end")


def _child_env_names(tui: PTYCraze) -> set[str]:
    tui.write(b"which\r")
    text = tui.wait_contains("envset:", timeout=WAIT)
    tui.wait_contains(":end", timeout=WAIT)
    match = _ENVSET.search(_ANSI.sub("", tui.screen()))
    assert match, _ANSI.sub("", text)[-2000:]
    names = match.group(1).split()
    return set() if names == ["none"] else set(names)


def test_herdr_child_env_loses_the_hook_gate(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """H5: while herdr is reported to, the agent child has no HERDR_ENV.

    It keeps HERDR_PANE_ID, so the herdr CLI still resolves the pane. With
    --no-host-status the child's environment is craze's own, whole, and the
    socket hears nothing.
    """
    with FakeHerdr() as herdr:
        with PTYCraze(
            craze_bin, fake_agent_bin, tmp_path, script="env", env_extra=herdr.env()
        ) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            herdr.wait_for(_has_ready_idle, "the ready idle")
            assert _child_env_names(tui) == {"HERDR_PANE_ID"}
            quit_craze(tui)
        _wait_fake_gone(fake_agent_bin)
        assert herdr.snapshot(), "the herdr gate was met, so craze reported"

    with FakeHerdr() as herdr:
        with PTYCraze(
            craze_bin,
            fake_agent_bin,
            tmp_path,
            script="env",
            extra_args=["--no-host-status"],
            env_extra=herdr.env(),
        ) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            assert _child_env_names(tui) == {"HERDR_ENV", "HERDR_PANE_ID"}
            quit_craze(tui)
        _wait_fake_gone(fake_agent_bin)
        assert herdr.connections == 0 and herdr.snapshot() == [], herdr.snapshot()
