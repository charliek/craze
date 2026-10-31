"""Reopening a saved session through the hub (plan 046 B3, B6): `craze new
--load` and `craze ps --saved`, against the built binaries.

Every case seeds its own session index (CRAZE_HOME's sessions.jsonl, under
conftest.isolate_run_env) with saved sessions whose provider is cursor, the
fake agent its `[agents].cursor`, its script `load` (every session/load is
answered, replaying a short transcript). `craze new --load` starts the hub
on demand (hub.Ensure), and the hub starts each session's host (craze serve
--load). Each step has a bound of its own, and conftest's host_cleanup stops
whatever a case leaves.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import time
from pathlib import Path

import pytest

import conftest
from test_detach import _entries
from test_hub import _cleanup_stops, _home, _ps, _ps_rows, _saved_rows, _the_hub

WAIT = 10.0
# craze new --load's own bound is 4 minutes; a reopen here takes well under a
# second, and a hung one is reported by this.
LOAD_WAIT = 6 * WAIT

SAVED_ID = "0192f0aa-5555-7000-8000-00000000c0de"
SHORT = SAVED_ID[-8:]


def _seed(work: Path, fake_agent_bin: Path, idle_exit: str = conftest.TEST_HOST_IDLE_EXIT) -> None:
    """config.toml (the fake agent as cursor, host_idle_exit) and an index
    holding one saved cursor session in work."""
    craze_home = Path(os.environ["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    (craze_home / "config.toml").write_text(
        f'host_idle_exit = "{idle_exit}"\n\n[agents]\ncursor = "{fake_agent_bin}"\n', encoding="utf-8"
    )
    row = {"sessionId": "fake-session-1", "provider": "cursor", "cwd": str(work), "title": "a saved session",
           "pinned": False, "createdAt": "2026-10-12T09:00:00Z", "updatedAt": "2026-10-12T10:00:00Z",
           "crazeId": SAVED_ID}
    (craze_home / "sessions.jsonl").write_text(json.dumps(row) + "\n", encoding="utf-8")


def _load(craze_bin: Path, *args: str) -> subprocess.CompletedProcess[bytes]:
    """`craze new --load` with no terminal, in the test's environment -- the
    fake agent's script `load` -- so the hub it spawns, and every host that
    hub spawns, carries the test's marker."""
    return subprocess.run(
        [str(craze_bin), "new", "--load", *args],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        env={**os.environ, "CRAZE_FAKE_SCRIPT": "load"},
        timeout=LOAD_WAIT,
    )


def _serving(session: str) -> list[dict]:
    """The registry's entries serving session."""
    return [e for e in _entries(_home()) if e.get("crazeSessionId") == session]


