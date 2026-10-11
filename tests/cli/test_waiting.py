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

import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

from conftest import TEST_HOST_IDLE_EXIT, _argv
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
