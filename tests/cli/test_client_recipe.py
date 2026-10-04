"""The hermetic recipe: a client tested against a real hub (plan 036 §3.7).

This file is the executable form of protocol.md's "Testing a client against a
real hub" (under "Fixtures and the fake host"): the same environment, the same
config.toml, the same JSON lines in the same order, and the same answers. A
change to one is a change to the other.

It drives `bin/craze bridge --hub` as a raw JSON-RPC client -- its stdin and
stdout are pipes, as an SSH exec's are -- through the small NDJSON reader
below. The reader tells replies (by `id`) from notifications and keeps both,
so no wait depends on the order in which the two interleave. The hub is the
real one, spawned by the first bridge. Beside it run one `craze-fake-host`,
listed in the registry as a craze host is, and the session the hub creates:
a real `craze serve` whose grok is the fake agent. Every wait is bounded and
waits on its own precondition: a ready line, a reply's id, a notification.
The one poll, for the recent directories, is a bounded poll of the method
itself.
"""

from __future__ import annotations

import json
import os
import select
import signal
import subprocess
import time
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import IO, Any, TypeVar

import pytest

import conftest

T = TypeVar("T")

WAIT = 10.0
# The first bridge spawns the hub (hub.Ensure) before its hello is answered.
HUB_START_WAIT = 3 * WAIT
# Longer than the hub's own bounds on a create (the session's start, 60 s,
# then the first prompt's answer, 15 s): a start that hangs is reported by the
# hub's refusal, not by this wait.
CREATE_WAIT = 90.0
# The host writes the session's index row after its start, so the recent
# directory can follow the create's answer (plan 036 §3.7 step 9).
RECENT_WAIT = 5.0

FAKE_HOST_ID = "0a0a0a0a0a0a"
FAKE_SESSION_ID = "recipe-fake"
HELLO = {"protocols": [1], "client": {"kind": "test", "name": "recipe"}}


def recipe_env(craze_bin: Path) -> dict[str, str]:
    """The recipe's environment: six variables and nothing inherited.

    It is built from nothing, as protocol.md's `clean` wrapper runs `env -i`.
    Whatever a bridge inherits reaches the hub, whose environment is fixed the
    moment the first bridge spawns it. Through the hub it reaches every host
    the hub creates and every agent those hosts start: the hub hands on PATH,
    CRAZE_FAKE_* and the API keys (internal/hub/env.go). So nothing is
    inherited, and the developer's shell cannot change an answer:

    - HOME, CRAZE_HOME and CRAZE_RUNTIME_DIR are conftest.isolate_run_env's.
      HOME holds the registry the hub lists hosts from, CRAZE_HOME the craze
      directory (config.toml, sessions.jsonl, and native's keys: none), and
      CRAZE_RUNTIME_DIR the socket base, kept short under /tmp because a
      socket's path is capped at 100 bytes. The last is also the marker that
      conftest.host_cleanup finds this test's processes by.
    - PATH is craze's own directory, then /usr/bin:/bin. cursor-agent, grok
      and gx (often installed on a developer's machine) are then not found,
      so cursor is `unavailable`, "not found", on both OSes, because a missing
      binary is reported ahead of the macOS login-session rule. gx is not
      listed. grok reaches the fake agent only through config.toml's
      [agents], the one route a hub's host takes, since the hub strips
      CRAZE_AGENT_BIN and CRAZE_PROVIDER.
    - CRAZE_FAKE_SCRIPT=grok-echo has the fake agent speak grok's dialect and
      echo each prompt. CRAZE_FAKE_SESSION_ID={dir} has it name its session
      after its working directory, the session's own. No other CRAZE_FAKE_*
      is present to change the fake's behaviour, and no API key is present,
      so native has none: `needs_setup`.
    """
    return {
        "HOME": os.environ["HOME"],
        "CRAZE_HOME": os.environ["CRAZE_HOME"],
        "CRAZE_RUNTIME_DIR": os.environ["CRAZE_RUNTIME_DIR"],
        "PATH": f"{craze_bin.parent}:/usr/bin:/bin",
        "CRAZE_FAKE_SCRIPT": "grok-echo",
        "CRAZE_FAKE_SESSION_ID": "{dir}",
    }


class Lines:
    """Newline-terminated lines off one pipe, each read bounded.

    It reads raw chunks with select() and os.read() against a deadline, never
    a buffered readline(), which can hang on a partial line
    (test_bridge._recv_line's technique). Bytes past a line's newline are kept
    for the next call.
    """

    def __init__(self, pipe: IO[bytes] | None) -> None:
        assert pipe is not None
        self._fd = pipe.fileno()
        self._buf = b""

    def line(self, deadline: float, what: str) -> bytes | None:
        """The next line, without its newline, or None at EOF."""
        while b"\n" not in self._buf:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not select.select([self._fd], [], [], remaining)[0]:
                raise AssertionError(f"timed out waiting for {what}")
            chunk = os.read(self._fd, 65536)
            if not chunk:
                if self._buf:
                    raise AssertionError(f"EOF after a partial line, waiting for {what}: {self._buf[:200]!r}")
                return None
            self._buf += chunk
        line, _, self._buf = self._buf.partition(b"\n")
        return line


