"""craze bridge (plan 027 C14, §3.10): the SSH-side byte pump.

It resolves the one running craze session (or --session's) against the
registry C13 built, dials its control socket, checks the peer, and pumps
stdin/stdout against it verbatim -- it speaks no protocol itself. These tests
drive it as a real subprocess against a real TUI (PTYCraze + the fake agent),
exactly as an SSH exec would: its stdin and stdout are pipes, never a pty.
"""

from __future__ import annotations

import json
import os
import select
import subprocess
from pathlib import Path
from typing import Any

from conftest import REMOVED_CONFIG_ENV, host_env_names
from test_tui import PTYCraze, _wait_entry, _wait_fake_gone, _wait_glob, quit_craze

WAIT = 10.0


def _bridge_env(home: Path, **extra: str) -> dict[str, str]:
    """The environment an SSH exec of `craze bridge` would run with: HOME
    shared with the tab that started the host, and nothing else it needs --
    not CRAZE_HOME, not the runtime-base variables (§3.10: "Nothing about the
    runtime base"), proven by leaving them unset here while the TUI that bound
    the socket ran under conftest's own short CRAZE_RUNTIME_DIR.
    """
    env = os.environ.copy()
    env["HOME"] = str(home)
    for name in ("CRAZE_HOME", "CRAZE_PROVIDER", "CRAZE_RUNTIME_DIR", "XDG_RUNTIME_DIR",
                 "CRAZE_CONTROL_SOCKET", REMOVED_CONFIG_ENV, "CRAZE_JOURNAL"):
        env.pop(name, None)
    for name in host_env_names(env):
        del env[name]
    env.update(extra)
    return env


def _run_bridge(craze_bin: Path, home: Path, args: list[str], **extra_env: str) -> subprocess.CompletedProcess:
    """One craze bridge invocation expected to finish on its own (an error
    path: no pump ever starts)."""
    env = _bridge_env(home, **extra_env)
    return subprocess.run(
        [str(craze_bin), "bridge", *args],
        input=b"",
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        timeout=WAIT,
    )


def _popen_bridge(craze_bin: Path, home: Path, args: list[str]) -> subprocess.Popen:
    """A long-lived craze bridge, its stdin/stdout pipes -- never a pty, as
    an SSH exec's are not one either."""
    env = _bridge_env(home)
    return subprocess.Popen(
        [str(craze_bin), "bridge", *args],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
    )


def _hello(id_: str = "1") -> dict[str, Any]:
    return {
        "jsonrpc": "2.0",
        "id": id_,
        "method": "hello",
        "params": {"protocols": [1], "client": {"kind": "test", "name": "pytest-bridge"}},
    }


def _send(proc: subprocess.Popen, obj: dict[str, Any]) -> None:
    assert proc.stdin is not None
    proc.stdin.write(json.dumps(obj).encode() + b"\n")
    proc.stdin.flush()


def _recv_line(proc: subprocess.Popen, timeout: float = WAIT) -> bytes:
    assert proc.stdout is not None
    ready, _, _ = select.select([proc.stdout], [], [], timeout)
    if not ready:
        raise AssertionError("craze bridge: timed out waiting for a reply line")
    line = proc.stdout.readline()
    if not line:
        raise AssertionError("craze bridge: EOF waiting for a reply line")
    return line


def _wait_running_entry(cache: Path, timeout: float = WAIT) -> dict:
    (entry_path,) = _wait_glob(cache / "hosts", "*.json", timeout=timeout)
    return _wait_entry(entry_path, lambda e: e["ready"], timeout=timeout)


