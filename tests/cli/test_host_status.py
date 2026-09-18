"""craze reporting its status to herdr and roost (plan 015 §3.3, §3.4, §3.7).

A thread serves a unix socket the way each host's control socket answers: one
newline-delimited JSON request per connection, one reply line. The tests point
craze's host variables at that socket -- never at a real herdr or roost;
conftest strips every inherited HERDR_*/ROOST_* variable first -- and assert on
the decoded lines in the order they arrived.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any, Self

import pytest
from test_tui import _ANSI, PTYCraze, _wait_fake_gone

PANE = "w9:p42"
SOURCE = "custom:craze"
AGENT = "craze"

TAB = "7"
# The fake agent's one session id: roost's ownership is (source, session id).
FAKE_SESSION = "fake-session-1"

# Generous: the macOS CI leg is slow, and every wait below ends as soon as its
# condition holds.
WAIT = 20.0
LONG_WAIT = 45.0


class _FakeHostSocket:
    """A host control socket that records every request and answers it.

    Connections are served one at a time, which is how craze's reporters send
    -- one worker per host, one dial per request, each waiting for its reply --
    so the recorded order is the order craze wrote. The directory comes from
    mkdtemp under /tmp because a unix socket path is capped at 104 bytes on
    macOS, and pytest's tmp_path there is longer than that. A subclass says
    what the host's variables are and what it answers.
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

    def reply(self, request: dict[str, Any]) -> dict[str, Any]:
        raise NotImplementedError

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
        reply = self.reply(obj)
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

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


class FakeHerdr(_FakeHostSocket):
    """herdr's control socket: every request is answered ok."""

    def env(self, **overrides: str | None) -> dict[str, str]:
        """herdr's gate, met against this socket; None drops a variable."""
        env: dict[str, str | None] = {
            "HERDR_ENV": "1",
            "HERDR_SOCKET_PATH": self.path,
            "HERDR_PANE_ID": PANE,
        }
        env.update(overrides)
        return {k: v for k, v in env.items() if v is not None}

    def reply(self, request: dict[str, Any]) -> dict[str, Any]:
        return {"id": request.get("id"), "result": {"type": "ok"}}


class FakeRoost(_FakeHostSocket):
    """roost's tab socket: every tab.agent_report is accepted.

    The reply is roost's shape (tests/ipc-vectors/tab.agent_report.response.json
    in roost), with the ownership the report asked for echoed back.
    """

    def env(self) -> dict[str, str]:
        """What a roost tab's shell has: the gate, met against this socket,
        and the hook command variable craze must keep from its agent."""
        return {
            "ROOST_SOCKET": self.path,
            "ROOST_TAB_ID": TAB,
            "ROOST_AGENT_HOOK": "/nonexistent/roostctl",
        }

    def reply(self, request: dict[str, Any]) -> dict[str, Any]:
        params = request.get("params") or {}
        tab: dict[str, Any] = {"id": TAB, "state": "none"}
        if params.get("ownership_action") != "release":
            tab["ownership"] = {"source": "craze", "session_id": params.get("session_id")}
        return {"id": request.get("id"), "ok": True, "result": {"accepted": True, "tab": tab}}


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


def assert_framing(host: _FakeHostSocket) -> None:
    """One JSON object per connection, ending in exactly one newline."""
    assert host.connections == len(host.raw), (host.connections, host.raw)
    for raw in host.raw:
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
            craze_home = tmp_path / "host-off"
            craze_home.mkdir()
            (craze_home / "config.toml").write_text("host_status = false\n", encoding="utf-8")
            env["CRAZE_HOME"] = str(craze_home)
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


# --- roost ---------------------------------------------------------------------

# Every params field roost-session 0.0.19 accepts, as its unknown-field error
# lists them. roost denies unknown fields, so a line carrying anything else --
# lifecycle_if, which newer roost added -- is rejected whole.
_ROOST_019_PARAMS = {
    "tab_id",
    "source",
    "session_id",
    "ownership_action",
    "lifecycle",
    "attention",
    "severity",
    "title",
    "body",
    "detail",
    "metadata",
}


def roost_line(line_id: str, action: str, **params: Any) -> dict[str, Any]:
    """One whole tab.agent_report request, for the fake agent's session."""
    return {
        "id": line_id,
        "op": "tab.agent_report",
        "params": {
            "tab_id": TAB,
            "source": "craze",
            "session_id": FAKE_SESSION,
            "ownership_action": action,
            **params,
        },
    }


def lifecycles(lines: list[dict[str, Any]]) -> list[str | None]:
    """Each line's lifecycle, None for a line that leaves it unchanged."""
    return [ln["params"].get("lifecycle") for ln in lines]


def actions(lines: list[dict[str, Any]]) -> list[str]:
    return [ln["params"]["ownership_action"] for ln in lines]


