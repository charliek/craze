"""The host cleanup's process scan fails closed (plan 030 X62, sol r15-c7r2).

On macOS the scan lists the machine's pids with `ps`, which can stall or fail.
A scan that does not answer within PS_TIMEOUT, or fails, must raise
ProcessScanError -- never return an empty set, which would read as "nothing
left" -- and the cleanup that meets it must fail the test without signalling a
single pid it could not match to the test. These tests force both on any
platform by standing in for `ps` and for os.kill.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

import conftest
from conftest import PS_TIMEOUT, ProcessScanError


def _as_darwin(mp: pytest.MonkeyPatch) -> None:
    # Only the platform the scan branches on: nothing here reaches the real
    # macOS sysctl path, since `ps` is replaced below.
    mp.setattr(conftest.sys, "platform", "darwin")


# Each test patches inside its own MonkeyPatch.context(), undone before the
# test returns: the autouse host_cleanup's teardown shares the function-scoped
# monkeypatch fixture and runs its real scan after the test.


def test_a_ps_that_does_not_answer_is_a_scan_error() -> None:
    seen: dict[str, object] = {}

    def stalled(*args: object, **kwargs: object) -> subprocess.CompletedProcess[bytes]:
        seen["timeout"] = kwargs.get("timeout")
        raise subprocess.TimeoutExpired(cmd="ps", timeout=PS_TIMEOUT)

    with pytest.MonkeyPatch.context() as mp:
        _as_darwin(mp)
        mp.setattr(conftest.subprocess, "run", stalled)
        with pytest.raises(ProcessScanError, match="did not answer"):
            conftest._all_pids()
    # The bound is passed to ps itself: without it a stalled ps hangs here.
    assert seen["timeout"] == PS_TIMEOUT


def test_a_ps_that_fails_is_a_scan_error() -> None:
    def failed(*args: object, **kwargs: object) -> subprocess.CompletedProcess[bytes]:
        raise subprocess.CalledProcessError(1, "ps")

    with pytest.MonkeyPatch.context() as mp:
        _as_darwin(mp)
        mp.setattr(conftest.subprocess, "run", failed)
        with pytest.raises(ProcessScanError, match="failed"):
            conftest._all_pids()


def test_a_failed_scan_signals_nothing(tmp_path: Path) -> None:
    """_stop_hosts meets the scan error before it signals anything: a pid it
    could not match to this test is never sent SIGTERM or SIGKILL."""
    signalled: list[tuple[int, int]] = []

    def unknown() -> set[int]:
        raise ProcessScanError("`ps` did not answer within 5s")

    # A registry entry naming a live pid (this process): if the cleanup
    # signalled registered pids without the scan it would signal this one.
    hosts = tmp_path / ".cache" / "craze" / "hosts"
    hosts.mkdir(parents=True)
    entry = hosts / "0123456789ab.json"
    entry.write_text(f'{{"pid": {os.getpid()}}}')
    try:
        with pytest.MonkeyPatch.context() as mp:
            mp.setattr(conftest, "marker_pids", unknown)
            mp.setattr(conftest.os, "kill", lambda pid, sig: signalled.append((pid, sig)))
            with pytest.raises(ProcessScanError):
                conftest._stop_hosts(tmp_path, "/nonexistent/fake-agent")
    finally:
        # The stand-in entry is this test's own, not a host's: gone before the
        # real cleanup looks.
        entry.unlink()
    assert signalled == []
