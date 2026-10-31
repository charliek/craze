"""The waiting commands end to end (plan 041 §3.1-§3.4, §7 W2): `craze new
--wait`, `craze send`, `craze wait` and `craze stop` through the built binary
over the fake agent.

`craze new --wait` starts its session's host itself -- no hub -- detached and
unattended, so the host carries the test's marker (conftest's host_cleanup
finds anything a case leaves), and finds the fake agent through `[agents]` in
the test's config, as no agent binary crosses to it. Every case stops the
sessions it starts and checks they have gone. Each step has a bound of its
own.
"""

from __future__ import annotations

import errno
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
from collections.abc import Callable
from pathlib import Path
from urllib.parse import quote

import pytest

from conftest import PS_TIMEOUT, REMOVED_CONFIG_ENV, TEST_HOST_IDLE_EXIT, _argv, _environ, marker_pids, pid_alive
from sse_fixture import SSEFixture, Step, answer, call_step, plan_path_in, write_native_config
from test_detach import _entries
from test_native import alive, gone, until_alive

WAIT = 10.0


def _config(fake_agent_bin: Path) -> None:
    """The test's config.toml: the suite's idle exit, and the fake agent as
    cursor -- the only way a host craze new --wait starts can find it."""
    craze_home = Path(os.environ["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    (craze_home / "config.toml").write_text(
        f'host_idle_exit = "{TEST_HOST_IDLE_EXIT}"\n\n[agents]\ncursor = "{fake_agent_bin}"\n', encoding="utf-8"
    )


def _craze(craze_bin: Path, *args: str, script: str = "echo", stdin: bytes | None = None) -> subprocess.CompletedProcess[bytes]:
    """craze with no terminal, in the test's environment and the fake agent's
    script; stdin is /dev/null unless given."""
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = script
    return subprocess.run(
        [str(craze_bin), *args],
        input=stdin,
        stdin=subprocess.DEVNULL if stdin is None else None,
        capture_output=True,
        env=env,
        timeout=6 * WAIT,
    )


def _home() -> Path:
    return Path(os.environ["HOME"])


def _stop(craze_bin: Path, short: str) -> None:
    """craze stop, and the session's host gone from the registry within WAIT."""
    out = _craze(craze_bin, "stop", short)
    assert out.returncode == 0 and out.stdout == f"stopped {short}\n".encode(), out
    deadline = time.monotonic() + WAIT
    while any(e.get("crazeSessionId", "").endswith(short) for e in _entries(_home())):
        assert time.monotonic() < deadline, f"session {short}'s host is still registered"
        time.sleep(0.05)


def test_new_wait_then_send_wait_and_stop(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A task handed to a new unattended session returns its result; craze
    send continues it -- without --wait, then followed with craze wait from
    the handle it printed, and with --wait from a prompt file it removes, and
    from stdin -- craze stop ends it, and craze wait then names the reload."""
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()

    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(work), "first", "task")
    assert out.returncode == 0 and out.stderr == b"", out
    res = json.loads(out.stdout)
    assert res["status"] == "completed" and res["message"] == "echo: first task" and res["turn"] == "turn-1", res
    short = res["short"]
    (entry,) = [e for e in _entries(_home()) if e.get("crazeSessionId") == res["session"]]
    assert entry["workspace"] == str(work) and not entry.get("requestId"), entry
    if sys.platform == "linux":
        argv = _argv(entry["pid"])
        assert "--unattended" in argv and "--no-host-status" in argv, argv
        assert not any(a.startswith("--agent-bin") for a in argv), argv

    out = _craze(craze_bin, "send", short, "second")
    m = re.fullmatch(rb"started turn-2\nnext: craze wait " + short.encode() + rb" --from (\S+)\n", out.stdout)
    assert out.returncode == 0 and m is not None, out
    out = _craze(craze_bin, "wait", short, "--from", m.group(1).decode(), "--json")
    res = json.loads(out.stdout)
    assert out.returncode == 0 and res["status"] == "completed", out
    assert res["turn"] == "turn-2" and res["message"] == "echo: second" and "partial" not in res, res

    task = tmp_path / "task.md"
    task.write_text("third, from a file\n", encoding="utf-8")
    out = _craze(craze_bin, "send", short, "--wait", "--prompt-file", str(task), "--remove-prompt-file")
    assert out.returncode == 0 and out.stdout.startswith(b"echo: third, from a file\n---\ncompleted \xc2\xb7 "), out
    assert not task.exists()
    out = _craze(craze_bin, "send", short, "--wait", "--json", stdin=b"fourth, piped\n")
    res = json.loads(out.stdout)
    assert out.returncode == 0 and res["message"] == "echo: fourth, piped" and res["turn"] == "turn-4", out

    _stop(craze_bin, short)
    out = _craze(craze_bin, "wait", short)
    assert out.returncode == 1 and out.stdout == b"", out
    assert out.stderr == f"craze wait: the session has ended (craze send {short} <prompt> reloads it)\n".encode(), out


def test_send_reloads_a_stopped_session(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """craze send to a session craze new --wait started and craze stop ended
    loads it again -- unattended, the session's own id -- and runs the
    prompt there."""
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "--plan", "-C", str(work), "plan it")
    res = json.loads(out.stdout)
    assert out.returncode == 0 and res["status"] == "completed", out
    session, short = res["session"], res["short"]
    _stop(craze_bin, short)

    out = _craze(craze_bin, "send", short, "--wait", "--json", "now do it", script="load")
    res = json.loads(out.stdout)
    assert out.returncode == 0 and res["status"] == "completed" and res["session"] == session, out
    assert res["message"] == "echo: now do it", res
    (entry,) = [e for e in _entries(_home()) if e.get("crazeSessionId") == session]
    if sys.platform == "linux":
        argv = _argv(entry["pid"])
        assert f"--load={session}" in argv and "--unattended" in argv and "--plan" in argv, argv
    _stop(craze_bin, short)


def test_the_prompt_rules(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """No prompt (stdin /dev/null), words beside --prompt-file, an empty one
    and plain craze new's --wait-only flags are usage errors, exit 2, one
    line -- and nothing is started; an id nothing knows is exit 1."""
    _config(fake_agent_bin)
    task = tmp_path / "task.md"
    task.write_text("x", encoding="utf-8")
    for args, want in [
        (["new", "--wait", "--provider", "cursor", "-C", str(tmp_path)], b"craze new: needs a prompt"),
        (["new", "--wait", "--provider", "cursor", "-C", str(tmp_path), "--prompt-file", str(task), "words"], b"not both"),
        (["new", "--wait", "--provider", "cursor", "-C", str(tmp_path), "  "], b"craze new: the prompt is empty"),
        (["new", "--provider", "cursor", "-C", str(tmp_path), "--plan"], b"craze new: --plan needs --wait"),
        (["send", "abcd1234", "--max-message", "9", "x"], b"craze send: --max-message needs --wait"),
    ]:
        out = _craze(craze_bin, *args)
        assert out.returncode == 2 and out.stdout == b"" and want in out.stderr and out.stderr.count(b"\n") == 1, (args, out)
    out = _craze(craze_bin, "send", "abcd1234", "hello")
    assert out.returncode == 1 and out.stderr == b"craze send: no session abcd1234\n", out
    assert _entries(_home()) == []


def _process_tree(root: int) -> list[int]:
    """root and every process descending from it, found as a tree kill finds
    them -- each process whose parent is in the tree -- from a snapshot of
    every process's parent: /proc/<pid>/stat on Linux, `ps` on macOS."""
    parents: dict[int, int] = {}
    if sys.platform == "linux":
        for p in Path("/proc").iterdir():
            if not p.name.isdigit():
                continue
            try:
                stat = (p / "stat").read_text(encoding="utf-8", errors="replace")
            except OSError:
                continue
            parents[int(p.name)] = int(stat.rsplit(")", 1)[1].split()[1])
    else:
        out = subprocess.run(["ps", "-A", "-o", "pid=,ppid="], capture_output=True, check=True, timeout=PS_TIMEOUT).stdout
        for line in out.decode().splitlines():
            f = line.split()
            if len(f) == 2 and f[0].isdigit() and f[1].isdigit():
                parents[int(f[0])] = int(f[1])
    tree = [root]
    i = 0
    while i < len(tree):
        tree += [pid for pid, ppid in parents.items() if ppid == tree[i] and pid not in tree]
        i += 1
    return tree


def _open_gate(gate: Path, waiter: subprocess.Popen[bytes]) -> int:
    """The write end of the fake agent's gate FIFO, once the agent has it open
    -- which it does when its first turn reaches its first step -- within
    WAIT; a FIFO no one reads refuses a non-blocking writer (ENXIO)."""
    deadline = time.monotonic() + WAIT
    while True:
        try:
            return os.open(gate, os.O_WRONLY | os.O_NONBLOCK)
        except OSError as e:
            if e.errno != errno.ENXIO:
                raise
        assert waiter.poll() is None, ("craze new --wait exited", waiter.returncode, waiter.communicate())
        assert time.monotonic() < deadline, "the session's turn never reached its first step"
        time.sleep(0.02)


def test_a_tree_kill_cancels_the_turn_and_leaves_the_session(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Claude Code's TaskStop ends a Bash call by signalling its whole process
    tree, found by parent pid (plan 041 X32, live check L6). craze new
    --wait's host is not in that tree -- it was started through an
    intermediate that exited at once -- so such a kill reaches the command
    alone: it cancels its own turn, `cancelled`, exit 3, and the session goes
    on, its host serving, for craze send to continue."""
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()
    gate = tmp_path / "gate"
    os.mkfifo(gate)
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = "long-turn"
    env["CRAZE_FAKE_GATE"] = str(gate)
    waiter = subprocess.Popen(
        [str(craze_bin), "new", "--wait", "--json", "--provider", "cursor", "-C", str(work), "a long task"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
    )
    fd = -1
    try:
        fd = _open_gate(gate, waiter)
        (entry,) = _entries(_home())
        tree = _process_tree(waiter.pid)
        assert entry["pid"] not in tree, ("the host is in the command's process tree", entry["pid"], tree)
        for pid in tree:
            os.kill(pid, signal.SIGTERM)
        out, err = waiter.communicate(timeout=6 * WAIT)
        assert waiter.returncode == 3 and err == b"", (waiter.returncode, out, err)
        res = json.loads(out)
        assert res["status"] == "cancelled" and res["turn"] == "turn-1" and res["session"] == entry["crazeSessionId"], res
        short = res["short"]
        assert pid_alive(entry["pid"]), "the tree kill ended the host"
        assert [e["hostId"] for e in _entries(_home())] == [entry["hostId"]]
        # The next turn's two steps, let through at the gate.
        os.write(fd, b"\x01\x01")
        out = _craze(craze_bin, "send", short, "--wait", "--json", "and on", script="long-turn")
        res = json.loads(out.stdout)
        assert out.returncode == 0 and res["status"] == "completed" and res["turn"] == "turn-2", out
    finally:
        if fd >= 0:
            os.close(fd)
        if waiter.poll() is None:
            waiter.kill()
            waiter.communicate()
    _stop(craze_bin, short)


def _one_object(out: subprocess.CompletedProcess[bytes]) -> dict:
    """The one JSON object a command printed on stdout, on one line."""
    assert out.stdout.count(b"\n") == 1 and out.stdout.endswith(b"\n"), out
    return json.loads(out.stdout)


def _usage_object(out: subprocess.CompletedProcess[bytes], kind: str = "usage") -> None:
    """A usage error under --json: exit 2, one object, the stderr line its words."""
    assert out.returncode == 2 and out.stderr.count(b"\n") == 1, out
    assert _one_object(out) == {"error": {"message": out.stderr.decode().rstrip("\n"), "kind": kind, "retryable": False}}, out


def test_usage_errors_under_json(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Through the binary's own command line: an unknown flag before and after
    --json, an invalid value, an argument error and a failure before the command
    ran each print one object beside the stderr line, and the prompt's and the
    provider's errors say their kinds. --json as a value, or past `--`, is no
    --json; craze prompt's stream never gets an object. Nothing is started."""
    _config(fake_agent_bin)
    work = str(tmp_path)
    for args in (
        ["wait", "--json", "--nope"],
        ["wait", "--nope", "--json"],
        ["version", "--json"],
        ["--json", "nosuchcommand"],
        ["wait", "abcd1234", "--json", "--timeout", "soon"],
        ["wait", "a", "b", "--json"],
        ["new", "--wait", "--json", "--plan", "--ask", "-C", work, "x"],
    ):
        _usage_object(_craze(craze_bin, *args))
    _usage_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "nosuch", "-C", work, "x"), "config")
    _usage_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", work, "  "), "prompt")
    _usage_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", work), "prompt")
    _usage_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", work, "--prompt-file", "/no/such/file"), "prompt")
    _usage_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", work, "x" * (33 << 10)), "prompt")
    env = {**os.environ, REMOVED_CONFIG_ENV: "/x"}
    _usage_object(subprocess.run([str(craze_bin), "wait", "abcd1234", "--json"], capture_output=True, env=env, stdin=subprocess.DEVNULL, timeout=WAIT))

    for args in (
        ["wait", "--nope"],
        ["new", "--title", "--json", "--nope"],
        ["new", "-C", "--json", "--nope"],
        ["new", "--wait", "--nope", "--", "--json"],
        ["wait", "--json", "--json=false", "--nope"],
        ["prompt", "--json", "--nope"],
    ):
        out = _craze(craze_bin, *args)
        assert out.returncode == 2 and out.stdout == b"" and out.stderr.count(b"\n") == 1, (args, out)
    assert _entries(_home()) == []


def test_failures_and_endings_say_their_kinds(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A failed turn's `error` is the object, a running ACP agent's JSON-RPC error
    an `agent` kind with no HTTP status; the turn's stop reason is `error`; a
    `craze wait` reading it afterwards says the same; a directory that is not one
    is `workspace`, an id nothing knows `config`."""
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(work), "go", script="turnfail")
    res = json.loads(out.stdout)
    assert out.returncode == 3 and res["status"] == "failed" and res["stopReason"] == "error", out
    assert res["error"] == {"message": res["error"]["message"], "kind": "agent", "retryable": False}, res
    assert "the turn failed" in res["error"]["message"], res
    short = res["short"]
    out = _craze(craze_bin, "wait", short, "--json")
    again = json.loads(out.stdout)
    assert out.returncode == 3 and again["partial"] is True and again["error"] == res["error"] and again["stopReason"] == "error", out
    _stop(craze_bin, short)

    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(work / "nope"), "go")
    assert out.returncode == 1 and _one_object(out)["error"]["kind"] == "workspace", out
    assert _one_object(out)["error"]["retryable"] is False and sorted(_one_object(out)) == ["error"], out
    out = _craze(craze_bin, "wait", "abcd1234", "--json")
    assert out.returncode == 1 and _one_object(out)["error"] == {"message": "no session abcd1234", "kind": "config", "retryable": False}, out
    assert _entries(_home()) == []


def test_an_agent_refusing_the_model_at_the_start(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """cursor's -32602 for a --model it does not serve is told from any other start
    failure: it names the model, is `model_not_found`, and says a retry will not
    help; its data, the agent's own diagnostic, stays out of the message; and the
    session the command started is stopped."""
    _config(fake_agent_bin)
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "--model", "nope-model", "-C", str(tmp_path), "go", script="permodel")
    assert out.returncode == 1, out
    err = _one_object(out)["error"]
    assert err["kind"] == "model_not_found" and err["retryable"] is False, out
    assert 'the agent refused model "nope-model": ' in err["message"], out
    assert "Invalid model value" not in err["message"], out
    assert _entries(_home()) == []


def test_a_native_start_refused_for_its_configuration(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A native --model no model answers to, and a model table that will not load,
    are the real matching and loading paths through the built binary: `config`, no
    retry helps, the session the command started stopped. No model is ever called."""
    _config(fake_agent_bin)
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "--model", "definitely-not-a-model", "-C", str(tmp_path), "go")
    assert out.returncode == 1, out
    err = _one_object(out)["error"]
    assert err["kind"] == "config" and err["retryable"] is False, out
    assert 'unknown model "definitely-not-a-model"' in err["message"], out
    assert _entries(_home()) == []

    native = Path(os.environ["CRAZE_HOME"]) / "native"
    native.mkdir(parents=True, exist_ok=True)
    (native / "models.toml").write_text("version = [\n", encoding="utf-8")
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "-C", str(tmp_path), "go")
    assert out.returncode == 1, out
    err = _one_object(out)["error"]
    assert err["kind"] == "config" and err["retryable"] is False, out
    assert "not valid TOML" in err["message"], out
    assert _entries(_home()) == []