def _has_claim(lines: list[dict[str, Any]]) -> bool:
    return actions(lines)[:1] == ["claim"]


def assert_roost_wire(lines: list[dict[str, Any]]) -> None:
    """String-wrapped int64 ids, never decreasing (a claim and the state line
    that follows it in one report share the report's seq), and no params field
    roost-session 0.0.19 would reject."""
    ids = [ln["id"] for ln in lines]
    assert all(isinstance(i, str) and i.isdigit() for i in ids), ids
    assert [int(i) for i in ids] == sorted(int(i) for i in ids), ids
    for ln in lines:
        assert set(ln["params"]) <= _ROOST_019_PARAMS, ln


def craze_version(craze_bin: Path) -> str:
    out = subprocess.run(
        [str(craze_bin), "version"], capture_output=True, text=True, check=True, timeout=WAIT
    )
    return out.stdout.strip()


def test_roost_echo_turn_sequence(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """R1: a plain turn is exactly claim, working, finished, release.

    The claim carries the model, craze's provider and craze's version; the
    turn's end raises roost's "Turn complete" banner.
    """
    version = craze_version(craze_bin)
    assert version, "craze version printed nothing"
    with FakeRoost() as roost, PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, env_extra=roost.env()
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        roost.wait_for(_has_claim, "the claim")
        tui.write(b"hello\r")
        tui.wait_contains("echo: hello", timeout=WAIT)
        roost.wait_for(lambda ls: lifecycles(ls)[-1:] == ["finished"], "the finished line")
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    lines = roost.snapshot()
    assert len(lines) == 4, lines
    ids = [ln["id"] for ln in lines]
    assert_roost_wire(lines)
    assert len(set(ids)) == 4, ids
    assert lines == [
        roost_line(
            ids[0],
            "claim",
            lifecycle="inactive",
            attention="clear",
            detail="session_start",
            metadata={"model": "default", "craze.provider": "cursor", "version": version},
        ),
        roost_line(ids[1], "preserve", lifecycle="working", attention="clear", detail="prompt"),
        roost_line(
            ids[2],
            "preserve",
            lifecycle="finished",
            attention="set",
            severity="info",
            title="craze",
            body="Turn complete",
            detail="stop",
        ),
        roost_line(ids[3], "release", lifecycle="inactive", attention="clear", detail="session_end"),
    ]
    assert_framing(roost)


def test_roost_permission_card_waits(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """R2: a permission card is waiting/warn, with the card's header as the body."""
    with FakeRoost() as roost, PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="permission",
        extra_args=["--no-force"],
        env_extra=roost.env(),
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        roost.wait_for(_has_claim, "the claim")
        tui.write(b"run it\r")
        tui.wait_contains("[a]llow", timeout=WAIT)
        roost.wait_for(lambda ls: "waiting" in lifecycles(ls), "waiting")
        tui.write(b"a")
        tui.wait_contains("decision:opt-once", timeout=WAIT)
        roost.wait_for(
            lambda ls: lifecycles(ls) == ["inactive", "working", "waiting", "working", "finished"],
            "working then finished after the answer",
        )
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    lines = roost.snapshot()
    assert lifecycles(lines) == ["inactive", "working", "waiting", "working", "finished", "inactive"]
    assert actions(lines) == ["claim", "preserve", "preserve", "preserve", "preserve", "release"]
    assert_roost_wire(lines)
    waiting = lines[2]
    body = waiting["params"].get("body")
    assert isinstance(body, str) and body.startswith("permission "), waiting
    assert waiting == roost_line(
        waiting["id"],
        "preserve",
        lifecycle="waiting",
        attention="set",
        severity="warn",
        title="craze",
        body=body,
        detail="permission_prompt",
    )


def test_roost_failed_turn(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """R2: a turn that ends in an error is failed/error, the error's first line
    as the body."""
    with FakeRoost() as roost, PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, script="turnfail", env_extra=roost.env()
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        roost.wait_for(_has_claim, "the claim")
        tui.write(b"boom\r")
        tui.wait_contains("the turn failed", timeout=WAIT)
        roost.wait_for(lambda ls: "failed" in lifecycles(ls), "the failed line")
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    lines = roost.snapshot()
    assert lifecycles(lines) == ["inactive", "working", "failed", "inactive"], lines
    assert_roost_wire(lines)
    failed = lines[2]
    assert failed == roost_line(
        failed["id"],
        "preserve",
        lifecycle="failed",
        attention="set",
        severity="error",
        title="craze",
        body="json-rpc error -32000: the turn failed",
        detail="error",
    )


def test_roost_cancelled_turn(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """R2: Esc on a running turn is finished with attention cleared: no banner
    for a turn the user stopped themselves."""
    with FakeRoost() as roost, PTYCraze(
        craze_bin, fake_agent_bin, tmp_path, script="hang", env_extra=roost.env()
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        roost.wait_for(_has_claim, "the claim")
        tui.write(b"wait for it\r")
        roost.wait_for(lambda ls: "working" in lifecycles(ls), "the working line")
        tui.write(b"\x1b")
        roost.wait_for(lambda ls: "finished" in lifecycles(ls), "the finished line")
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    lines = roost.snapshot()
    assert lifecycles(lines) == ["inactive", "working", "finished", "inactive"], lines
    assert_roost_wire(lines)
    finished = lines[2]
    assert finished == roost_line(
        finished["id"],
        "preserve",
        lifecycle="finished",
        attention="clear",
        detail="cancelled",
    )


def test_roost_queue_drain_has_no_finished_between_turns(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """D5: a queued message drained after a turn never reaches roost as finished.

    Same shape as the herdr case: the second turn is a second long, so a
    finished line between the two would outlive the hub's idle debounce.
    """
    with FakeRoost() as roost, PTYCraze(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="long-turn",
        step="2s,500ms",
        env_extra=roost.env(),
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        roost.wait_for(_has_claim, "the claim")
        tui.write(b"go the long way\r")
        tui.wait_contains("Working", timeout=WAIT)
        tui.write(b"Reply with MANGO\r")
        tui.wait_contains("#1 Reply with MANGO", timeout=WAIT)
        mark = tui.mark()
        tui.wait_contains_since("❯ Reply with MANGO", mark, timeout=LONG_WAIT)
        drained = time.monotonic()
        roost.wait_for(
            lambda ls: _finished_after(roost, ls, drained),
            "the finished line after the drained turn",
            timeout=LONG_WAIT,
        )
        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)

    lines = roost.snapshot()
    assert lifecycles(lines) == ["inactive", "working", "finished", "inactive"], lines
    assert_roost_wire(lines)


def _finished_after(roost: FakeRoost, lines: list[dict[str, Any]], after: float) -> bool:
    """The newest line is a finished one that arrived after the given moment.

    Called under the server's lock, from wait_for, so lines and arrivals agree.
    """
    return bool(lines) and lifecycles(lines)[-1] == "finished" and roost.arrivals[-1] > after


def test_roost_sigterm_still_releases(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """SIGTERM is an exit like any other, and the roost tab is released."""
    with FakeRoost() as roost:
        with PTYCraze(craze_bin, fake_agent_bin, tmp_path, env_extra=roost.env()) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            roost.wait_for(_has_claim, "the claim")
            tui.close()
            # close() escalates to SIGKILL after 2 s; a SIGTERM exit is not that.
            assert tui.proc.returncode != -9, tui.screen()[-3000:]
        _wait_fake_gone(fake_agent_bin)
        lines = roost.snapshot()

    assert actions(lines) == ["claim", "release"], lines
    assert_roost_wire(lines)
    assert int(lines[1]["id"]) > int(lines[0]["id"]), lines
    assert lines[1] == roost_line(
        lines[1]["id"], "release", lifecycle="inactive", attention="clear", detail="session_end"
    )


def test_child_env_loses_both_hook_gates(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """R4 and H5 together: reporting to both hosts, the agent child has neither
    ROOST_AGENT_HOOK nor HERDR_ENV, and keeps ROOST_TAB_ID and HERDR_PANE_ID.

    With --no-host-status all four reach the child, and neither socket hears
    anything.
    """
    with FakeHerdr() as herdr, FakeRoost() as roost:
        env = {**herdr.env(), **roost.env()}
        with PTYCraze(craze_bin, fake_agent_bin, tmp_path, script="env", env_extra=env) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            herdr.wait_for(_has_ready_idle, "the ready idle")
            roost.wait_for(_has_claim, "the claim")
            assert _child_env_names(tui) == {"ROOST_TAB_ID", "HERDR_PANE_ID"}
            quit_craze(tui)
        _wait_fake_gone(fake_agent_bin)
        assert herdr.snapshot() and roost.snapshot(), "both gates were met, so craze reported"

    with FakeHerdr() as herdr, FakeRoost() as roost:
        env = {**herdr.env(), **roost.env()}
        with PTYCraze(
            craze_bin,
            fake_agent_bin,
            tmp_path,
            script="env",
            extra_args=["--no-host-status"],
            env_extra=env,
        ) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            assert _child_env_names(tui) == {
                "ROOST_AGENT_HOOK",
                "ROOST_TAB_ID",
                "HERDR_ENV",
                "HERDR_PANE_ID",
            }
            quit_craze(tui)
        _wait_fake_gone(fake_agent_bin)
        assert herdr.connections == 0 and herdr.snapshot() == [], herdr.snapshot()
        assert roost.connections == 0 and roost.snapshot() == [], roost.snapshot()
