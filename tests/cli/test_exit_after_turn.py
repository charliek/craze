"""A session that ends with its turn, and the controls an unattended caller
retries: --exit-after-turn, the unattended idle exit, stop and cancel's --json,
--turn, --session-file and ps --brief, through the built binary over the fake
agent."""

from __future__ import annotations

import json
import os
import re
import signal
import subprocess
import time
from pathlib import Path

from conftest import pid_alive
from test_detach import _entries, _entry_files
from test_journal import journals, lines_of
from test_waiting import WAIT, _config, _craze, _home, _hosts_gone, _one_object, _open_gate, _stop


def _unattended_config(fake_agent_bin: Path, limit: str) -> None:
    """The suite's config with a shorter host_idle_exit_unattended."""
    _config(fake_agent_bin)
    path = Path(os.environ["CRAZE_HOME"]) / "config.toml"
    text = path.read_text(encoding="utf-8")
    path.write_text(re.sub(r'host_idle_exit_unattended = "[^"]*"', f'host_idle_exit_unattended = "{limit}"', text), encoding="utf-8")


def _work(tmp_path: Path) -> Path:
    work = tmp_path / "proj"
    work.mkdir(exist_ok=True)
    return work


def _record(entry: dict) -> dict:
    """The host's stop record: what began its stop, and the turn it found."""
    path = _home() / ".cache" / "craze" / "host-logs" / f"{entry['hostId']}.stop"
    return json.loads(path.read_text(encoding="utf-8"))


def _env(script: str, **extra: str) -> dict[str, str]:
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = script
    env.update(extra)
    return env


