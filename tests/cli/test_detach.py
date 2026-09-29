"""The detached host (plan 030 C7, §3.3-§3.6, AC2-AC4): a session that
outlives its terminal.

Every case here runs the ordinary craze -- no CRAZE_DETACH=0 -- so the session
lives in a `craze serve` host of its own and the TUI is one client of it. The
terminals are real ptys that own a controlling session (setctty), so closing
the master hangs craze up the way a closed tab does. Each step has a bound of
its own; conftest's host_cleanup stops whatever host a case leaves and fails
the case if one will not go.
"""

from __future__ import annotations

import json
import time
from datetime import datetime
from pathlib import Path

import pytest
from conftest import pid_alive, seed_host_idle_exit
from test_tui import _ANSI, PTYCraze, _wait_fake_gone, _wait_output

WAIT = 10.0


@pytest.fixture(autouse=True)
def _detached(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """The ordinary, detached craze: conftest's opt-out is lifted, and the
    suite's short idle exit seeded in HOME (a case may write its own)."""
    monkeypatch.delenv("CRAZE_DETACH", raising=False)
    seed_host_idle_exit(tmp_path)


def _tui(craze_bin: Path, fake_agent_bin: Path, workspace: Path, **kwargs) -> PTYCraze:
    """craze over the fake agent in workspace, in a terminal it can be hung up
    from."""
    return PTYCraze(craze_bin, fake_agent_bin, workspace, setctty=True, **kwargs)


def _hosts(home: Path) -> Path:
    return home / ".cache" / "craze" / "hosts"


def _entry_files(home: Path) -> list[Path]:
    """Every registry entry file under home, parsed or not."""
    return sorted(_hosts(home).glob("*.json"))


def _entries(home: Path) -> list[dict]:
    """Every registry entry under home. A host writes its entry by rename, so
    a file there that does not parse is never one half-written: it is a
    failure, not something to pass over (sol r12-c7). One unlinked between
    the listing and the read is gone, and skipped."""
    out = []
    for path in _entry_files(home):
        try:
            text = path.read_text(encoding="utf-8")
        except FileNotFoundError:
            continue
        try:
            out.append(json.loads(text))
        except ValueError as e:
            raise AssertionError(f"registry entry {path.name} does not parse ({e}): {text[:200]!r}") from None
    return out


def _ready_entry(home: Path, timeout: float = WAIT) -> dict:
    """The one host's ready registry entry, once it is."""
    deadline = time.monotonic() + timeout
    seen: list[dict] = []
    while time.monotonic() < deadline:
        seen = _entries(home)
        if len(seen) == 1 and seen[0].get("ready") and seen[0].get("crazeSessionId"):
            return seen[0]
        time.sleep(0.05)
    raise AssertionError(f"no single ready host in the registry: {seen}")


def _wait_host_gone(home: Path, pid: int, timeout: float) -> None:
    """The host's process gone and no registry entry file left -- whatever
    one would hold -- each within the time left of one bound."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not pid_alive(pid) and not _entry_files(home):
            return
        time.sleep(0.05)
    raise AssertionError(
        f"host {pid} alive={pid_alive(pid)}, registry files={[p.name for p in _entry_files(home)]} after {timeout}s"
    )


def _index_row(home: Path, entry: dict) -> dict | None:
    """The session's row in the index, or None when it has none (the engine
    writes it at the first prompt, so a stopped or expired session that had
    one stays resumable).

    The index is parsed, not searched (sol r12-c7): every line must be a JSON
    row -- the index is rewritten whole, by rename, so none is ever
    half-written -- and the session's (its crazeId the host's
    crazeSessionId) must be one whole row: the provider session it loads
    (the host's own, when the entry names it), the provider, the directory it
    ran in, and both times.
    """
    path = home / ".craze" / "sessions.jsonl"
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        return None
    rows = []
    for n, line in enumerate(lines, 1):
        try:
            rows.append(json.loads(line))
        except ValueError as e:
            raise AssertionError(f"{path.name}:{n} is not a JSON row ({e}): {line[:200]!r}") from None
    mine = [r for r in rows if isinstance(r, dict) and r.get("crazeId") == entry["crazeSessionId"]]
    if not mine:
        return None
    assert len(mine) == 1, f"{len(mine)} index rows for one session: {mine}"
    row = mine[0]
    assert isinstance(row.get("sessionId"), str) and row["sessionId"], row
    if entry.get("providerSessionId"):
        assert row["sessionId"] == entry["providerSessionId"], (row, entry)
    assert row.get("provider") == "cursor", row
    assert isinstance(row.get("cwd"), str) and Path(row["cwd"]).resolve() == home.resolve(), row
    for key in ("createdAt", "updatedAt"):
        assert isinstance(row.get(key), str), row
        datetime.fromisoformat(row[key])
    return row


def _prompt(tui: PTYCraze, text: str) -> None:
    mark = tui.mark()
    tui.write(text.encode() + b"\r")
    tui.wait_contains_since(f"echo: {text}", mark, timeout=WAIT)


def test_hangup_leaves_the_host_and_dash_c_reattaches(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """AC2: closing the terminal is a detach. The host stays listed, its
    process alive and its agent running; `craze -c` in a new terminal attaches
    to that same host -- not a new one -- and shows the transcript."""
    with _tui(craze_bin, fake_agent_bin, tmp_path) as first:
        first.wait_contains("cursor", timeout=WAIT)
        _prompt(first, "before the hangup")
        entry = _ready_entry(tmp_path)
        assert pid_alive(entry["pid"])
        first.hangup(timeout=5)

    # The host's survival is a fact that has to hold for a moment, not an
    # event to wait for; half a second is far past the client's own teardown.
    time.sleep(0.5)
    assert pid_alive(entry["pid"]), "the host died with its terminal"
    assert [e["hostId"] for e in _entries(tmp_path)] == [entry["hostId"]]

    with _tui(craze_bin, fake_agent_bin, tmp_path, extra_args=["-c"]) as second:
        second.wait_contains("echo: before the hangup", timeout=WAIT)
        _prompt(second, "after the reattach")
        assert [e["hostId"] for e in _entries(tmp_path)] == [entry["hostId"]]
        assert entry["pid"] == _ready_entry(tmp_path)["pid"]
        second.write(b"/exit\r")
        assert second.wait_exit(timeout=5) == 0, second.screen()[-3000:]
    _wait_host_gone(tmp_path, entry["pid"], 5)
    _wait_fake_gone(fake_agent_bin, timeout=5)


@pytest.mark.parametrize("quit_keys", [b"/exit\r", b"\x04"], ids=["slash-exit", "ctrl-d"])
def test_quit_ends_the_session(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, quit_keys: bytes
) -> None:
    """AC3: `/exit` (and ctrl+d, the same quit path) stops the host. The
    client is back within 2 s, the host and its registry entry are gone within
    5 s, the fake agent with them, and the session's index row remains."""
    with _tui(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        _prompt(tui, "one turn")
        entry = _ready_entry(tmp_path)
        tui.write(quit_keys)
        assert tui.wait_exit(timeout=2) == 0, tui.screen()[-3000:]
        _wait_host_gone(tmp_path, entry["pid"], 5)
    _wait_fake_gone(fake_agent_bin, timeout=5)
    assert _index_row(tmp_path, entry) is not None, "the stopped session left no index row"


def test_idle_host_exits_and_keeps_its_row(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """AC4: with host_idle_exit = "2s", a host nobody is attached to exits by
    itself. The startup grace is over once a client has attached (idle.go), so
    no test knob is needed; the wait is the limit plus the watcher's 1 s tick
    plus the stop. A hung-up terminal is what leaves it unattended, and the
    session is saved and resumable, not lost."""
    config = tmp_path / ".craze" / "config.toml"
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text('host_idle_exit = "2s"\n', encoding="utf-8")
    with _tui(craze_bin, fake_agent_bin, tmp_path) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        _prompt(tui, "then walk away")
        entry = _ready_entry(tmp_path)
        tui.hangup(timeout=5)
    _wait_host_gone(tmp_path, entry["pid"], 12)
    _wait_fake_gone(fake_agent_bin, timeout=5)
    assert _index_row(tmp_path, entry) is not None, "the expired session left no index row"
    log = (tmp_path / ".cache" / "craze" / "host-logs" / f"{entry['hostId']}.log").read_text()
    assert "idle" in log.lower(), log


def _host_logs(home: Path) -> str:
    """Every host log under home, concatenated (one host runs per case)."""
    logs = sorted((home / ".cache" / "craze" / "host-logs").glob("*.log"))
    return "".join(path.read_text(encoding="utf-8", errors="replace") for path in logs)


def test_host_stderr_lands_in_the_host_log(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The host has no terminal: what craze and the agent write to stderr goes
    to ~/.cache/craze/host-logs/<hostId>.log, never to the client's screen.
    The in-process twin (test_tui's stderr cases, CRAZE_DETACH=0) proves the
    same two lines reach the terminal there, so their absence here is the
    lane, not a pty that dropped them."""
    canary = "CANARY-HOST-LOG"
    with _tui(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        env_extra={"CRAZE_FAKE_STDERR": canary},
        extra_args=["--plugin-dir", "definitely-not-here"],
    ) as tui:
        tui.wait_contains("cursor", timeout=WAIT)
        _prompt(tui, "hello")
        entry = _ready_entry(tmp_path)
        tui.write(b"\x04")
        assert tui.wait_exit(timeout=2) == 0, tui.screen()[-3000:]
        _wait_host_gone(tmp_path, entry["pid"], 5)
    text = _ANSI.sub("", tui.screen())
    assert canary not in text, text[-3000:]
    assert "plugin dir skipped:" not in text, text[-3000:]
    log = _host_logs(tmp_path)
    assert canary in log, log[-3000:]
    assert "plugin dir skipped:" in log, log[-3000:]


def test_a_failed_start_shows_and_its_stderr_is_in_the_log(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """A session that never started still reaches the client as the start
    failure (a start failure is not lost to a host with no terminal), exit 1;
    the agent's stderr behind it is in the host log."""
    canary = "CANARY-START-FAILED"
    with _tui(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="authfail",
        env_extra={"CRAZE_FAKE_STDERR": canary},
    ) as tui:
        tui.wait_contains("authentication failed", timeout=WAIT)
        mark = tui.mark()
        tui.write(b"\x04")
        assert tui.wait_exit(timeout=5) == 1, tui.screen()[-3000:]
        # And printed once the screen is restored, naming the host's log.
        _wait_output(tui, "authentication failed", timeout=5, mark=mark)
        _wait_output(tui, "the session host's log: ", timeout=5, mark=mark)
    _wait_fake_gone(fake_agent_bin, timeout=5)
    deadline = time.monotonic() + WAIT
    while time.monotonic() < deadline and canary not in _host_logs(tmp_path):
        time.sleep(0.05)
    assert canary in _host_logs(tmp_path), _host_logs(tmp_path)[-3000:]
