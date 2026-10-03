"""The per-machine hub (plan 032 §3.5, §3.12): `craze hub` run by hand, and
the commands that start it on demand, `craze ps`, `craze new` and
`craze bridge --hub`.

The first cases start the real binary's hidden `craze hub` directly: what a
spawned hub does once it is up is the same, less its ready line. The later
ones run `craze ps` and `craze bridge --hub`, which find the hub or spawn it
(hub.Ensure re-executes craze). Every case runs under the suite's isolation
(conftest.isolate_run_env: its own HOME, CRAZE_HOME and CRAZE_RUNTIME_DIR, the
marker host_cleanup finds a hub by), and conftest's host_cleanup stops a hub a
case leaves -- which each case that starts one checks. Each step has a bound
of its own.
"""

from __future__ import annotations

import json
import os
import re
import signal
import socket
import subprocess
import threading
import time
from pathlib import Path

import pytest

import conftest
from conftest import TEST_HOST_IDLE_EXIT, seed_host_idle_exit
from test_bridge import _hello as _bridge_hello
from test_bridge import _recv_line, _send
from test_detach import _entries, _prompt
from test_tui import PTYCraze

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
    assert result["capabilities"]["sessionCreate"] is True, result["capabilities"]
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


# ------------------------------------------------------------- craze ps


def _ps(craze_bin: Path, home: Path | None, *args: str) -> subprocess.CompletedProcess[bytes]:
    """`craze ps` with no terminal, in the test's environment -- its HOME
    home's when given, CRAZE_HOME then following it as the hosts' does -- so
    the hub it spawns carries the test's marker."""
    env = os.environ.copy()
    if home is not None:
        env["HOME"] = str(home)
        env.pop("CRAZE_HOME", None)
    return subprocess.run(
        [str(craze_bin), "ps", *args],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        env=env,
        timeout=3 * WAIT,
    )


def _ps_rows(out: bytes) -> list[dict[str, str]]:
    """craze ps's table, a dict a row by its header's columns."""
    lines = out.decode().rstrip("\n").split("\n")
    header = lines[0]
    assert header.split() == ["SESSION", "STATE", "PROVIDER", "MODEL", "DIR", "SINCE", "TITLE"], header
    names = header.split()
    starts = [header.index(n) for n in names]
    rows = []
    for line in lines[1:]:
        cells = [line[s : (starts[i + 1] if i + 1 < len(starts) else None)].strip() for i, s in enumerate(starts)]
        rows.append(dict(zip(names, cells)))
    return rows


def _the_hub(home: Path) -> dict:
    """The one hub record under home, its hub running and carrying this
    test's marker."""
    recs = [json.loads(p.read_text(encoding="utf-8")) for p in sorted(_hubs(home).glob("*.json"))]
    assert len(recs) == 1, recs
    rec = recs[0]
    assert conftest.pid_alive(rec["pid"]) and rec["pid"] in conftest.marker_pids(), rec
    return rec


def _cleanup_stops(rec: dict, tmp_path: Path, fake_agent_bin: Path) -> None:
    """host_cleanup's own stop leaves nothing: the hub (and every host) gone,
    its record and socket with it."""
    assert conftest._stop_hosts(tmp_path, str(fake_agent_bin)) is None
    deadline = time.monotonic() + WAIT
    while conftest.pid_alive(rec["pid"]) and time.monotonic() < deadline:
        time.sleep(0.05)
    assert not conftest.pid_alive(rec["pid"]), "the hub outlived the cleanup"
    assert not Path(rec["socket"]).exists(), "the hub left its socket"
    assert not conftest._stray_processes(str(fake_agent_bin))


def _ready_entries(home: Path, n: int) -> list[dict]:
    """The registry's n ready entries under home, once there are."""
    deadline = time.monotonic() + WAIT
    seen: list[dict] = []
    while time.monotonic() < deadline:
        seen = [e for e in _entries(home) if e.get("ready") and e.get("crazeSessionId")]
        if len(seen) == n:
            return seen
        time.sleep(0.05)
    raise AssertionError(f"not {n} ready hosts in the registry: {seen}")