def test_a_plan_returned_says_its_turn_ended_there(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """An ACP agent's plan under --plan: completed, its stop reason cancelled, and
    planEnded says so. A turn that ended any other way has no planEnded."""
    _config(fake_agent_bin)
    with (Path(os.environ["CRAZE_HOME"]) / "config.toml").open("a", encoding="utf-8") as f:
        f.write(f'grok = "{fake_agent_bin}"\n')
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "grok", "--plan", "-C", str(tmp_path), "plan it", script="grok-plan")
    res = json.loads(out.stdout)
    assert out.returncode == 0 and res["status"] == "completed" and res["stopReason"] == "cancelled", out
    assert res["planEnded"] is True and res["plan"], res
    _stop(craze_bin, res["short"])
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(tmp_path), "hi")
    res = json.loads(out.stdout)
    assert out.returncode == 0 and res["stopReason"] == "end_turn" and "planEnded" not in res and "error" not in res, out
    _stop(craze_bin, res["short"])


def _long_turn_waiter(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> tuple[subprocess.Popen[bytes], int, dict]:
    """craze new --wait --json on the fake agent's turn held at its gate: the
    waiter, the gate's write end, and the host's registry entry, the turn running."""
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()
    gate = tmp_path / "gate"
    os.mkfifo(gate)
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = "long-turn"
    env["CRAZE_FAKE_GATE"] = str(gate)
    waiter = subprocess.Popen(
        [str(craze_bin), "new", "--wait", "--json", "--provider", "cursor", "-C", str(work), "a long task"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
    )
    fd = _open_gate(gate, waiter)
    (entry,) = _entries(_home())
    return waiter, fd, entry


def _hosts_gone() -> None:
    deadline = time.monotonic() + WAIT
    while _entries(_home()):
        assert time.monotonic() < deadline, ("a host is still registered", _entries(_home()))
        time.sleep(0.05)


def test_a_stop_mid_turn_is_the_turns_result(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """craze stop under a running turn: its waiter reports the turn, `cancelled`,
    exit 3, stopped, saying what began it. An ACP agent reports no usage and shows
    no steps, so there is no usage and it is not called whole. craze wait afterwards
    names the reload, as it did."""
    waiter, fd, entry = _long_turn_waiter(craze_bin, fake_agent_bin, tmp_path)
    try:
        short = entry["crazeSessionId"][-8:]
        out = _craze(craze_bin, "stop", short)
        assert out.returncode == 0, out
        stdout, stderr = waiter.communicate(timeout=6 * WAIT)
        assert waiter.returncode == 3 and stderr == b"", (waiter.returncode, stdout, stderr)
        res = json.loads(stdout)
        assert res["status"] == "cancelled" and res["stopReason"] == "stopped" and res["turn"] == "turn-1", res
        err = res["error"]
        assert err["kind"] == "stopped" and err["retryable"] is False and err["detail"] == {"cause": "stop"}, res
        assert "usage" not in res and res["usageComplete"] is False, res
        assert res["toolCalls"] == 0 and res["files"] == [] and "handle" not in res, res
    finally:
        os.close(fd)
        if waiter.poll() is None:
            waiter.kill()
            waiter.communicate()
    _hosts_gone()
    record = json.loads((_home() / ".cache" / "craze" / "host-logs" / f"{entry['hostId']}.stop").read_text(encoding="utf-8"))
    assert record["cause"] == "stop" and record["turn"] == "turn-1", record
    out = _craze(craze_bin, "wait", short, "--json")
    assert out.returncode == 1 and _one_object(out)["error"]["kind"] == "stopped", out


def test_a_signal_to_the_host_says_so(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """SIGTERM to the host, not the waiter: the same result, its cause `signal`."""
    waiter, fd, entry = _long_turn_waiter(craze_bin, fake_agent_bin, tmp_path)
    try:
        os.kill(entry["pid"], signal.SIGTERM)
        stdout, stderr = waiter.communicate(timeout=6 * WAIT)
        assert waiter.returncode == 3 and stderr == b"", (waiter.returncode, stdout, stderr)
        res = json.loads(stdout)
        assert res["status"] == "cancelled" and res["stopReason"] == "stopped", res
        assert res["error"]["kind"] == "stopped" and res["error"]["detail"] == {"cause": "signal"}, res
    finally:
        os.close(fd)
        if waiter.poll() is None:
            waiter.kill()
            waiter.communicate()
    _hosts_gone()


def test_a_killed_host_is_told_from_a_stop(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """SIGKILL to the host says nothing, so its process does: `host_exited`, exit 1,
    retryable, one object with the handle, and the next craze sweeps what it left."""
    waiter, fd, entry = _long_turn_waiter(craze_bin, fake_agent_bin, tmp_path)
    try:
        os.kill(entry["pid"], signal.SIGKILL)
        stdout, stderr = waiter.communicate(timeout=6 * WAIT)
        assert waiter.returncode == 1, (waiter.returncode, stdout, stderr)
        body = json.loads(stdout)
        err = body["error"]
        assert err["kind"] == "host_exited" and err["retryable"] is True and "detail" not in err, body
        assert "its host exited" in err["message"] and body["handle"] and body["usageComplete"] is False, body
        assert "usage" not in body, body
        assert b"host_exited" not in stderr and b"its host exited" in stderr, stderr
    finally:
        os.close(fd)
        if waiter.poll() is None:
            waiter.kill()
            waiter.communicate()
    assert not (_home() / ".cache" / "craze" / "host-logs" / f"{entry['hostId']}.stop").exists(), "a killed host wrote a stop record"
    # A killed host cleans up nothing: the next read of the registry sweeps its row.
    assert _craze(craze_bin, "ps", "--json").returncode == 0
    _hosts_gone()


def _native_waiter(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, fixture: SSEFixture) -> subprocess.Popen[bytes]:
    """craze new --wait --json on a native session over fixture."""
    _config(fake_agent_bin)
    write_native_config(Path(os.environ["CRAZE_HOME"]) / "native", fixture.base_url)
    work = tmp_path / "proj"
    work.mkdir()
    (work / "a.txt").write_text("alpha\n", encoding="utf-8")
    return subprocess.Popen(
        [str(craze_bin), "new", "--wait", "--json", "--provider", "native", "-C", str(work), "read it"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=os.environ.copy(),
    )


def _held_step(reached: threading.Event, release: threading.Event) -> Callable[[object], Step]:
    """A scripted step that answers only once released: a model request in flight
    for as long as the test wants, reached saying it is."""

    def step(_request: object) -> Step:
        reached.set()
        release.wait(6 * WAIT)
        return answer("too late")

    return step


def _reached(reached: threading.Event, waiter: subprocess.Popen[bytes]) -> str:
    """The short id of the one host, once the turn's request is in flight."""
    deadline = time.monotonic() + 3 * WAIT
    while not reached.wait(0.05):
        assert waiter.poll() is None, ("craze new --wait exited", waiter.returncode, waiter.communicate())
        assert time.monotonic() < deadline, "the turn's request never reached the fixture"
    (entry,) = _entries(_home())
    return entry["crazeSessionId"][-8:]


def test_a_stop_during_the_first_step_has_no_usage(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A native turn stopped while its first request is out: nothing has reported,
    so no usage, and a step's spend is said to be missing."""
    reached, release = threading.Event(), threading.Event()
    with SSEFixture() as fixture:
        fixture.set_script([_held_step(reached, release)])
        waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
        try:
            short = _reached(reached, waiter)
            assert _craze(craze_bin, "stop", short).returncode == 0
            stdout, stderr = waiter.communicate(timeout=6 * WAIT)
            assert waiter.returncode == 3 and stderr == b"", (waiter.returncode, stdout, stderr)
            res = json.loads(stdout)
            assert res["status"] == "cancelled" and res["stopReason"] == "stopped", res
            assert res["error"]["detail"] == {"cause": "stop"}, res
            assert "usage" not in res and res["usageComplete"] is False and res["toolCalls"] == 0, res
        finally:
            release.set()
            if waiter.poll() is None:
                waiter.kill()
                waiter.communicate()
    _hosts_gone()


def test_a_stop_after_a_finished_step_has_its_usage(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A native turn stopped with its second request out: the first step's usage
    and its tool call are in the result, not the harness's zero for a cancelled
    turn, and the step in flight is said to be missing."""
    reached, release = threading.Event(), threading.Event()
    with SSEFixture() as fixture:
        fixture.set_script([call_step("read", {"filePath": "a.txt"}, usage=(40, 3)), _held_step(reached, release)])
        waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
        try:
            short = _reached(reached, waiter)
            assert _craze(craze_bin, "stop", short).returncode == 0
            stdout, stderr = waiter.communicate(timeout=6 * WAIT)
            assert waiter.returncode == 3 and stderr == b"", (waiter.returncode, stdout, stderr)
            res = json.loads(stdout)
            assert res["status"] == "cancelled" and res["stopReason"] == "stopped" and res["toolCalls"] == 1, res
            assert res["usage"]["turn"]["input"] == 40 and res["usage"]["turn"]["output"] == 3, res
            assert res["usageComplete"] is False, res
        finally:
            release.set()
            if waiter.poll() is None:
                waiter.kill()
                waiter.communicate()
    _hosts_gone()


def test_a_cancel_mid_step_reads_the_same(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """craze cancel mid-step: `cancelled`, exit 3, stop reason `cancelled` and no
    error, usage as a stop's, and the same word on the step left out."""
    reached, release = threading.Event(), threading.Event()
    with SSEFixture() as fixture:
        fixture.set_script([call_step("read", {"filePath": "a.txt"}, usage=(40, 3)), _held_step(reached, release)])
        waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
        try:
            short = _reached(reached, waiter)
            assert _craze(craze_bin, "cancel", short).returncode == 0
            stdout, stderr = waiter.communicate(timeout=6 * WAIT)
            assert waiter.returncode == 3 and stderr == b"", (waiter.returncode, stdout, stderr)
            res = json.loads(stdout)
            assert res["status"] == "cancelled" and res["stopReason"] == "cancelled" and "error" not in res, res
            assert res["usage"]["turn"]["input"] == 40 and res["usageComplete"] is False, res
        finally:
            release.set()
            if waiter.poll() is None:
                waiter.kill()
                waiter.communicate()
        _stop(craze_bin, short)


def _edits(craze_bin: Path, fake_agent_bin: Path, provider: str, workspace: Path, script: str) -> dict:
    """A waiting turn of the fake agent's edits script, as provider, in workspace."""
    _config(fake_agent_bin)
    with (Path(os.environ["CRAZE_HOME"]) / "config.toml").open("a", encoding="utf-8") as f:
        f.write(f'grok = "{fake_agent_bin}"\ngx = "{fake_agent_bin}"\n')
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", provider, "-C", str(workspace), "go", script=script)
    assert out.returncode == 0, out
    res = json.loads(out.stdout)
    _stop(craze_bin, res["short"])
    return res


@pytest.mark.parametrize("through_a_symlink", [False, True])
def test_files_from_a_cursor_turn(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, through_a_symlink: bool) -> None:
    """Every way a call ends and a path is spelled, through the binary: only what
    completed with kind edit, delete or move, spelled from the workspace, with -C
    itself a symlink; a file: URI decoded, any other left out, and a name kept whole."""
    root = tmp_path.resolve()
    real = root / "w"
    real.mkdir()
    workspace = real
    if through_a_symlink:
        workspace = root / "link"
        workspace.symlink_to(real)
    res = _edits(craze_bin, fake_agent_bin, "cursor", workspace, "edits")
    many = [f"many/{i:02d}.txt" for i in range(64)]
    assert res["files"] == [
        "made.txt",
        "rel/one.txt",
        "real.txt",
        "gone/dir/deleted.txt",
        "again.txt",
        "removed.txt",
        "new-name.txt",
        "old-name.txt",
        "moved-from.txt",
        "moved-to.txt",
        "uri.txt",
        "zero\u200bwidth.txt",
        *many,
    ], res["files"]
    assert res["outsideFiles"] == [str(root / "escaped.txt"), f"{workspace}-sibling/x.txt"], res
    assert res["filesTruncated"] is True, res
    assert "planFile" not in res, res


@pytest.mark.parametrize("provider", ["grok", "gx"])
def test_files_from_a_grok_turn(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, provider: str) -> None:
    """grok's and gx's edit tools by name, whatever kind they say. The plan file is
    the one under the session id the agent gave, which is not craze's, and it is in
    neither list; another session's plan.md and a worktree's are files like others."""
    workspace = tmp_path.resolve() / "w"
    workspace.mkdir()
    res = _edits(craze_bin, fake_agent_bin, provider, workspace, "grok-edits")
    assert res["session"] != "fake-session-1", res
    sessions = Path(os.environ["HOME"]).resolve() / ".grok" / "sessions" / quote(str(workspace), safe="")
    assert res["planFile"] == str(sessions / "fake-session-1" / "plan.md"), res
    assert res["files"] == ["plan.md", "made.txt", "written.txt"], res
    assert res["outsideFiles"] == [str(sessions / "another-session" / "plan.md")], res
    assert "filesTruncated" not in res, res


def test_files_absent_and_the_plain_footer(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A turn with no edit: files is [] and the keys beside it are absent. Plain
    output's footer says all three when there are some."""
    workspace = tmp_path.resolve() / "w"
    workspace.mkdir()
    res = _edits(craze_bin, fake_agent_bin, "cursor", workspace, "echo")
    assert res["files"] == [], res
    assert not {"outsideFiles", "planFile", "filesTruncated"} & res.keys(), res
    out = _craze(craze_bin, "new", "--wait", "--provider", "grok", "-C", str(workspace), "go", script="grok-edits")
    assert out.returncode == 0, out
    text = out.stdout.decode()
    assert "\nfiles: plan.md, made.txt, written.txt\n" in text, text
    assert "\noutside the workspace: " in text and "another-session/plan.md\n" in text, text
    assert "fake-session-1/plan.md\n" in text and "\nplan file: " in text, text
    _stop(craze_bin, re.search(r"^session (\S+) ·", text, re.M).group(1))


def test_files_from_a_native_turn(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A native turn in plan mode: the refused edit is not listed, the plan file is
    planFile, and a shell's write is never in files, the rule the docs state. A turn
    in agent mode lists its edit, and not one whose old text did not match."""
    _config(fake_agent_bin)
    workspace = tmp_path.resolve() / "w"
    workspace.mkdir()
    (workspace / "notes.txt").write_text("alpha\n", encoding="utf-8")
    craze_home = Path(os.environ["CRAZE_HOME"])
    with SSEFixture() as fixture:
        write_native_config(craze_home / "native", fixture.base_url)
        plans: list[str] = []

        def write_the_plan(request) -> Step:
            plans.append(plan_path_in(request))
            return call_step("write", {"filePath": plans[0], "content": "# The plan\n"})

        fixture.set_script(
            [
                call_step("write", {"filePath": "refused.txt", "content": "x"}),
                call_step("bash", {"command": "echo x > shell.txt"}),
                write_the_plan,
                call_step("exit_plan_mode", {}),
            ]
        )
        out = _craze(craze_bin, "new", "--wait", "--json", "--plan", "--provider", "native", "-C", str(workspace), "plan it")
        assert out.returncode == 0, out
        res = json.loads(out.stdout)
        assert (workspace / "shell.txt").exists() and not (workspace / "refused.txt").exists(), res
        assert res["files"] == [] and "outsideFiles" not in res, res
        assert res["planFile"] == str(Path(plans[0]).resolve()), res
        _stop(craze_bin, res["short"])

        fixture.set_script(
            [
                call_step("edit", {"filePath": "notes.txt", "oldString": "not there", "newString": "x"}),
                call_step("edit", {"filePath": "notes.txt", "oldString": "alpha", "newString": "beta"}),
                call_step("write", {"filePath": str(workspace.parent / "o.txt"), "content": "x"}),
                answer("done"),
            ]
        )
        out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "-C", str(workspace), "edit it")
        assert out.returncode == 0, out
        res = json.loads(out.stdout)
        assert res["files"] == ["notes.txt"], res
        assert res["outsideFiles"] == [str(workspace.parent / "o.txt")] and "planFile" not in res, res
        _stop(craze_bin, res["short"])


# --- What a dead host's shell commands come to -------------------------------
#
# A native shell command leads a session of its own, so a SIGKILLed host takes none
# of its commands with it. Each is a real process here, a shell and a child of the
# shell, told apart from anything else by a marker: a leak fails on the marker, and
# whatever a failure leaves is killed by its pid, checked by that marker first.


def _command(name: str, seconds: int) -> tuple[str, str, str]:
    """A shell command that writes its shell's pid and its child's to name.leader and
    name.child and waits; with the markers of each, the child's a sleep's arguments."""
    leader, child = f"craze-reap-{name}-leader", f"sleep {seconds}"
    return f"{child} & echo $! > {name}.child; echo $$ > {name}.leader; wait  # {leader}", leader, child


def _pids_of(work: Path, name: str, proc: subprocess.Popen[bytes] | None = None) -> tuple[int, int]:
    """The shell's and the child's pid, once both have been written."""
    deadline = time.monotonic() + 3 * WAIT
    pids: list[int] = []
    for part in ("leader", "child"):
        path = work / f"{name}.{part}"
        while True:
            text = path.read_text(encoding="utf-8") if path.exists() else ""
            if text.endswith("\n") and text.strip().isdigit():
                pids.append(int(text))
                break
            assert time.monotonic() < deadline, f"the command never wrote {path.name}"
            assert proc is None or proc.poll() is None, ("craze exited before the command ran", proc.communicate())
            time.sleep(0.02)
    return pids[0], pids[1]


def _kill_marked(*procs: tuple[int, str]) -> list[str]:
    """Kill each (pid, marker) still running as itself: what a failed case left."""
    left = []
    for pid, marker in procs:
        if alive(pid, marker):
            left.append(f"{marker} ({pid})")
            try:
                os.kill(pid, signal.SIGKILL)
            except OSError:
                pass
    return left


def _record(host_id: str) -> Path:
    return _home() / ".cache" / "craze" / "host-logs" / f"{host_id}.pgids"


def test_a_waiter_reaps_its_killed_hosts_commands(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """The host SIGKILLed under a running shell command: by the time its waiter has
    reported host_exited, the shell and the shell's child have been signalled and the
    record has gone. No other craze runs here."""
    command, leader_marker, child_marker = _command("fg", 7301)
    leader = child = 0
    with SSEFixture() as fixture:
        fixture.set_script([call_step("bash", {"command": command, "timeout": 600000})])
        waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
        try:
            leader, child = _pids_of(tmp_path / "proj", "fg", waiter)
            assert until_alive(child, child_marker) and alive(leader, leader_marker)
            (entry,) = _entries(_home())
            lines = _record(entry["hostId"]).read_text(encoding="utf-8").splitlines()
            assert [line.split()[:1] for line in lines] == [[str(leader)]] and lines[0].split()[2] == "shell", lines
            os.kill(entry["pid"], signal.SIGKILL)
            stdout, stderr = waiter.communicate(timeout=6 * WAIT)
            assert waiter.returncode == 1 and json.loads(stdout)["error"]["kind"] == "host_exited", (stdout, stderr)
            assert not _record(entry["hostId"]).exists(), "the waiter left the record"
            gone(leader, leader_marker)
            gone(child, child_marker)
        finally:
            if waiter.poll() is None:
                waiter.kill()
                waiter.communicate()
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left


def test_the_next_craze_reaps_a_killed_hosts_commands(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """No waiter attached: the commands run on with their host gone, until the next
    craze that reads the registry, which signals them and removes the record, and
    answers as if nothing had happened."""
    command, leader_marker, child_marker = _command("fg", 7302)
    leader = child = 0
    with SSEFixture() as fixture:
        fixture.set_script([call_step("bash", {"command": command, "timeout": 600000})])
        waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
        try:
            leader, child = _pids_of(tmp_path / "proj", "fg", waiter)
            assert until_alive(child, child_marker)
            (entry,) = _entries(_home())
            waiter.kill()
            waiter.communicate()
            os.kill(entry["pid"], signal.SIGKILL)
            # The host's gone, and nothing has looked: its commands are running.
            deadline = time.monotonic() + WAIT
            while pid_alive(entry["pid"]):
                assert time.monotonic() < deadline, "the SIGKILLed host is still there"
                time.sleep(0.02)
            assert alive(leader, leader_marker) and alive(child, child_marker), "a command went with its host"
            assert _record(entry["hostId"]).exists()
            out = _craze(craze_bin, "ps", "--json")
            assert out.returncode == 0 and out.stderr == b"", out
            assert json.loads(out.stdout)["sessions"] == [], out
            assert not _record(entry["hostId"]).exists(), "ps left the record"
            gone(leader, leader_marker)
            gone(child, child_marker)
        finally:
            if waiter.poll() is None:
                waiter.kill()
                waiter.communicate()
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left


def test_the_next_craze_finds_a_group_whose_shell_has_exited(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """The shell exits after its host was killed, leaving a child running in its group
    with nothing at the number to name it: the next craze finds it by scanning for the
    group, knows it for a command's by what its environment carries, and ends it."""
    command, leader_marker, child_marker = _command("fg", 7306)
    # The shell stays until the test lets it go, not until its child ends.
    command = "mkfifo gate; " + command.replace("; wait  #", "; read line < gate  #")
    leader = child = 0
    with SSEFixture() as fixture:
        fixture.set_script([call_step("bash", {"command": command, "timeout": 600000})])
        waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
        try:
            leader, child = _pids_of(tmp_path / "proj", "fg", waiter)
            assert until_alive(child, child_marker)
            (entry,) = _entries(_home())
            waiter.kill()
            waiter.communicate()
            os.kill(entry["pid"], signal.SIGKILL)
            deadline = time.monotonic() + WAIT
            while pid_alive(entry["pid"]):
                assert time.monotonic() < deadline, "the SIGKILLed host is still there"
                time.sleep(0.02)
            fd = os.open(tmp_path / "proj" / "gate", os.O_WRONLY)
            os.write(fd, b"go\n")
            os.close(fd)
            gone(leader, leader_marker)
            assert alive(child, child_marker), "the child went with its shell"
            assert _craze(craze_bin, "ps", "--json").returncode == 0
            gone(child, child_marker)
            assert not _record(entry["hostId"]).exists(), "ps left the record"
        finally:
            if waiter.poll() is None:
                waiter.kill()
                waiter.communicate()
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left


def _a_dead_hosts_stubborn_command(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, fixture: SSEFixture, seconds: int) -> tuple[dict, int, int, str, str]:
    """A native host SIGKILLed, no waiter, leaving a command that ignores SIGTERM: its
    entry, the command's two pids and their markers."""
    command, leader_marker, child_marker = _command("fg", seconds)
    fixture.set_script([call_step("bash", {"command": "trap '' TERM; " + command, "timeout": 600000})])
    waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
    try:
        leader, child = _pids_of(tmp_path / "proj", "fg", waiter)
        assert until_alive(child, child_marker)
        entry = next(e for e in _entries(_home()) if Path(e["workspace"]).resolve() == (tmp_path / "proj").resolve())
    finally:
        waiter.kill()
        waiter.communicate()
    os.kill(entry["pid"], signal.SIGKILL)
    deadline = time.monotonic() + WAIT
    while pid_alive(entry["pid"]):
        assert time.monotonic() < deadline, "the SIGKILLed host is still there"
        time.sleep(0.02)
    return entry, leader, child, leader_marker, child_marker


def test_a_signal_during_the_join_keeps_the_commands_exit_code(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A waiter prints a failed turn's result, exit 3, and stays for its sweep of an
    unrelated dead host's stubborn command. A SIGTERM then ends it with that 3, one
    object on stdout, not by the signal."""
    _config(fake_agent_bin)
    work = tmp_path / "w"
    work.mkdir()
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(work), "go", script="turnfail")
    assert out.returncode == 3, out
    short = json.loads(out.stdout)["short"]
    leader = child = 0
    leader_marker = child_marker = "never"
    with SSEFixture() as fixture:
        try:
            dead, leader, child, leader_marker, child_marker = _a_dead_hosts_stubborn_command(craze_bin, fake_agent_bin, tmp_path, fixture, 7308)
            proc = subprocess.Popen(
                [str(craze_bin), "wait", short, "--json"],
                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=os.environ.copy(),
            )
            assert proc.stdout is not None
            line = proc.stdout.readline()
            assert json.loads(line)["status"] == "failed", line
            # Past the command's own handlers, which end as it returns, and into the join:
            # the sweep's grace has most of its 2 s to run.
            time.sleep(0.2)
            assert proc.poll() is None, "the wait did not stay for its sweep"
            proc.send_signal(signal.SIGTERM)
            rest, _ = proc.communicate(timeout=6 * WAIT)
            assert proc.returncode == 3 and rest == b"", (proc.returncode, rest)
            # Cut short: no SIGKILL went, and the record stays for the next craze.
            assert _record(dead["hostId"]).exists(), "the sweep finished before the signal"
            assert alive(leader, leader_marker), "the stubborn command had its SIGKILL"
            assert _craze(craze_bin, "ps", "--json").returncode == 0
            gone(leader, leader_marker)
            gone(child, child_marker)
            assert not _record(dead["hostId"]).exists(), "the next craze left the record"
        finally:
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left
    _stop(craze_bin, short)


def test_a_short_command_writes_its_output_then_stays_for_the_sigkill(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A dead host's command that ignores SIGTERM: `ps` writes its answer as it arrives,
    with the process still there and the grace well short of its end, and is still there
    to send the SIGKILL before it exits. The sweep started beside the command, so the
    answer does not wait for it."""
    leader = child = 0
    leader_marker = child_marker = "never"
    with SSEFixture() as fixture:
        try:
            entry, leader, child, leader_marker, child_marker = _a_dead_hosts_stubborn_command(craze_bin, fake_agent_bin, tmp_path, fixture, 7307)
            began = time.monotonic()
            proc = subprocess.Popen(
                [str(craze_bin), "ps", "--json"],
                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=os.environ.copy(),
            )
            assert proc.stdout is not None
            line = proc.stdout.readline()
            took = time.monotonic() - began
            assert json.loads(line)["sessions"] == [], line
            assert proc.poll() is None, "ps had exited by the time its answer was read"
            assert took < 1.0, f"the answer took {took:.2f}s, waiting on the sweep's 2 s grace"
            proc.communicate(timeout=6 * WAIT)
            assert proc.returncode == 0
            # Its exit is the postcondition: both went at the SIGKILL, and the record is gone.
            gone(leader, leader_marker)
            gone(child, child_marker)
            assert not _record(entry["hostId"]).exists(), "ps left the record"
        finally:
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left


def test_no_craze_a_native_command_runs_inherits_its_hosts_mark(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A native command has its host's mark; what that command runs that is not craze
    inherits it. A craze it runs clears it before anything else, so neither a hub and
    the host it starts, nor a `craze prompt`, nor any agent or agent's child of theirs
    carries the first host's mark."""
    dumps = tmp_path / "dumps"
    dumps.mkdir()
    # The agent, as craze runs it: its environment written down, by a child of its own.
    wrapper = tmp_path / "agent.sh"
    wrapper.write_text(f'#!/bin/sh\nsh -c \'env > "{dumps}/agent.$$"\'\nexec "{fake_agent_bin}" "$@"\n', encoding="utf-8")
    wrapper.chmod(0o755)
    inner = tmp_path / "inner"
    inner.mkdir()
    command = (
        f'env > own.env; sh -c \'env > child.env\'; '
        f'"{craze_bin}" new --json --provider cursor -C "{inner}" nested > new.json 2> new.err; '
        f'"{craze_bin}" prompt --provider cursor --workspace "{inner}" nested > prompt.out 2>&1'
    )
    with SSEFixture() as fixture:
        fixture.set_script([call_step("bash", {"command": command, "timeout": 60000}), answer("done")])
        waiter = _native_waiter(craze_bin, wrapper, tmp_path, fixture)
        stdout, stderr = waiter.communicate(timeout=6 * WAIT)
        assert waiter.returncode == 0, (waiter.returncode, stdout, stderr)
    work = tmp_path / "proj"
    outer = json.loads(stdout)
    (entry,) = [e for e in _entries(_home()) if e["crazeSessionId"] == outer["session"]]
    mark = f"CRAZE_HOST_ID={entry['hostId']}"
    # The control: the mark is there to inherit.
    assert mark in (work / "own.env").read_text(encoding="utf-8").splitlines()
    assert mark in (work / "child.env").read_text(encoding="utf-8").splitlines()
    assert (work / "new.json").read_text(encoding="utf-8"), (work / "new.err").read_text(encoding="utf-8")
    deadline = time.monotonic() + 3 * WAIT
    while len(list(dumps.iterdir())) < 2:
        assert time.monotonic() < deadline, ("no agent ran twice", (work / "new.err").read_text(encoding="utf-8"), (work / "prompt.out").read_text(encoding="utf-8"))
        time.sleep(0.05)
    for dump in dumps.iterdir():
        assert "CRAZE_HOST_ID=" not in dump.read_text(encoding="utf-8"), dump
    # Every hub, host and agent still running, the first host and all it led to.
    for pid in marker_pids():
        env = _environ(pid)
        assert env is None or not any(e.startswith(b"CRAZE_HOST_ID=") for e in env), (pid, _argv(pid))


def test_a_healthy_hosts_commands_are_left_alone_and_a_stop_leaves_no_record(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """A sweep on the way into every craze keeps off a running host: its command runs
    on through `ps`, `wait` and a bad `stop`. A clean stop then ends it, as a stop
    always has, and leaves no record."""
    command, leader_marker, child_marker = _command("fg", 7303)
    leader = child = 0
    with SSEFixture() as fixture:
        fixture.set_script([call_step("bash", {"command": command, "timeout": 600000})])
        waiter = _native_waiter(craze_bin, fake_agent_bin, tmp_path, fixture)
        try:
            leader, child = _pids_of(tmp_path / "proj", "fg", waiter)
            assert until_alive(child, child_marker)
            (entry,) = _entries(_home())
            short = entry["crazeSessionId"][-8:]
            assert _craze(craze_bin, "ps", "--json").returncode == 0
            assert _craze(craze_bin, "stop", "nosuchid").returncode != 0
            assert _craze(craze_bin, "wait", short, "--timeout", "1s", "--json").returncode == 5
            assert alive(leader, leader_marker) and alive(child, child_marker), "a sweep touched a running host's command"
            assert _record(entry["hostId"]).exists(), "a sweep removed a running host's record"
            _stop(craze_bin, short)
            waiter.communicate(timeout=6 * WAIT)
            gone(leader, leader_marker)
            gone(child, child_marker)
            assert not _record(entry["hostId"]).exists(), "a clean stop left its record"
        finally:
            if waiter.poll() is None:
                waiter.kill()
                waiter.communicate()
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left


def _background_job(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, fixture: SSEFixture, seconds: int) -> tuple[dict, int, int, str, str]:
    """A plain craze new's interactive session, with a native background job running
    after its turn has ended: its host's entry, the job's two pids and their markers."""
    _config(fake_agent_bin)
    write_native_config(Path(os.environ["CRAZE_HOME"]) / "native", fixture.base_url)
    work = tmp_path / "proj"
    work.mkdir()
    command, leader_marker, child_marker = _command("job", seconds)
    fixture.set_script([call_step("bash", {"command": command, "run_in_background": True}), answer("started it")])
    out = _craze(craze_bin, "new", "--json", "--provider", "native", "-C", str(work), "start it")
    assert out.returncode == 0, out
    leader, child = _pids_of(work, "job")
    assert until_alive(child, child_marker) and alive(leader, leader_marker)
    (entry,) = _entries(_home())
    return entry, leader, child, leader_marker, child_marker


def test_a_killed_hosts_background_job_is_reaped(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A background job outlives its turn, so it has nothing but the record: its group
    goes once the host has been killed, and the next craze finds nothing left to do."""
    leader = child = 0
    leader_marker = child_marker = "never"
    with SSEFixture() as fixture:
        try:
            entry, leader, child, leader_marker, child_marker = _background_job(craze_bin, fake_agent_bin, tmp_path, fixture, 7304)
            os.kill(entry["pid"], signal.SIGKILL)
            out = _craze(craze_bin, "ps", "--json")
            assert out.returncode == 0, out
            gone(leader, leader_marker)
            gone(child, child_marker)
            deadline = time.monotonic() + WAIT
            while _record(entry["hostId"]).exists():
                assert time.monotonic() < deadline, "the record was left"
                time.sleep(0.02)
        finally:
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left


def test_a_clean_stop_ends_a_background_job_and_its_record(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A job left running at a clean stop is ended by it, as it always has been, and
    the host's record goes with it."""
    leader = child = 0
    leader_marker = child_marker = "never"
    with SSEFixture() as fixture:
        try:
            entry, leader, child, leader_marker, child_marker = _background_job(craze_bin, fake_agent_bin, tmp_path, fixture, 7305)
            assert _record(entry["hostId"]).exists()
            _stop(craze_bin, entry["crazeSessionId"][-8:])
            gone(leader, leader_marker)
            gone(child, child_marker)
            assert not _record(entry["hostId"]).exists(), "a clean stop left its record"
        finally:
            left = _kill_marked((leader, leader_marker), (child, child_marker))
    assert not left, left
