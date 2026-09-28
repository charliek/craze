"""craze attach (plan 027 C28, §3.15): the full TUI over a running session.

A host craze runs over the fake agent in one pty and `craze attach` in
another, both with the same HOME -- the registry and the session index are
found under it -- exactly as two terminal tabs of one user would be. The
resolution's failures and the refused flags need no terminal: they are answered
before one is asked for, so they run as plain subprocesses.
"""

from __future__ import annotations

import json
import os
import subprocess
import time
from pathlib import Path

from conftest import host_env_names
from test_bridge import _wait_running_entry
from test_tui import _ANSI, PTYCraze, _wait_fake_gone, _wait_glob, _wait_output, quit_craze

WAIT = 10.0


def _host(craze_bin: Path, fake_agent_bin: Path, workspace: Path, home: Path) -> PTYCraze:
    """A host craze over the fake agent, in workspace, with HOME home."""
    return PTYCraze(craze_bin, fake_agent_bin, workspace, env_extra={"HOME": str(home)})


def _attach(craze_bin: Path, workspace: Path, home: Path, args: list[str] | None = None) -> PTYCraze:
    """craze attach in a terminal of its own, its cwd workspace."""
    return PTYCraze(
        craze_bin,
        None,
        workspace,
        command=["attach", *(args or [])],
        env_extra={"HOME": str(home)},
    )


def _run_attach(craze_bin: Path, cwd: Path, home: Path, args: list[str]) -> subprocess.CompletedProcess:
    """craze attach with no terminal, for an answer it gives before needing
    one. PWD is the cwd, so the directory it names is the one given."""
    env = os.environ.copy()
    env["HOME"] = str(home)
    env["PWD"] = str(cwd)
    env.pop("CRAZE_HOME", None)
    for name in host_env_names(env):
        del env[name]
    return subprocess.run(
        [str(craze_bin), "attach", *args],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        cwd=str(cwd),
        env=env,
        timeout=WAIT,
    )


def test_attach_joins_the_session_and_leaves_it_running(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The attach TUI shows the host's transcript; a prompt typed in it
    reaches the session and shows in both; quitting it (a view close) exits 0
    and says nothing, and the host goes on taking prompts. A second attach
    then sees the host quit: it exits 0 with `craze: session ended` once its
    screen is restored (§3.9)."""
    with _host(craze_bin, fake_agent_bin, tmp_path, tmp_path) as host:
        host.wait_contains("cursor")
        host.write(b"from the host\r")
        host.wait_contains("echo: from the host")
        _wait_running_entry(tmp_path / ".cache" / "craze")

        with _attach(craze_bin, tmp_path, tmp_path) as view:
            view.wait_contains("echo: from the host")
            host_mark, view_mark = host.mark(), view.mark()
            view.write(b"from the viewer\r")
            view.wait_contains_since("echo: from the viewer", view_mark)
            host.wait_contains_since("echo: from the viewer", host_mark)
            view.write(b"\x04")
            assert view.wait_exit() == 0, view.screen()[-3000:]
            assert "session ended" not in _ANSI.sub("", view.screen())

        host_mark = host.mark()
        host.write(b"after the viewer\r")
        host.wait_contains_since("echo: after the viewer", host_mark)

        with _attach(craze_bin, tmp_path, tmp_path) as view:
            view.wait_contains("echo: after the viewer")
            quit_craze(host)
            assert view.wait_exit(timeout=WAIT) == 0, view.screen()[-3000:]
            _wait_output(view, "craze: session ended")
    _wait_fake_gone(fake_agent_bin)


def test_attach_refuses_the_flags_of_a_session(craze_bin: Path, tmp_path: Path) -> None:
    """--continue, --resume, --provider and --model choose or start a session;
    an attach joins one that runs. Each is refused by name, a usage error."""
    for args, flag in (
        (["--continue"], "continue"),
        (["-c"], "continue"),
        (["--resume"], "resume"),
        (["--provider", "grok"], "provider"),
        (["--model", "gpt-5"], "model"),
    ):
        result = _run_attach(craze_bin, tmp_path, tmp_path, args)
        assert result.returncode == 2, (args, result)
        assert result.stdout == b"", result.stdout
        assert result.stderr == (
            f"craze attach: --{flag} does not apply: attach joins a running session\n".encode()
        ), (args, result.stderr)


def test_attach_lists_what_runs_when_it_cannot_choose(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """No session running anywhere: one line, exit 1. Two running in one
    directory: an attach from another directory is exit 1, naming the one it
    looked in, listing both and the --session hint; from theirs it is exit 2,
    listing both (owner, §3.19)."""
    busy, idle = tmp_path / "busy", tmp_path / "idle"
    busy.mkdir()
    idle.mkdir()

    result = _run_attach(craze_bin, idle, tmp_path, [])
    assert result.returncode == 1, result
    assert result.stderr == f"craze: no running craze session in {idle}\n".encode(), result.stderr

    with _host(craze_bin, fake_agent_bin, busy, tmp_path) as one, _host(
        craze_bin, fake_agent_bin, busy, tmp_path
    ) as two:
        one.wait_contains("cursor")
        two.wait_contains("cursor")
        hosts = tmp_path / ".cache" / "craze" / "hosts"
        _wait_glob(hosts, "*.json")
        entries = _wait_entries(hosts, 2)
        ids = sorted(entries)

        result = _run_attach(craze_bin, idle, tmp_path, [])
        assert result.returncode == 1, result
        lines = result.stderr.decode().splitlines()
        assert lines[0] == f"craze: no running craze session in {idle}", lines
        assert sorted(lines[1:-1]) == [f"  {id_}  {busy}" for id_ in ids], lines
        assert lines[-1] == "attach to one with: craze attach --session <id>", lines

        result = _run_attach(craze_bin, busy, tmp_path, [])
        assert result.returncode == 2, result
        lines = result.stderr.decode().splitlines()
        assert lines[0] == f"craze: 2 running craze sessions in {busy}:", lines
        assert sorted(lines[1:-1]) == [f"  {id_}  {busy}" for id_ in ids], lines
        assert lines[-1] == "attach to one with: craze attach --session <id>", lines

        quit_craze(one)
        quit_craze(two)
    _wait_fake_gone(fake_agent_bin)


def _wait_entries(hosts: Path, n: int) -> dict[str, dict]:
    """The n ready registry entries under hosts, by the id a listing names
    them by (their craze session id)."""
    deadline = time.monotonic() + WAIT
    found: dict[str, dict] = {}
    while time.monotonic() < deadline:
        found = {}
        for path in hosts.glob("*.json"):
            try:
                entry = json.loads(path.read_text(encoding="utf-8"))
            except (FileNotFoundError, ValueError):
                continue
            if entry.get("ready") and entry.get("crazeSessionId"):
                found[entry["crazeSessionId"]] = entry
        if len(found) == n:
            return found
        time.sleep(0.05)
    raise AssertionError(f"the registry never held {n} ready entries: {found}")