def test_ps_lists_detached_sessions_and_attach_reaches_one(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A7: two sessions, each in a terminal of its own, every terminal hung
    up -- the hosts stay -- and `craze ps` lists both from the hub it starts:
    each one's short id, idle, its provider, its directory, its first prompt
    as its title. `--json` prints the hub's roster (its epoch the hub's id);
    `--no-hub` reads the hosts itself and says so. Then `craze attach` in one
    session's directory reaches it."""
    monkeypatch.delenv("CRAZE_DETACH", raising=False)
    seed_host_idle_exit(tmp_path)
    work = {"first session": tmp_path / "proj-a", "second session": tmp_path / "proj-b"}
    for i, (text, ws) in enumerate(work.items()):
        ws.mkdir()
        # Each agent session an id of its own: the index keys a row by it.
        env = {"HOME": str(tmp_path), "CRAZE_FAKE_SESSION_ID": f"fake-session-{i + 1}"}
        with PTYCraze(craze_bin, fake_agent_bin, ws, setctty=True, env_extra=env) as tui:
            tui.wait_contains("cursor", timeout=WAIT)
            _prompt(tui, text)
            tui.hangup(timeout=5)
    entries = {e["crazeSessionId"]: e for e in _ready_entries(tmp_path, 2)}
    assert all(conftest.pid_alive(e["pid"]) for e in entries.values()), "a host died with its terminal"

    out = _ps(craze_bin, tmp_path)
    assert out.returncode == 0 and out.stderr == b"", (out.returncode, out.stderr)
    rows = {r["SESSION"]: r for r in _ps_rows(out.stdout)}
    assert sorted(rows) == sorted(i[-8:] for i in entries), out.stdout
    for sid, e in entries.items():
        row = rows[sid[-8:]]
        ws = Path(e["workspace"])
        text = next(t for t, w in work.items() if w.resolve() == ws.resolve())
        assert row["STATE"] == "idle" and row["PROVIDER"] == "cursor" and row["MODEL"] == "-", row
        assert row["DIR"] in ("~/" + ws.name, str(ws)), row
        assert row["TITLE"] == text, row
    rec = _the_hub(tmp_path)

    out = _ps(craze_bin, tmp_path, "--json")
    assert out.returncode == 0 and out.stderr == b"", out.stderr
    doc = json.loads(out.stdout)
    assert doc["epoch"] == rec["hubId"], (doc, rec)
    assert sorted(r["sessionId"] for r in doc["sessions"]) == sorted(entries), doc
    for r in doc["sessions"]:
        assert r["hostId"] == entries[r["sessionId"]]["hostId"] and r["status"] == "reachable", r
        assert r["row"]["sessionId"] == r["sessionId"] and r["row"]["activity"] == "idle", r

    out = _ps(craze_bin, tmp_path, "--no-hub")
    assert out.returncode == 0, out.stderr
    assert out.stderr == b"craze ps: --no-hub: reading each session's host directly\n", out.stderr
    assert sorted(r["SESSION"] for r in _ps_rows(out.stdout)) == sorted(rows)

    (sid_a,) = [s for s, e in entries.items() if Path(e["workspace"]).resolve() == work["first session"].resolve()]
    attach = PTYCraze(
        craze_bin, None, work["first session"], command=["attach"], setctty=True, env_extra={"HOME": str(tmp_path)}
    )
    with attach as view:
        view.wait_contains("echo: first session", timeout=WAIT)
        view.hangup(timeout=5)
    assert conftest.pid_alive(entries[sid_a]["pid"]), "the attach's hang-up ended the session"
    _cleanup_stops(rec, tmp_path, fake_agent_bin)