class Held:
    """craze new --wait on the fake agent's long turn, held at its gate: the
    waiter, the gate's write end, its host's entry, and the session's short id."""

    def __init__(self, craze_bin: Path, tmp_path: Path, *flags: str, prompts: Path | None = None) -> None:
        self.gate = tmp_path / "gate"
        os.mkfifo(self.gate)
        extra = {"CRAZE_FAKE_GATE": str(self.gate)}
        if prompts is not None:
            extra["CRAZE_FAKE_DUMP_PROMPTS"] = str(prompts)
        self.waiter = subprocess.Popen(
            [str(craze_bin), "new", "--wait", "--json", "--provider", "cursor", "-C", str(_work(tmp_path)), *flags, "a long task"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=_env("long-turn", **extra),
        )
        self.fd = _open_gate(self.gate, self.waiter)
        (self.entry,) = _entries(_home())
        self.short = self.entry["crazeSessionId"][-8:]

    def steps(self, n: int) -> None:
        """Lets n of the long turns' steps through (each turn has two)."""
        os.write(self.fd, b"\x01" * n)

    def result(self) -> tuple[dict, int, bytes]:
        stdout, stderr = self.waiter.communicate(timeout=6 * WAIT)
        return json.loads(stdout), self.waiter.returncode, stderr

    def close(self) -> None:
        os.close(self.fd)
        if self.waiter.poll() is None:
            self.waiter.kill()
            self.waiter.communicate()


def _until(what: str, cond) -> None:
    deadline = time.monotonic() + WAIT
    while not cond():
        assert time.monotonic() < deadline, what
        time.sleep(0.02)


def _hosts_named(short: str) -> list[dict]:
    return [e for e in _entries(_home()) if e.get("crazeSessionId", "").endswith(short)]


# ------------------------------------------------------------- --exit-after-turn


def test_a_completed_turn_leaves_no_host(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """The result, exit 0, says the session is stopping, and by the time the
    command has exited its host is gone: no later look. The cause is on the record
    and the journal, and the session can still be loaded."""
    _config(fake_agent_bin)
    file = tmp_path / "session.json"
    out = _craze(craze_bin, "new", "--wait", "--json", "--exit-after-turn", "--session-file", str(file),
                 "--provider", "cursor", "-C", str(_work(tmp_path)), "go")
    res = _one_object(out)
    assert out.returncode == 0 and out.stderr == b"", out
    assert res["status"] == "completed" and res["stopping"] is True, res
    rec = json.loads(file.read_text(encoding="utf-8"))
    assert not pid_alive(rec["hostPid"]), "the host outlived the command"
    assert not _entries(_home()), "a host is still registered"
    assert _record(rec_entry(rec))["cause"] == "exit_after_turn"
    (journal,) = journals(Path(os.environ["CRAZE_HOME"]))
    stops = [ln for ln in lines_of(journal) if ln.get("kind") == "host_stop"]
    assert stops and stops[-1]["fields"]["stopCause"] == "exit_after_turn", stops
    # The plain footer says so too, and the session is loaded again by name.
    again = _craze(craze_bin, "send", res["short"], "--wait", "--exit-after-turn", "and on", script="load")
    assert again.returncode == 0 and b"session stopping (--exit-after-turn)" in again.stdout, again
    assert not _entries(_home())


def test_the_command_waits_for_a_host_slow_to_go(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """An agent that ignores SIGTERM holds its host's stop for the grace it is given.
    The host is gone by the command's exit all the same, by its own process, and
    no host is a stale registry entry's word for it."""
    _config(fake_agent_bin)
    file = tmp_path / "session.json"
    out = subprocess.run(
        [str(craze_bin), "new", "--wait", "--json", "--exit-after-turn", "--session-file", str(file),
         "--provider", "cursor", "-C", str(_work(tmp_path)), "go"],
        stdin=subprocess.DEVNULL, capture_output=True, timeout=6 * WAIT, env=_env("echo", CRAZE_FAKE_STUBBORN="1"),
    )
    assert out.returncode == 0 and out.stderr == b"" and _one_object(out)["stopping"] is True, out
    assert not pid_alive(json.loads(file.read_text(encoding="utf-8"))["hostPid"]), "the host outlived the command"
    assert not _entries(_home())


def _full_pipe() -> tuple[int, int]:
    """A pipe that holds as much as it can, its read end kept open and never read: any
    result written to it blocks, on every OS's pipe size."""
    r, w = os.pipe()
    os.set_blocking(w, False)
    try:
        while True:
            os.write(w, b"x" * 4096)
    except BlockingIOError:
        pass
    try:
        while True:
            os.write(w, b"x")
    except BlockingIOError:
        pass
    os.set_blocking(w, True)
    return r, w


def test_a_stdout_nobody_reads_pins_nothing(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """The waiter's write blocks on a stdout nobody reads. It has dropped its attachment,
    so the idle backstop still ends the host; a signal then lets it go, with the result's
    exit code."""
    _unattended_config(fake_agent_bin, "1s")
    file = tmp_path / "session.json"
    r, w = _full_pipe()
    waiter = None
    try:
        waiter = subprocess.Popen(
            [str(craze_bin), "new", "--wait", "--json", "--exit-after-turn", "--session-file", str(file),
             "--provider", "cursor", "-C", str(_work(tmp_path)), "go"],
            stdin=subprocess.DEVNULL, stdout=w, stderr=subprocess.PIPE, env=_env("echo"),
        )
        _until("no session file", file.exists)
        pid = json.loads(file.read_text(encoding="utf-8"))["hostPid"]
        host = json.loads(file.read_text(encoding="utf-8"))["hostId"]
        _until("the idle exit did not end the host", lambda: not pid_alive(pid))
        assert waiter.poll() is None, "the write did not block"
        assert _record({"hostId": host})["cause"] == "idle"
        waiter.send_signal(signal.SIGTERM)
        _, err = waiter.communicate(timeout=6 * WAIT)
        assert waiter.returncode == 0 and b"not written whole" in err, err
    finally:
        os.close(r)
        os.close(w)
        if waiter is not None and waiter.poll() is None:
            waiter.kill()
            waiter.communicate()


def test_a_signal_during_a_blocked_write_goes_on_to_the_stop(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """With the idle limit far off: the host is up behind the blocked write, nothing
    attached. A signal gives up on the write and goes on to the stop, the host gone by
    the command's exit, saying exit_after_turn."""
    _config(fake_agent_bin)
    file = tmp_path / "session.json"
    r, w = _full_pipe()
    waiter = None
    try:
        waiter = subprocess.Popen(
            [str(craze_bin), "new", "--wait", "--json", "--exit-after-turn", "--session-file", str(file),
             "--provider", "cursor", "-C", str(_work(tmp_path)), "go"],
            stdin=subprocess.DEVNULL, stdout=w, stderr=subprocess.PIPE, env=_env("turnfail"),
        )
        _until("no handle in the session file", lambda: file.exists() and "handle" in json.loads(file.read_text(encoding="utf-8")))
        rec = json.loads(file.read_text(encoding="utf-8"))

        def detached() -> bool:
            out = _craze(craze_bin, "ps", "--json", "--no-hub")
            sessions = _one_object(out)["sessions"]
            return bool(sessions) and "attached" not in (sessions[0].get("row") or {"attached": 1})

        _until("the waiter's attachment was not dropped", detached)
        assert pid_alive(rec["hostPid"]) and waiter.poll() is None
        waiter.send_signal(signal.SIGTERM)
        _, err = waiter.communicate(timeout=6 * WAIT)
        assert waiter.returncode == 3 and b"not written whole" in err, err
        assert not pid_alive(rec["hostPid"]), "the host outlived the command"
        assert _record(rec_entry(rec))["cause"] == "exit_after_turn"
    finally:
        os.close(r)
        os.close(w)
        if waiter is not None and waiter.poll() is None:
            waiter.kill()
            waiter.communicate()


def rec_entry(rec: dict) -> dict:
    return {"hostId": rec["hostId"]}


def test_a_failed_turn_stops_too(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Exit 3, the turn's own failure unchanged, and no host left."""
    _config(fake_agent_bin)
    out = _craze(craze_bin, "new", "--wait", "--json", "--exit-after-turn", "--provider", "cursor", "-C", str(_work(tmp_path)),
                 "go", script="turnfail")
    res = _one_object(out)
    assert out.returncode == 3 and res["status"] == "failed" and res["stopping"] is True, out
    assert res["error"]["kind"] != "stopped", res
    assert not _entries(_home())


def test_needs_input_abandons_the_ask(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Exit 4: the result says the session is stopping and has no handle to follow,
    the ask cannot be answered, and the host's record names the cause and the turn."""
    _config(fake_agent_bin)
    file = tmp_path / "session.json"
    out = _craze(craze_bin, "new", "--wait", "--json", "--exit-after-turn", "--no-force", "--session-file", str(file),
                 "--provider", "cursor", "-C", str(_work(tmp_path)), "go", script="permission")
    res = _one_object(out)
    assert out.returncode == 4 and res["status"] == "needs_input" and res["stopping"] is True, out
    assert "handle" not in res and res["ask"]["kind"] == "permission", res
    rec = json.loads(file.read_text(encoding="utf-8"))
    assert not pid_alive(rec["hostPid"])
    record = _record(rec_entry(rec))
    assert record["cause"] == "exit_after_turn" and record["turn"] == "turn-1", record
    plain = _craze(craze_bin, "new", "--wait", "--exit-after-turn", "--no-force",
                   "--provider", "cursor", "-C", str(_work(tmp_path)), "go", script="permission")
    assert plain.returncode == 4 and b"the ask is abandoned" in plain.stdout and b"next:" not in plain.stdout, plain


def test_a_signal_is_a_result_and_stops(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """SIGTERM to the waiter cancels its own turn: a result, exit 3, then the stop."""
    _config(fake_agent_bin)
    held = Held(craze_bin, tmp_path, "--exit-after-turn")
    try:
        held.waiter.send_signal(signal.SIGTERM)
        res, code, _ = held.result()
        assert code == 3 and res["status"] == "cancelled" and res["stopping"] is True, res
        assert not pid_alive(held.entry["pid"]), "the host outlived the command"
    finally:
        held.close()


def test_the_result_outlives_a_stdout_that_has_gone(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A stdout whose reader went: the write fails, exit 1, and the stop still goes,
    with the host gone by the command's exit."""
    _config(fake_agent_bin)
    r, w = os.pipe()
    os.close(r)
    file = tmp_path / "session.json"
    try:
        out = subprocess.run(
            [str(craze_bin), "new", "--wait", "--json", "--exit-after-turn", "--session-file", str(file),
             "--provider", "cursor", "-C", str(_work(tmp_path)), "go"],
            stdin=subprocess.DEVNULL, stdout=w, stderr=subprocess.PIPE, env=_env("echo"), timeout=6 * WAIT,
        )
    finally:
        os.close(w)
    assert out.returncode == 1 and b"broken pipe" in out.stderr, out
    assert not pid_alive(json.loads(file.read_text(encoding="utf-8"))["hostPid"])
    assert not _entries(_home())


def test_a_queued_prompt_and_a_second_send_under_the_fence(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A row queued before the fence runs first; prompts after it are refused, whoever
    sends them, kind stopped, while the turn runs; nothing runs past the fenced turn,
    and no host is left. Only once the host has gone does send load the session again."""
    _config(fake_agent_bin)
    prompts = tmp_path / "prompts"
    held = Held(craze_bin, tmp_path, prompts=prompts)
    fenced = None
    try:
        ahead = _craze(craze_bin, "send", held.short, "--json", "ahead", script="long-turn")
        assert _one_object(ahead)["status"] == "queued", ahead
        fenced = subprocess.Popen(
            [str(craze_bin), "send", held.short, "--wait", "--json", "--exit-after-turn", "the last"],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=_env("long-turn"),
        )

        def refused() -> bool:
            out = _craze(craze_bin, "send", held.short, "--json", "too late", script="long-turn")
            if out.returncode == 0:
                # The fenced prompt was not in yet: this one is a row ahead of it.
                return False
            err = _one_object(out)["error"]
            assert out.returncode == 1 and err["kind"] == "stopped" and err["retryable"] is False, out
            assert "takes no other" in err["message"], out
            return True

        # Each pass that was not refused queued a row ahead of the fenced one, each
        # of which then runs: count what the session was handed instead of guessing.
        _until("the second send was never refused", refused)
        waited = _craze(craze_bin, "send", held.short, "--wait", "--json", "too late, waited", script="long-turn")
        assert waited.returncode == 1 and _one_object(waited)["error"]["kind"] == "stopped", waited
        # Enough steps for every turn the queue holds.
        held.steps(64)
        out, err = fenced.communicate(timeout=6 * WAIT)
        res = json.loads(out)
        assert fenced.returncode == 0 and res["status"] == "completed" and res["stopping"] is True, (out, err)
        first, _, _ = held.result()
        assert first["status"] == "completed", first
        assert not pid_alive(held.entry["pid"]), "the host outlived the fenced waiter"
        sent = prompts.read_text(encoding="utf-8")
        assert "the last" in sent and "ahead" in sent and "too late, waited" not in sent, sent
        assert sent.rindex("ahead") < sent.rindex("the last"), sent
    finally:
        held.close()
        if fenced is not None and fenced.poll() is None:
            fenced.kill()
            fenced.communicate()


def test_timeout_leaves_the_session_and_the_fence(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """--timeout's exit 5: nothing stops the session, the fence stays. A later craze
    wait reports the turn and exits, and stops nothing either; craze stop finishes it."""
    _config(fake_agent_bin)
    held = Held(craze_bin, tmp_path, "--exit-after-turn", "--timeout", "500ms")
    try:
        res, code, _ = held.result()
        assert code == 5 and res["status"] == "running" and "stopping" not in res and res["handle"], res
        assert pid_alive(held.entry["pid"]), "--timeout ended the host"
        refused = _craze(craze_bin, "send", held.short, "--json", "another", script="long-turn")
        assert refused.returncode == 1 and _one_object(refused)["error"]["kind"] == "stopped", refused
        held.steps(2)
        out = _craze(craze_bin, "wait", held.short, "--from", res["handle"], "--json")
        later = _one_object(out)
        assert out.returncode == 0 and later["status"] == "completed" and "stopping" not in later, out
        assert pid_alive(held.entry["pid"]), "craze wait stopped the session"
        still = _craze(craze_bin, "send", held.short, "--json", "another", script="long-turn")
        assert still.returncode == 1 and _one_object(still)["error"]["kind"] == "stopped", still
        _stop(craze_bin, held.short)
    finally:
        held.close()


def test_a_killed_waiter_leaves_the_idle_exit_as_the_backstop(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A waiter killed outright sends nothing: the fence holds, and once the turn is
    over the host's own idle exit ends it, saying so."""
    _unattended_config(fake_agent_bin, "1s")
    held = Held(craze_bin, tmp_path, "--exit-after-turn")
    try:
        held.waiter.kill()
        held.waiter.communicate()
        assert pid_alive(held.entry["pid"])
        held.steps(2)
        _until("the host never idled out", lambda: not pid_alive(held.entry["pid"]))
        assert _record(held.entry)["cause"] == "idle"
    finally:
        held.close()


# ------------------------------------------------------------ unattended idle


def test_an_unattended_host_idles_out_on_its_own_limit(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """With no flag: the host new --wait started exits once its waiter has gone, after
    host_idle_exit_unattended and not host_idle_exit (the suite's 30 s)."""
    _unattended_config(fake_agent_bin, "1s")
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(_work(tmp_path)), "go")
    assert out.returncode == 0 and "stopping" not in _one_object(out), out
    (entry,) = _entries(_home())
    _until("the unattended host never idled out", lambda: not pid_alive(entry["pid"]))
    assert _record(entry)["cause"] == "idle"


def test_an_open_ask_does_not_pin_an_unattended_host(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """needs_input under --no-force: nobody is attached once the waiter has gone, and
    the ask does not hold the host past its limit."""
    _unattended_config(fake_agent_bin, "1s")
    out = _craze(craze_bin, "new", "--wait", "--json", "--no-force", "--provider", "cursor", "-C", str(_work(tmp_path)), "go", script="permission")
    assert out.returncode == 4 and _one_object(out)["handle"], out
    (entry,) = _entries(_home())
    _until("the open ask pinned the unattended host", lambda: not pid_alive(entry["pid"]))
    record = _record(entry)
    assert record["cause"] == "idle" and record["turn"] == "turn-1", record


# ------------------------------------------------------------ stop and cancel


def test_stop_json_and_its_exits(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """stopped, then already_ended twice over, plain and --json, exit 0 each; an id
    nobody has, exit 1, kind config."""
    _config(fake_agent_bin)
    first = _one_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(_work(tmp_path)), "go"))
    short, session = first["short"], first["session"]
    out = _craze(craze_bin, "stop", short, "--json")
    assert out.returncode == 0 and _one_object(out) == {"session": session, "short": short, "result": "stopped"}, out
    _hosts_gone()
    out = _craze(craze_bin, "stop", short, "--json")
    assert out.returncode == 0 and _one_object(out) == {"session": session, "short": short, "result": "already_ended"}, out
    out = _craze(craze_bin, "stop", short)
    assert out.returncode == 0 and out.stdout == f"session {short} has already ended\n".encode() and out.stderr == b"", out
    out = _craze(craze_bin, "stop", "0123abcd", "--json")
    body = _one_object(out)
    assert out.returncode == 1 and body["result"] == "not_found" and body["error"]["kind"] == "config", out
    assert b"no session 0123abcd" in out.stderr, out
    # Not an id at all stays the usage error it was.
    assert _craze(craze_bin, "stop", "a/b", "--json").returncode == 2


def test_stop_and_cancel_on_a_killed_hosts_session(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A stale registry entry and an index row that says nothing of a death: the id
    reads as ended, exit 0, both ways, with --json; plain cancel keeps its exit 1."""
    _config(fake_agent_bin)
    res = _one_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(_work(tmp_path)), "go"))
    (entry,) = _entries(_home())
    os.kill(entry["pid"], signal.SIGKILL)
    _until("the killed host is still there", lambda: not pid_alive(entry["pid"]))
    out = _craze(craze_bin, "stop", res["short"], "--json")
    assert out.returncode == 0 and _one_object(out)["result"] == "already_ended", out
    out = _craze(craze_bin, "cancel", res["short"], "--json")
    assert out.returncode == 0 and _one_object(out)["result"] == "already_ended", out
    out = _craze(craze_bin, "cancel", res["short"])
    assert out.returncode == 1 and b"the session has ended" in out.stderr, out


def test_ambiguous_and_an_index_that_cannot_be_read(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Two ended sessions a prefix of both names, exit 1, kind usage; an index that
    cannot be read, exit 1, kind internal: neither reads as not_found."""
    _config(fake_agent_bin)
    index = Path(os.environ["CRAZE_HOME"]) / "sessions.jsonl"
    ids = ["0199c0de-1111-7000-8000-00000000a001", "0199c0de-1111-7000-8000-00000000a002"]
    work = str(_work(tmp_path))
    index.write_text(
        "".join(
            json.dumps({"sessionId": f"s-{i}", "provider": "cursor", "cwd": work, "title": "", "pinned": False,
                        "createdAt": "2026-10-01T00:00:00Z", "updatedAt": "2026-10-01T00:00:00Z", "crazeId": sid}) + "\n"
            for i, sid in enumerate(ids)
        ),
        encoding="utf-8",
    )
    for verb in ("stop", "cancel"):
        out = _craze(craze_bin, verb, ids[0][:-1], "--json")
        body = _one_object(out)
        assert out.returncode == 1 and body["result"] == "ambiguous" and body["error"]["kind"] == "usage", out
        one = _craze(craze_bin, verb, ids[1][-8:], "--json")
        assert one.returncode == 0 and _one_object(one) == {"session": ids[1], "short": ids[1][-8:], "result": "already_ended"}, one
    index.unlink()
    index.mkdir()
    for verb in ("stop", "cancel"):
        out = _craze(craze_bin, verb, ids[0][-8:], "--json")
        body = _one_object(out)
        assert out.returncode == 1 and body["result"] == "index_error" and body["error"]["kind"] == "internal", out


def test_an_unreadable_registry_entry_reads_no_false_success(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A live, prompted host whose registry JSON cannot be read, its inode kept: its
    session has an index row, and neither verb may read that as ended. Exit 1, kind
    internal, plain and --json, the host running on; an id a readable host serves still
    goes through; and with the entry back, stop works."""
    _config(fake_agent_bin)
    res = _one_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(_work(tmp_path)), "go"))
    (entry,) = _entries(_home())
    (path,) = _entry_files(_home())
    good = path.read_bytes()
    with path.open("r+b") as f:
        f.write(b"garbage")
        f.truncate()
    try:
        for verb in ("stop", "cancel"):
            out = _craze(craze_bin, verb, res["session"], "--json")
            body = _one_object(out)
            assert out.returncode == 1 and body["result"] == "index_error" and body["error"]["kind"] == "internal", out
            plain = _craze(craze_bin, verb, res["session"])
            assert plain.returncode == 1 and plain.stdout == b"" and b"cannot be read" in plain.stderr, plain
        assert pid_alive(entry["pid"]), "the host was stopped"
    finally:
        with path.open("r+b") as f:
            f.write(good)
            f.truncate()
    out = _craze(craze_bin, "stop", res["short"], "--json")
    assert out.returncode == 0 and _one_object(out)["result"] == "stopped", out
    _hosts_gone()


def test_two_stops_at_once_are_both_a_success(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A stop racing another, and so its host's exit, reads stopped or already_ended."""
    _config(fake_agent_bin)
    held = Held(craze_bin, tmp_path)
    try:
        procs = [
            subprocess.Popen([str(craze_bin), "stop", held.short, "--json"], stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=_env("long-turn"))
            for _ in range(6)
        ]
        for p in procs:
            out, err = p.communicate(timeout=6 * WAIT)
            assert p.returncode == 0 and err == b"", (out, err)
            assert json.loads(out)["result"] in ("stopped", "already_ended"), out
    finally:
        held.close()


def test_cancel_turn_against_current_finished_and_next(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """--turn is held against the current turn by the host: the next queued turn's id
    and a finished one's cancel nothing, exit 0; the current one's is cancelled, and a
    retry of that same cancel leaves the row that started behind it running."""
    _config(fake_agent_bin)
    held = Held(craze_bin, tmp_path)
    try:
        assert _one_object(_craze(craze_bin, "send", held.short, "--json", "behind", script="long-turn"))["status"] == "queued"
        session = held.entry["crazeSessionId"]
        out = _craze(craze_bin, "cancel", held.short, "--turn", "turn-2", "--json")
        assert out.returncode == 0 and _one_object(out) == {"session": session, "short": held.short, "turn": "turn-2", "result": "turn_not_current"}, out
        plain = _craze(craze_bin, "cancel", held.short, "--turn", "turn-2")
        assert plain.returncode == 0 and plain.stdout == b"turn-2 is not the current turn; nothing was cancelled\n", plain
        out = _craze(craze_bin, "cancel", held.short, "--turn", "turn-1", "--json")
        assert out.returncode == 0 and _one_object(out) == {"session": session, "short": held.short, "turn": "turn-1", "result": "cancelled"}, out
        res, code, _ = held.result()
        assert code == 3 and res["status"] == "cancelled", res

        # turn-2 starts behind it: the retried cancel of turn-1 must not reach it.
        def current() -> str:
            state = _one_object(_craze(craze_bin, "ps", "--json", "--no-hub"))["sessions"][0]["row"]
            return state["activity"]

        _until("the queued turn never started", lambda: current() == "working")
        out = _craze(craze_bin, "cancel", held.short, "--turn", "turn-1", "--json")
        assert out.returncode == 0 and _one_object(out)["result"] == "turn_not_current", out
        assert current() == "working", "the retried cancel reached the next turn"
        out = _craze(craze_bin, "cancel", held.short, "--json")
        assert out.returncode == 0 and _one_object(out)["turn"] == "turn-2", out
        _until("turn-2 is still running", lambda: current() != "working")
        out = _craze(craze_bin, "cancel", held.short, "--json")
        assert out.returncode == 0 and _one_object(out) == {"session": session, "short": held.short, "result": "nothing_to_cancel"}, out
        plain = _craze(craze_bin, "cancel", held.short)
        assert plain.returncode == 0 and plain.stdout == b"nothing to cancel\n", plain
        _stop(craze_bin, held.short)
    finally:
        held.close()


# ------------------------------------------------------------ --session-file


def test_the_session_file_is_there_before_the_turn_ends(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Read while the agent is held: the session and host, then the handle, which
    craze wait takes. A file already there is replaced, 0600."""
    _config(fake_agent_bin)
    file = tmp_path / "session.json"
    file.write_text("what an earlier run left", encoding="utf-8")
    held = Held(craze_bin, tmp_path, "--session-file", str(file))
    try:
        _until("the file has no handle", lambda: file.exists() and "handle" in json.loads(file.read_text(encoding="utf-8")))
        rec = json.loads(file.read_text(encoding="utf-8"))
        assert set(rec) == {"session", "short", "hostPid", "hostId", "handle"}, rec
        assert rec["session"] == held.entry["crazeSessionId"] and rec["short"] == held.short, rec
        assert rec["hostPid"] == held.entry["pid"] and rec["hostId"] == held.entry["hostId"], rec
        assert file.stat().st_mode & 0o777 == 0o600
        assert held.waiter.poll() is None, "the turn was over before the file was read"
        held.steps(2)
        out = _craze(craze_bin, "wait", held.short, "--from", rec["handle"], "--json")
        assert out.returncode == 0 and _one_object(out)["turn"] == "turn-1", out
        res, code, _ = held.result()
        assert code == 0 and res["turn"] == "turn-1", res
        _stop(craze_bin, held.short)
    finally:
        held.close()


def test_send_writes_the_session_file_for_a_live_session(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """A session found running has no ready line: the file is written once attached,
    and a row's handle follows; a send that reloads writes it too."""
    _config(fake_agent_bin)
    held = Held(craze_bin, tmp_path)
    file = tmp_path / "session.json"
    sender = None
    try:
        sender = subprocess.Popen(
            [str(craze_bin), "send", held.short, "--wait", "--json", "--session-file", str(file), "behind"],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=_env("long-turn"),
        )
        _until("the file has no handle", lambda: file.exists() and "handle" in json.loads(file.read_text(encoding="utf-8")))
        rec = json.loads(file.read_text(encoding="utf-8"))
        assert rec["hostId"] == held.entry["hostId"] and ".row-" in rec["handle"], rec
        assert sender.poll() is None
        held.steps(4)
        out, err = sender.communicate(timeout=6 * WAIT)
        assert sender.returncode == 0 and json.loads(out)["status"] == "completed", (out, err)
        held.result()
        _stop(craze_bin, held.short)
        out = _craze(craze_bin, "send", held.short, "--wait", "--json", "--exit-after-turn", "--session-file", str(file), "again", script="load")
        assert out.returncode == 0, out
        rec2 = json.loads(file.read_text(encoding="utf-8"))
        assert rec2["session"] == rec["session"] and rec2["hostId"] != rec["hostId"] and ".turn-" in rec2["handle"], rec2
        assert not pid_alive(rec2["hostPid"])
    finally:
        held.close()
        if sender is not None and sender.poll() is None:
            sender.kill()
            sender.communicate()


def test_a_first_write_that_fails_leaves_no_host(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Exit 1, kind config, before the prompt, the session it started stopped."""
    _config(fake_agent_bin)
    prompts = tmp_path / "prompts"
    blocked = tmp_path / "a-file"
    blocked.write_text("", encoding="utf-8")
    for bad in (blocked / "under-a-file", tmp_path):
        out = subprocess.run(
            [str(craze_bin), "new", "--wait", "--json", "--session-file", str(bad), "--provider", "cursor", "-C", str(_work(tmp_path)), "go"],
            stdin=subprocess.DEVNULL, capture_output=True, timeout=6 * WAIT, env=_env("echo", CRAZE_FAKE_DUMP_PROMPTS=str(prompts)),
        )
        err = _one_object(out)["error"]
        assert out.returncode == 1 and err["kind"] == "config" and "--session-file" in err["message"], out
        assert "the session it started was stopped" in err["message"], out
        assert not _entries(_home()), "a host was left"
        assert not prompts.exists() or prompts.read_text(encoding="utf-8") == "", "the prompt was sent"


def test_a_start_that_fails_leaves_no_session_file(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """What an earlier run left is gone as the command starts, so a failure before the
    ready line names no old session; so does a usage error leave it alone."""
    _config(fake_agent_bin)
    file = tmp_path / "session.json"
    file.write_text("what an earlier run left", encoding="utf-8")
    out = _craze(craze_bin, "new", "--wait", "--session-file", str(file), "--provider", "cursor", "--bad-flag", "go")
    assert out.returncode == 2 and file.exists(), out
    out = _craze(craze_bin, "new", "--wait", "--json", "--session-file", str(file), "--provider", "cursor", "-C", str(tmp_path / "gone"), "go")
    assert out.returncode == 1 and _one_object(out)["error"]["kind"] == "workspace", out
    assert not file.exists(), "a failed start left a file naming another session"


def test_flags_that_need_wait(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    _config(fake_agent_bin)
    for args in (("new", "--exit-after-turn", "go"), ("new", "--session-file", "f", "go"), ("send", "abcd1234", "--exit-after-turn", "go"),
                 ("send", "abcd1234", "--session-file", "f", "go"), ("new", "--wait", "--session-file", "", "go")):
        out = _craze(craze_bin, *args, "--json")
        assert out.returncode == 2 and _one_object(out)["error"]["kind"] == "usage", out


# ------------------------------------------------------------------ ps --brief


def _strip(roster: dict) -> dict:
    """The roster with what may differ between two reads gone."""
    for key in ("epoch", "cursor"):
        roster.pop(key, None)
    for row in roster["sessions"]:
        row.pop("approximate", None)
        host = row.get("row") or {}
        host.pop("since", None)
    return roster


def test_ps_brief_is_the_same_rows_without_catalogs(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Both of ps's paths, the hub's and a host's: no catalogs anywhere, and otherwise
    what plain --json says. --brief needs --json, and does not go with --saved."""
    _config(fake_agent_bin)
    res = _one_object(_craze(craze_bin, "new", "--wait", "--json", "--provider", "cursor", "-C", str(_work(tmp_path)), "go"))
    for flags in (("--no-hub",), ()):
        whole = _one_object(_craze(craze_bin, "ps", "--json", *flags))
        brief_out = _craze(craze_bin, "ps", "--json", "--brief", *flags)
        assert brief_out.returncode == 0 and b"catalogs" not in brief_out.stdout, brief_out
        brief = _one_object(brief_out)
        row = whole["sessions"][0]["row"]
        assert "catalogs" in row, row
        del row["catalogs"]
        assert _strip(whole) == _strip(brief), (whole, brief)
    out = _craze(craze_bin, "ps", "--brief")
    assert out.returncode == 2 and b"--brief needs --json" in out.stderr, out
    out = _craze(craze_bin, "ps", "--json", "--brief", "--saved")
    assert out.returncode == 2 and _one_object(out)["error"]["kind"] == "usage", out
    _stop(craze_bin, res["short"])
