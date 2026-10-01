"""The per-machine hub, `craze hub` (plan 032 §3.5, C10), run by hand.

No craze command spawns a hub until `craze ps` and `craze bridge --hub` (C13),
so these cases start the real binary's hidden `craze hub` directly: what a
spawned hub does once it is up is the same, less its ready line. Every case
runs under the suite's isolation (conftest.isolate_run_env: its own HOME,
CRAZE_HOME and CRAZE_RUNTIME_DIR, the marker host_cleanup finds a hub by), and
conftest's host_cleanup stops a hub a case leaves -- which the last case
checks. Each step has a bound of its own.
"""

from __future__ import annotations

import json
import os
import signal
import socket
import subprocess
import threading
import time
from pathlib import Path

import pytest

import conftest

WAIT = 10.0


def _hubs(home: Path) -> Path:
    return home / ".cache" / "craze" / "hubs"


def _host_logs(home: Path) -> Path:
    return home / ".cache" / "craze" / "host-logs"


def _home() -> Path:
    return Path(os.environ["HOME"])


def _start(craze_bin: Path, tmp_path: Path, **env: str) -> subprocess.Popen[bytes]:
    """`craze hub` in the test's environment plus env, its stderr in a file.

    A thread of its own waits for it, so it is reaped the moment it exits:
    a hub is this process's child, and a zombie still answers signal 0 --
    which, where there is no /proc to say it is one (macOS), the cleanup's
    scan would take for a hub left running.
    """
    err = (tmp_path / f"hub-stderr-{time.monotonic_ns()}.txt").open("wb")
    try:
        proc = subprocess.Popen(
            [str(craze_bin), "hub"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=err,
            env={**os.environ, **env},
        )
    finally:
        err.close()
    threading.Thread(target=proc.wait, daemon=True).start()
    return proc


def _record_of(proc: subprocess.Popen[bytes]) -> dict:
    """The hub record naming proc, within WAIT. A hub writes its record by
    rename, so a file that does not parse is never one half-written."""
    deadline = time.monotonic() + WAIT
    while True:
        for path in sorted(_hubs(_home()).glob("*.json")):
            try:
                rec = json.loads(path.read_text(encoding="utf-8"))
            except FileNotFoundError:
                continue
            if rec.get("pid") == proc.pid:
                rec["_path"] = path
                return rec
        if proc.poll() is not None:
            pytest.fail(f"craze hub exited {proc.returncode} before its record named it")
        if time.monotonic() > deadline:
            pytest.fail(f"no hub record named pid {proc.pid} within {WAIT}s")
        time.sleep(0.02)


def _hello(sock: str) -> dict:
    """hello on the hub's socket: its result, or the test fails."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(WAIT)
        s.connect(sock)
        req = {"jsonrpc": "2.0", "id": 1, "method": "hello", "params": {"protocols": [1], "client": {"kind": "test"}}}
        s.sendall(json.dumps(req).encode() + b"\n")
        buf = b""
        while not buf.endswith(b"\n"):
            chunk = s.recv(65536)
            if not chunk:
                pytest.fail(f"the hub closed the connection after {buf!r}")
            buf += chunk
    resp = json.loads(buf)
    assert "error" not in resp, resp
    return resp["result"]


def _log_of(rec: dict) -> str:
    path = _host_logs(_home()) / f"hub-{rec['ns']}.log"
    try:
        return path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return ""


def test_a_hub_serves_its_hello_and_stops_on_sigterm(craze_bin: Path, tmp_path: Path) -> None:
    proc = _start(craze_bin, tmp_path)
    rec = _record_of(proc)
    result = _hello(rec["socket"])
    assert result["endpoint"]["kind"] == "hub"
    assert result["endpoint"]["hostId"] == rec["hubId"]
    assert result["endpoint"]["pid"] == proc.pid
    assert result["capabilities"]["rosterSubscribe"] is True
    assert "clientId" not in result
    proc.send_signal(signal.SIGTERM)
    assert proc.wait(timeout=WAIT) == 0
    assert not rec["_path"].exists(), "the hub left its record"
    assert not Path(rec["socket"]).exists(), "the hub left its socket"
    log = _log_of(rec)
    assert "stopping: SIGTERM" in log, log


def test_a_hub_nobody_uses_exits_after_its_grace(craze_bin: Path, tmp_path: Path) -> None:
    proc = _start(craze_bin, tmp_path, CRAZE_HUB_IDLE="1s")
    rec = _record_of(proc)
    assert proc.wait(timeout=WAIT) == 0
    assert not rec["_path"].exists(), "the idle hub left its record"
    assert not Path(rec["socket"]).exists(), "the idle hub left its socket"
    assert "stopping: idle" in _log_of(rec)


def test_a_killed_hub_is_replaced_by_the_next(craze_bin: Path, tmp_path: Path) -> None:
    """A SIGKILLed hub leaves its record and its socket behind; the next hub
    takes the free lock, removes the refusing socket, binds its own and writes
    its record over the stale one."""
    first = _start(craze_bin, tmp_path)
    old = _record_of(first)
    first.send_signal(signal.SIGKILL)
    first.wait(timeout=WAIT)
    assert old["_path"].exists() and Path(old["socket"]).exists(), "a SIGKILLed hub tidied up"
    second = _start(craze_bin, tmp_path)
    new = _record_of(second)
    assert new["hubId"] != old["hubId"]
    assert new["socket"] == old["socket"]
    assert _hello(new["socket"])["endpoint"]["hostId"] == new["hubId"]
    assert "removed a stale socket" in _log_of(new)
    second.send_signal(signal.SIGTERM)
    assert second.wait(timeout=WAIT) == 0


def test_the_cleanup_stops_a_hub_left_running(craze_bin: Path, tmp_path: Path, fake_agent_bin: Path) -> None:
    """A hub a test leaves -- a failed assertion, say -- is found by the
    test's marker and stopped by host_cleanup's own stop (its SIGTERM is the
    hub's teardown), with nothing reported left."""
    proc = _start(craze_bin, tmp_path)
    rec = _record_of(proc)
    assert proc.pid in conftest.marker_pids()
    assert conftest._stop_hosts(tmp_path, str(fake_agent_bin)) is None
    assert proc.wait(timeout=WAIT) == 0
    assert not rec["_path"].exists()
    assert not conftest._stray_processes(str(fake_agent_bin))