class Bridge:
    """One `craze bridge --hub`, driven as a raw JSON-RPC client.

    Its stdin and stdout are pipes. Its stderr goes to a file, because a pipe
    nobody reads could fill and stall it. Every line it writes is one JSON
    object: a reply carries the `id` of the request it answers, and a
    notification has none. A read files each line it takes, a reply by its id
    and a notification in `notes`, in order. `reply` reads until the reply it
    names has been filed, and `wait_for` reads until the notifications held
    satisfy it, so nothing read on the way is dropped.
    """

    def __init__(self, craze_bin: Path, env: dict[str, str], stderr: Path) -> None:
        self.stderr = stderr
        with stderr.open("wb") as err:
            self.proc = subprocess.Popen(
                [str(craze_bin), "bridge", "--hub"],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=err,
                env=env,
            )
        self._lines = Lines(self.proc.stdout)
        self._replies: dict[Any, dict] = {}
        self.notes: list[dict] = []

    def send(self, msg: dict) -> None:
        """One line on the bridge's stdin, encoded compactly, as protocol.md
        writes the recipe's lines."""
        assert self.proc.stdin is not None
        self.proc.stdin.write(json.dumps(msg, separators=(",", ":")).encode() + b"\n")
        self.proc.stdin.flush()

    def call(self, id_: Any, method: str, params: dict, timeout: float = WAIT) -> dict:
        """Send one request and return its result. The test fails on an
        error."""
        self.send({"jsonrpc": "2.0", "id": id_, "method": method, "params": params})
        msg = self.reply(id_, timeout)
        assert "error" not in msg, f"{method} was refused: {msg}"
        return msg["result"]

    def reply(self, id_: Any, timeout: float = WAIT) -> dict:
        """The reply to request id_, whole: its result or its error."""
        deadline = time.monotonic() + timeout
        while id_ not in self._replies:
            self._take(deadline, f"the reply to request {id_!r}")
        return self._replies.pop(id_)

    def wait_for(self, find: Callable[[list[dict]], T | None], what: str, timeout: float = WAIT) -> T:
        """find's answer over the notifications held, once it is not None,
        reading more until it is."""
        deadline = time.monotonic() + timeout
        while (found := find(self.notes)) is None:
            self._take(deadline, what)
        return found

    def close(self, timeout: float = WAIT) -> int:
        """Close stdin, read what is left up to the bridge's EOF, and return
        its exit status.

        Closing stdin is the client's end. The hub, or the host at the other
        end of a splice, closes the connection once nothing is owed, and the
        bridge then exits.
        """
        deadline = time.monotonic() + timeout
        self.close_stdin()
        while self._take(deadline, "the bridge's EOF", eof_ok=True):
            pass
        return self.proc.wait(timeout=max(0.1, deadline - time.monotonic()))

    def close_stdin(self) -> None:
        assert self.proc.stdin is not None
        try:
            self.proc.stdin.close()
        except BrokenPipeError:
            pass

    def _take(self, deadline: float, what: str, eof_ok: bool = False) -> bool:
        """Read one line and file it: False at EOF, which fails the test
        unless eof_ok."""
        try:
            line = self._lines.line(deadline, what)
        except AssertionError as e:
            raise AssertionError(f"{e}; the bridge's stderr: {self._stderr()!r}") from None
        if line is None:
            if eof_ok:
                return False
            raise AssertionError(f"the bridge closed its stdout, waiting for {what}; its stderr: {self._stderr()!r}")
        msg = json.loads(line)
        if "id" in msg:
            self._replies[msg["id"]] = msg
        else:
            self.notes.append(msg)
        return True

    def _stderr(self) -> str:
        return self.stderr.read_text(encoding="utf-8", errors="replace")[-2000:]


class FakeHost:
    """One `craze-fake-host`, its stdin on a pipe: its NDJSON ops go there,
    and its stdin's end ends it, as `quit` does."""

    def __init__(self, fake_host_bin: Path, env: dict[str, str], args: list[str], stderr: Path) -> None:
        with stderr.open("wb") as err:
            self.proc = subprocess.Popen(
                [str(fake_host_bin), *args],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=err,
                env=env,
            )
        line = Lines(self.proc.stdout).line(time.monotonic() + WAIT, "craze-fake-host's ready line")
        assert line is not None, f"craze-fake-host exited before its ready line: {stderr.read_text(encoding='utf-8')!r}"
        # Its socket is bound and its registry entry written before it
        # prints this line.
        self.ready: dict = json.loads(line)

    def op(self, op: dict) -> None:
        assert self.proc.stdin is not None
        self.proc.stdin.write(json.dumps(op).encode() + b"\n")
        self.proc.stdin.flush()

    def close_stdin(self) -> None:
        assert self.proc.stdin is not None
        try:
            self.proc.stdin.close()
        except BrokenPipeError:
            pass