def test_bridge_hello_round_trip(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A hello written to craze bridge's stdin reaches the host, and its
    result comes back on stdout verbatim (A18, the pytest half of
    TestARequestThenEOFStillGetsItsReply)."""
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor")
        entry = _wait_running_entry(tmp_path / ".cache" / "craze")

        bridge = _popen_bridge(craze_bin, tmp_path, ["--session", entry["crazeSessionId"]])
        try:
            _send(bridge, _hello("1"))
            reply = json.loads(_recv_line(bridge))
            assert reply["id"] == "1", reply
            result = reply["result"]
            assert result["endpoint"]["kind"] == "host", result
            assert result["endpoint"]["hostId"] == entry["hostId"]
            assert result["clientId"] and result["token"], result
        finally:
            bridge.stdin.close()
            bridge.wait(timeout=WAIT)

        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)


def test_bridge_half_close_still_reads_every_reply(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """stdin's EOF only half-closes the socket's write side (CloseWrite):
    the bridge keeps relaying whatever the host still has to say, and exits 0
    only once the host itself closes the connection (§3.10)."""
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor")
        entry = _wait_running_entry(tmp_path / ".cache" / "craze")

        bridge = _popen_bridge(craze_bin, tmp_path, ["--session", entry["crazeSessionId"]])
        _send(bridge, _hello("1"))
        hello_reply = json.loads(_recv_line(bridge))
        assert hello_reply["id"] == "1", hello_reply

        _send(bridge, {
            "jsonrpc": "2.0",
            "id": "2",
            "method": "sessions.list",
            "params": {},
        })
        # The bridge's stdin closes right after the write above: the host
        # still owes this reply, over the connection's now half-closed
        # write side.
        bridge.stdin.close()
        list_reply = json.loads(_recv_line(bridge))
        assert list_reply["id"] == "2", list_reply

        quit_craze(tui)
        # The host's own close ends the connection; the bridge relays that
        # EOF and exits 0.
        assert bridge.stdout.read() == b""
        code = bridge.wait(timeout=WAIT)
        assert code == 0, (code, bridge.stderr.read())
    _wait_fake_gone(fake_agent_bin)


def test_bridge_no_session_of_that_id(craze_bin: Path, tmp_path: Path) -> None:
    """--session naming nothing live is the contract line, exactly, exit 1,
    nothing on stdout (§3.10)."""
    home = tmp_path / "home"
    result = _run_bridge(craze_bin, home, ["--session", "nope"])
    assert result.returncode == 1, result
    assert result.stdout == b"", result.stdout
    assert result.stderr == b"craze bridge: no session nope\n", result.stderr


def test_bridge_no_flag_no_host_running(craze_bin: Path, tmp_path: Path) -> None:
    """No --session and no host running is the other half of the same
    contract."""
    home = tmp_path / "home"
    result = _run_bridge(craze_bin, home, [])
    assert result.returncode == 1, result
    assert result.stdout == b"", result.stdout
    assert result.stderr == b"craze bridge: no session running\n", result.stderr


def test_bridge_unknown_flag_exits_one(craze_bin: Path, tmp_path: Path) -> None:
    home = tmp_path / "home"
    result = _run_bridge(craze_bin, home, ["--nope"])
    assert result.returncode == 1, result
    assert result.stdout == b"", result.stdout
    assert result.stderr.startswith(b"craze bridge: "), result.stderr
    assert result.stderr.count(b"\n") == 1, result.stderr


def test_bridge_stray_config_env_exits_one(craze_bin: Path, tmp_path: Path) -> None:
    """A stray removed config-file variable in the SSH environment -- the root's shared
    PersistentPreRunE's own usage error (exit 2) everywhere else -- still
    reads as a bridge error here: one line, exit 1 (§3.10)."""
    home = tmp_path / "home"
    result = _run_bridge(craze_bin, home, [], **{REMOVED_CONFIG_ENV: str(tmp_path / "config.toml")})
    assert result.returncode == 1, result
    assert result.stdout == b"", result.stdout
    assert result.stderr.startswith(b"craze bridge: "), result.stderr
    assert REMOVED_CONFIG_ENV.encode() in result.stderr, result.stderr
    assert result.stderr.count(b"\n") == 1, result.stderr


def test_bridge_resolves_by_provider_session_id_and_host_id(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """--session also matches the provider's own id and the host id, read
    straight off the registry entry (§3.10's resolution)."""
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor")
        entry = _wait_running_entry(tmp_path / ".cache" / "craze")

        for id_ in (entry["providerSessionId"], entry["hostId"]):
            bridge = _popen_bridge(craze_bin, tmp_path, ["--session", id_])
            try:
                _send(bridge, _hello("1"))
                reply = json.loads(_recv_line(bridge))
                assert reply["id"] == "1", (id_, reply)
                assert reply["result"]["endpoint"]["hostId"] == entry["hostId"]
            finally:
                bridge.stdin.close()
                bridge.wait(timeout=WAIT)

        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)


def test_bridge_stdout_write_failure_exits_one_not_a_signal(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The SSH channel gone from under the bridge -- its stdout's only
    reader closed -- is a write failure mapped to exit 1, never SIGPIPE
    killing the process (§3.10): Go's default action for a write to a broken
    pipe on fd 1 is process death unless something is notified for it, which
    is exactly what this proves end to end, with a real closed pipe, not a
    mock."""
    with PTYCraze(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor")
        entry = _wait_running_entry(tmp_path / ".cache" / "craze")

        bridge = _popen_bridge(craze_bin, tmp_path, ["--session", entry["crazeSessionId"]])
        _send(bridge, _hello("1"))
        _recv_line(bridge)  # the hello reply, read and discarded

        # No more reader on the bridge's stdout: on Linux/macOS, once every
        # read end of a pipe is closed, the next write to it raises EPIPE at
        # once, whatever is still buffered.
        bridge.stdout.close()
        # A second request the host still answers over the socket (refused,
        # a connection binds once -- the reply itself is what matters), so
        # the bridge's conn -> stdout direction has something to relay.
        _send(bridge, _hello("2"))
        bridge.stdin.close()

        code = bridge.wait(timeout=WAIT)
        stderr = bridge.stderr.read()
        assert code == 1, (code, stderr)
        assert stderr.startswith(b"craze bridge: "), stderr

        quit_craze(tui)
    _wait_fake_gone(fake_agent_bin)
