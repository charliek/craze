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

from conftest import PS_TIMEOUT, REMOVED_CONFIG_ENV, TEST_HOST_IDLE_EXIT, _argv, pid_alive
from sse_fixture import SSEFixture, Step, answer, call_step, write_native_config
from test_detach import _entries

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