class Procs:
    """The processes one recipe starts, so that its fixture can end them."""

    def __init__(self, craze_bin: Path, fake_host_bin: Path, tmp_path: Path) -> None:
        self._craze_bin = craze_bin
        self._fake_host_bin = fake_host_bin
        self._tmp_path = tmp_path
        self.fake_hosts: list[FakeHost] = []
        self.bridges: list[Bridge] = []

    def fake_host(self, env: dict[str, str], *args: str) -> FakeHost:
        stderr = self._tmp_path / f"fake-host-{len(self.fake_hosts) + 1}.err"
        host = FakeHost(self._fake_host_bin, env, list(args), stderr)
        self.fake_hosts.append(host)
        return host

    def bridge(self, env: dict[str, str]) -> Bridge:
        stderr = self._tmp_path / f"bridge-{len(self.bridges) + 1}.err"
        bridge = Bridge(self._craze_bin, env, stderr)
        self.bridges.append(bridge)
        return bridge

    def end(self) -> None:
        """Close every fake host's stdin and every bridge's, then wait for
        each, bounded. One that has not exited by then is killed: a fake host
        killed so leaves its registry entry, which host_cleanup then
        reports."""
        for host in self.fake_hosts:
            host.close_stdin()
        for bridge in self.bridges:
            bridge.close_stdin()
        for proc in [h.proc for h in self.fake_hosts] + [b.proc for b in self.bridges]:
            try:
                proc.wait(timeout=WAIT)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=WAIT)


@pytest.fixture
def recipe_procs(host_cleanup: None, craze_bin: Path, craze_fake_host_bin: Path, tmp_path: Path) -> Iterator[Procs]:
    """The recipe's processes, ended before conftest's cleanup looks for any.

    This fixture names host_cleanup as a parameter, so pytest always sets
    host_cleanup up first and tears it down last. That makes the order
    structural, not an accident of how autouse fixtures happen to be ordered:
    this teardown closes each fake host's stdin before host_cleanup's scan
    runs. The order matters because stdin is how a fake host is told to go.
    It does not handle SIGTERM, so host_cleanup's SIGTERM would kill it before
    it unlists itself, and the registry entry it leaves fails the test.
    """
    procs = Procs(craze_bin, craze_fake_host_bin, tmp_path)
    yield procs
    procs.end()


def _upsert_of(notes: list[dict], session_id: str) -> dict | None:
    """The first roster upsert naming session_id."""
    for note in notes:
        if note.get("method") == "roster":
            for row in note["params"]["upserts"]:
                if row["sessionId"] == session_id:
                    return row
    return None


def _echoed(attached: dict, notes: list[dict]) -> bool | None:
    """True once the session's reply reads `echo: hello recipe`, as a client
    folds it: the attach snapshot's assistant entries, then each later `text`
    event. The fake agent sends the echo as two chunks, so a snapshot taken
    mid-turn can hold the first and an event the second."""
    snapshot = attached.get("snapshot") or {}
    text = "".join(e.get("text", "") for e in snapshot.get("main", {}).get("entries", []) if e.get("kind") == "assistant")
    for note in notes:
        if note.get("method") == "event" and note["params"]["event"].get("type") == "text":
            text += note["params"]["event"]["text"]
    return True if "echo: hello recipe" in text else None


def _session_closed(notes: list[dict]) -> dict | None:
    return next((n for n in notes if n.get("method") == "reset" and n["params"]["reason"] == "session_closed"), None)