def test_ps_with_nothing_running(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """With no session, `craze ps` prints `no sessions running`, exit 0, and
    `--json` the hub's empty roster; the hub it started is the test's, and
    the cleanup stops it."""
    out = _ps(craze_bin, None)
    assert (out.returncode, out.stdout, out.stderr) == (0, b"no sessions running\n", b"")
    rec = _the_hub(_home())
    out = _ps(craze_bin, None, "--json")
    assert out.returncode == 0 and out.stderr == b"", out.stderr
    assert json.loads(out.stdout) == {"epoch": rec["hubId"], "cursor": 0, "sessions": []}, out.stdout
    _cleanup_stops(rec, tmp_path, fake_agent_bin)


def test_bridge_hub_answers_a_hub_hello(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """`craze bridge --hub` starts the hub and pumps: a hello on stdin is
    answered by the hub itself; stdin's end ends the bridge, exit 0. With
    `--session` too it is one stderr line, exit 1."""
    proc = subprocess.Popen(
        [str(craze_bin), "bridge", "--hub"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=os.environ.copy(),
    )
    try:
        _send(proc, _bridge_hello("1"))
        reply = json.loads(_recv_line(proc, timeout=3 * WAIT))
    finally:
        assert proc.stdin is not None
        proc.stdin.close()
    assert proc.wait(timeout=WAIT) == 0
    assert proc.stderr is not None and proc.stderr.read() == b""
    rec = _the_hub(_home())
    assert reply["id"] == "1" and reply["result"]["endpoint"] == {
        "kind": "hub",
        "hostId": rec["hubId"],
        "crazeVersion": rec["crazeVersion"],
        "pid": rec["pid"],
    }, reply

    both = subprocess.run(
        [str(craze_bin), "bridge", "--hub", "--session", "x"],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        timeout=WAIT,
    )
    assert both.returncode == 1 and both.stdout == b"", both
    assert both.stderr.startswith(b"craze bridge: --hub and --session cannot be used together") and both.stderr.count(b"\n") == 1
    _cleanup_stops(rec, tmp_path, fake_agent_bin)


# ------------------------------------------------------------- craze new


def _new(craze_bin: Path, *args: str) -> subprocess.CompletedProcess[bytes]:
    """`craze new` with no terminal, in the test's environment, so the hub it
    spawns -- and every host that hub spawns -- carries the test's marker."""
    return subprocess.run(
        [str(craze_bin), "new", *args],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        env=os.environ.copy(),
        timeout=6 * WAIT,
    )


def _started(out: subprocess.CompletedProcess[bytes], work: Path) -> str:
    """craze new's one line, `started <id> in <dir>`: the short id."""
    assert out.returncode == 0 and out.stderr == b"", (out.returncode, out.stdout, out.stderr)
    m = re.fullmatch(rb"started ([0-9a-f]{8}) in (.+)\n", out.stdout)
    assert m is not None and m.group(2).decode() == str(work), out.stdout
    return m.group(1).decode()


def _created_entry(short: str) -> dict:
    """The registry entry of the session craze new started, by its short id."""
    (entry,) = [e for e in _entries(_home()) if e.get("crazeSessionId", "").endswith(short)]
    return entry


def _ready_entry(short: str) -> dict:
    """_created_entry once it says ready, within 10 s: the host writes its
    entry's ready flag on its own, after the start craze new's answer waited
    for (X48), so the flag can follow the answer (plan 032 X69: seen on macOS
    CI)."""
    deadline = time.monotonic() + 10.0
    while True:
        entry = _created_entry(short)
        if entry.get("ready") is True:
            return entry
        assert time.monotonic() < deadline, f"the created session's entry never said ready: {entry}"
        time.sleep(0.05)


def test_new_starts_sessions_that_ps_lists(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A12: `craze new` starts a session in another directory through the hub
    it spawns -- with a first prompt and without -- and returns only once the
    session has started (its registry entry, carrying the create's request id,
    says ready on its own soon after), printing its short id and directory; the hub's hosts find the
    fake agent through `[agents]` (no agent binary crosses to them), and the
    provider is persisted as the next default. `craze ps` then lists both,
    the first titled by its prompt."""
    craze_home = Path(os.environ["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    (craze_home / "config.toml").write_text(
        f'host_idle_exit = "{TEST_HOST_IDLE_EXIT}"\n\n[agents]\ncursor = "{fake_agent_bin}"\n', encoding="utf-8"
    )
    work = tmp_path / "proj-new"
    work.mkdir()

    first = _started(_new(craze_bin, "-C", str(work), "--provider", "cursor", "first", "words"), work)
    entry = _ready_entry(first)
    assert entry["workspace"] == str(work), entry
    assert entry["requestId"].startswith("new-") and entry["requestHash"].startswith("sha256:"), entry
    # The host saves its provider just after its start publishes readiness,
    # which is what craze new's answer waits for: the save can follow it
    # (SF-117), so it is awaited, within 10 s.
    deadline = time.monotonic() + 10.0
    while not re.search(r'(?m)^provider = "cursor"$', (craze_home / "config.toml").read_text(encoding="utf-8")):
        assert time.monotonic() < deadline, "the created session did not persist its provider within 10s"
        time.sleep(0.05)

    second = _started(_new(craze_bin, "-C", str(work), "--provider", "cursor"), work)
    assert second != first
    _ready_entry(second)
    rec = _the_hub(_home())

    deadline = time.monotonic() + WAIT
    while True:
        out = _ps(craze_bin, None)
        assert out.returncode == 0 and out.stderr == b"", out.stderr
        rows = {r["SESSION"]: r for r in _ps_rows(out.stdout)}
        if rows.get(first, {}).get("TITLE") == "first words" or time.monotonic() > deadline:
            break
        time.sleep(0.1)
    assert sorted(rows) == sorted([first, second]), out.stdout
    assert rows[first]["TITLE"] == "first words" and rows[first]["PROVIDER"] == "cursor", rows[first]
    assert rows[second]["STATE"] == "idle" and rows[second]["DIR"] == str(work), rows[second]
    _cleanup_stops(rec, tmp_path, fake_agent_bin)


def test_new_refuses_with_the_hubs_words(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A create the hub refuses is its words on one stderr line, exit 1: no
    provider named and none configured. Nothing is started."""
    out = _new(craze_bin, "-C", str(tmp_path), "hello")
    assert out.returncode == 1 and out.stdout == b"", out
    assert out.stderr.startswith(b"craze new: params.provider is required") and out.stderr.count(b"\n") == 1, out.stderr
    assert not [e for e in _entries(_home()) if e.get("requestId")]
    _cleanup_stops(_the_hub(_home()), tmp_path, fake_agent_bin)


def test_new_json_refusal_is_the_wire_error(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """SF-124: `craze new --json` refused by the hub prints one JSON object on
    stdout, {"error": <the wire's error>}: .error.data.code and .reason are
    where a script reads them on the wire. The stderr line and exit 1 are as
    without --json. Nothing is started."""
    out = _new(craze_bin, "--json", "-C", str(tmp_path), "hello")
    assert out.returncode == 1, out
    assert out.stderr.startswith(b"craze new: params.provider is required") and out.stderr.count(b"\n") == 1, out.stderr
    assert out.stdout.count(b"\n") == 1, out.stdout
    body = json.loads(out.stdout)
    assert list(body) == ["error"], body
    assert body["error"]["data"]["code"] == "bad_request" and body["error"]["data"]["reason"], body
    assert body["error"]["message"].startswith("params.provider is required"), body
    assert not [e for e in _entries(_home()) if e.get("requestId")]
    _cleanup_stops(_the_hub(_home()), tmp_path, fake_agent_bin)