def _gone(pid: int, timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    while conftest.pid_alive(pid):
        if time.monotonic() > deadline:
            return False
        time.sleep(0.05)
    return True


def test_a_saved_session_reopens_and_is_then_running(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """B-4 (i), (ii), B-5: `craze ps --saved` lists the saved session; `craze
    new --load <its short id>` reopens it -- `reopened <id> in <dir>`, its
    host loading it in its own directory -- and `craze ps` lists it running
    (polled: a list right after can be a second stale), `ps --saved` no
    longer; `new --load` again says `already running`, the same host. Stopped,
    it is reopened at once in a new host, the stopped one waited out (B9
    (ii)), and listed saved while it is stopped."""
    work = tmp_path / "proj"
    work.mkdir()
    _seed(work, fake_agent_bin)

    out = _ps(craze_bin, None, "--saved")
    assert out.returncode == 0, out.stderr
    assert [(r["SESSION"], r["PROVIDER"], r["TITLE"]) for r in _saved_rows(out.stdout)] == [
        (SHORT, "cursor", "a saved session")], out.stdout

    out = _load(craze_bin, SHORT)
    assert (out.returncode, out.stdout, out.stderr) == (0, f"reopened {SHORT} in {work}\n".encode(), b""), out
    (entry,) = _serving(SAVED_ID)
    assert Path(entry["workspace"]).resolve() == work.resolve() and "requestId" not in entry, entry
    rec = _the_hub(_home())

    deadline = time.monotonic() + WAIT
    while True:
        listed = _ps(craze_bin, None)
        if listed.returncode == 0 and any(r["SESSION"] == SHORT for r in _ps_rows(listed.stdout)):
            break
        assert time.monotonic() < deadline, listed.stdout
        time.sleep(0.1)
    saved = _ps(craze_bin, None, "--saved")
    assert (saved.returncode, saved.stdout, saved.stderr) == (0, b"no saved sessions\n", b""), saved

    out = _load(craze_bin, SAVED_ID, "--json")
    assert out.returncode == 0 and out.stderr == b"", out
    res = json.loads(out.stdout)
    assert res["joined"] is True and res["session"]["hostId"] == entry["hostId"], res
    out = _load(craze_bin, SHORT)
    assert (out.returncode, out.stdout) == (0, f"already running {SHORT} in {work}\n".encode()), out

    stop = subprocess.run([str(craze_bin), "stop", SHORT], stdin=subprocess.DEVNULL, capture_output=True,
                          env=os.environ.copy(), timeout=3 * WAIT)
    assert stop.returncode == 0, stop
    out = _load(craze_bin, SHORT)
    assert (out.returncode, out.stdout, out.stderr) == (0, f"reopened {SHORT} in {work}\n".encode(), b""), out
    (again,) = _serving(SAVED_ID)
    assert again["hostId"] != entry["hostId"], (again, entry)
    assert _gone(entry["pid"], WAIT), f"the stopped host is still running: {conftest.describe_pid(entry['pid'])}"
    _cleanup_stops(rec, tmp_path, fake_agent_bin)


def test_a_reopened_host_nobody_attaches_to_idles_out(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """B9 (xiii), B3's idle rule: a reopened host that no client attaches to
    follows the normal idle rules -- a session that has had no prompt from
    craze exits after min(host_idle_exit, 5 min), here 2 s -- and its session
    is saved again."""
    work = tmp_path / "proj"
    work.mkdir()
    _seed(work, fake_agent_bin, idle_exit="2s")
    out = _load(craze_bin, SHORT, "--json")
    assert out.returncode == 0, out
    # The host from the answer itself: read from the registry after the
    # command has returned, it could have idled out already.
    pid = json.loads(out.stdout)["session"]["host"]["pid"]
    rec = _the_hub(_home())
    assert _gone(pid, 6 * WAIT), f"the reopened host did not idle out: {conftest.describe_pid(pid)}"
    assert _serving(SAVED_ID) == []
    saved = _ps(craze_bin, None, "--saved")
    assert saved.returncode == 0 and [r["SESSION"] for r in _saved_rows(saved.stdout)] == [SHORT], saved.stdout
    _cleanup_stops(rec, tmp_path, fake_agent_bin)


@pytest.mark.parametrize(
    "args, want",
    [
        (["abcd", "a", "prompt"], "craze new: --load takes no prompt: reopen the session, then craze send it one"),
        (["abcd", "--provider", "grok"], "craze new: --load does not take --provider: a reopened session keeps its own"),
        (["abcd", "-C", "/tmp"], "craze new: --load does not take -C/--dir: a reopened session keeps its own"),
        (["abcd", "--wait"], "craze new: --load does not take --wait: a reopened session keeps its own"),
        (["abcd", "--title", "t"], "craze new: --load does not take --title: a reopened session keeps its own"),
        (["abcd", "--no-force"], "craze new: --load does not take --no-force: a reopened session keeps its own"),
        ([""], "craze new: --load needs a session id (craze ps --saved lists them)"),
    ],
)
def test_new_load_usage_errors(craze_bin: Path, args: list[str], want: str) -> None:
    """B-5: what a reopen does not take is a usage error, exit 2, one line,
    before anything is dialled: no hub is started."""
    out = _load(craze_bin, *args)
    assert (out.returncode, out.stdout, out.stderr.decode()) == (2, b"", want + "\n"), out
    hubs = _home() / ".cache" / "craze" / "hubs"
    assert not list(hubs.glob("*.json")), "a usage error started a hub"


def test_new_load_of_no_saved_session(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """An id no saved session matches is exit 1, `no saved session <id>`,
    before any hub is asked; with --json, one JSON error object."""
    work = tmp_path / "proj"
    work.mkdir()
    _seed(work, fake_agent_bin)
    out = _load(craze_bin, "zzzz")
    assert (out.returncode, out.stdout, out.stderr) == (
        1, b"", b"craze new: no saved session zzzz (craze ps --saved lists them)\n"), out
    out = _load(craze_bin, "zzzz", "--json")
    assert out.returncode == 1 and json.loads(out.stdout) == {
        "error": {"message": "no saved session zzzz (craze ps --saved lists them)"}, "kind": "config"}, out
    assert re.fullmatch(rb"craze new: no saved session zzzz[^\n]*\n", out.stderr), out.stderr