def test_a_client_against_a_real_hub(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, recipe_procs: Procs
) -> None:
    """protocol.md's recipe, step by step: what a create sheet reads from a
    real hub (sessions.createOptions), the roster with a fake host in it, a
    session created through the hub and read back through its splice, the
    recent directory the session leaves, and a cleanup that leaves nothing."""
    # 1. The environment, fixed before the first bridge spawns the hub.
    env = recipe_env(craze_bin)

    # 2. config.toml: the default provider, the hosts' idle exit (the suite's
    # TEST_HOST_IDLE_EXIT), and grok pointed at the fake agent.
    craze_home = Path(env["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    config = craze_home / "config.toml"
    config.write_text(
        f'provider = "grok"\nhost_idle_exit = "30s"\n\n[agents]\ngrok = "{fake_agent_bin}"\n', encoding="utf-8"
    )

    # 3. A fake host, listed in the registry under HOME.
    fake = recipe_procs.fake_host(
        env, "--registry", env["HOME"], "--host-id", FAKE_HOST_ID, "--session-id", FAKE_SESSION_ID
    )
    assert (fake.ready["hostId"], fake.ready["sessionId"]) == (FAKE_HOST_ID, FAKE_SESSION_ID), fake.ready

    # 4. Bridge A's hello: the hub's, which serves what a client needs.
    a = recipe_procs.bridge(env)
    hello = a.call(1, "hello", HELLO, timeout=HUB_START_WAIT)
    assert hello["endpoint"]["kind"] == "hub", hello
    caps = hello["capabilities"]
    assert [c for c in ("rosterSubscribe", "sessionCreate", "connect", "createOptions") if caps.get(c) is not True] == [], caps
    hub_pid = hello["endpoint"]["pid"]

    # 5. What a create can start, exactly.
    options = a.call(2, "sessions.createOptions", {})
    assert options == {
        "providers": [
            {
                "id": "cursor",
                "label": "cursor",
                "state": "unavailable",
                "reason": "cursor-agent not found on PATH",
                "fix": f"install cursor-agent, or set [agents].cursor in {config}",
            },
            {"id": "grok", "label": "grok", "state": "ready"},
            {
                "id": "native",
                "label": "native",
                "state": "needs_setup",
                "reason": "no model provider has a key",
                "fix": 'craze auth login (an API key, or "craze auth login chatgpt" for a ChatGPT plan)',
            },
        ],
        "defaultProvider": "grok",
        "recentDirs": [],
    }, options

    # 6. The roster: the fake host's row, and nothing else yet.
    sub = a.call(3, "sessions.subscribe", {})
    assert [(r["hostId"], r["sessionId"]) for r in sub["sessions"]] == [(FAKE_HOST_ID, FAKE_SESSION_ID)], sub

    # 7. A session created with the default provider, its first prompt taken;
    # the roster subscription is told of it.
    work = tmp_path / "work"
    work.mkdir()
    created = a.call(
        4, "session.create", {"cwd": str(work), "prompt": "hello recipe", "requestId": "recipe-1"}, timeout=CREATE_WAIT
    )
    assert created["prompt"] == "accepted", created
    host_id, session_id = created["session"]["hostId"], created["session"]["sessionId"]
    assert created["session"]["host"]["provider"] == "grok", created
    a.wait_for(lambda notes: _upsert_of(notes, session_id), f"a roster upsert naming {session_id}")

    # 8. Read it back through the hub: hello, the splice, the host's hello,
    # and an attach whose snapshot (or a later event) carries the echo.
    b = recipe_procs.bridge(env)
    assert b.call(1, "hello", HELLO)["endpoint"]["kind"] == "hub"
    assert b.call(2, "session.connect", {"sessionId": host_id}) == {}
    host_hello = b.call(3, "hello", HELLO)
    assert (host_hello["endpoint"]["kind"], host_hello["endpoint"]["hostId"]) == ("host", host_id), host_hello
    attached = b.call(4, "session.attach", {"sessionId": session_id})
    b.wait_for(lambda notes: _echoed(attached, notes), "echo: hello recipe in the snapshot or an event")

    # 9. The session's directory, once its host has written its index row: a
    # bounded poll of the method.
    deadline = time.monotonic() + RECENT_WAIT
    req = 5
    while not (dirs := a.call(req, "sessions.createOptions", {})["recentDirs"]) and time.monotonic() < deadline:
        req += 1
        time.sleep(0.1)
    assert dirs, f"no recent directory within {RECENT_WAIT:g}s"
    assert os.path.realpath(dirs[0]["dir"]) == os.path.realpath(work), dirs

    # 10. Cleanup: stop the session (its host closes bridge B's connection),
    # quit the fake host, close bridge A, and stop the hub, whose SIGTERM is
    # its clean teardown. Then nothing of the recipe's is left, before
    # host_cleanup looks.
    assert b.call(5, "session.stop", {"sessionId": session_id, "commandId": "1"}) == {}
    b.wait_for(_session_closed, "reset{session_closed}")
    assert b.close() == 0, b.stderr.read_text(encoding="utf-8")
    fake.op({"name": "quit"})
    assert fake.proc.wait(timeout=WAIT) == 0
    assert a.close() == 0, a.stderr.read_text(encoding="utf-8")
    os.kill(hub_pid, signal.SIGTERM)
    hubs = Path(env["HOME"]) / ".cache" / "craze" / "hubs"
    deadline = time.monotonic() + WAIT
    while True:
        left = conftest._stray_processes(str(fake_agent_bin))
        left += [str(p) for p in conftest._registry_files(tmp_path)] + [str(p) for p in hubs.glob("*.json")]
        if not left or time.monotonic() >= deadline:
            break
        time.sleep(0.05)
    assert not left, f"the recipe's cleanup left: {left}"
