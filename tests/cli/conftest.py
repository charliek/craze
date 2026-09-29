from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from collections.abc import Iterator, Mapping
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

# The config-file variable CRAZE_HOME replaced. craze refuses to run while it
# is set, so an inherited one is scrubbed below. It is spelled in two parts on
# purpose: the repo-walk test in internal/paths keeps the whole name out of
# every file outside that package, so a test still isolating itself with it
# fails CI instead of reaching the developer's real ~/.craze.
REMOVED_CONFIG_ENV = "CRAZE_" + "CONFIG"


def _bin(env_name: str, *parts: str) -> Path:
    override = os.environ.get(env_name)
    if override:
        return Path(override)
    path = ROOT.joinpath(*parts)
    if not path.is_file():
        pytest.fail(f"{path} is missing; run `make build` (or set {env_name})")
    return path


@pytest.fixture(scope="session")
def craze_bin() -> Path:
    return _bin("CRAZE_BIN", "bin", "craze")


@pytest.fixture(scope="session")
def fake_agent_bin() -> Path:
    return _bin("CRAZE_FAKE_AGENT_BIN", "bin", "craze-fake-agent")


@pytest.fixture(autouse=True)
def isolate_run_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    """Isolate everything a craze run reads out of the environment.

    Every helper here builds its child environment from ``os.environ``, so one
    fixture covers them all. HOME is in it because craze walks the plugin
    caches under HOME at session start: without it a run would find whatever
    the developer happens to have installed. CRAZE_HOME is set explicitly --
    never left to follow HOME -- so a developer's exported one cannot leak in,
    and the config file and session index are ``<tmp>/craze-home/config.toml``
    and ``<tmp>/craze-home/sessions.jsonl``.
    """
    home = tmp_path / "home"
    home.mkdir(parents=True, exist_ok=True)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("CRAZE_PROVIDER", "")
    monkeypatch.setenv("CRAZE_HOME", str(tmp_path / "craze-home"))
    monkeypatch.delenv(REMOVED_CONFIG_ENV, raising=False)
    # The session journal's opt-out (plan 020 §3.5). An exported one would
    # otherwise turn journaling off under the cases that assert a journal was
    # written -- or, unparseable, print a diagnostic into a run whose stderr a
    # case reads. A case that wants it sets it itself.
    monkeypatch.delenv("CRAZE_JOURNAL", raising=False)
    # craze reports its status to the terminal multiplexer it runs in (herdr,
    # roost) whenever that host's variables say so -- and this suite is often
    # run from inside one. Inherited, they would point a test craze at the
    # developer's own live pane or tab. Every host variable goes; a test that
    # wants a host sets exactly the ones it needs, aimed at its own fake socket.
    for name in host_env_names(os.environ):
        monkeypatch.delenv(name, raising=False)
    # Every TUI serves its session over a control socket (plan 027 C13), bound
    # in the first usable runtime base -- $XDG_RUNTIME_DIR/craze on a desktop,
    # which is the developer's own. Each test gets a short base of its own
    # instead, under the real /tmp and never under tmp_path: a macOS temp path
    # is long enough to overflow sun_path with the socket's name under it.
    # The opt-out is scrubbed too, so an exported one cannot turn the socket
    # off under the cases that assert it exists; a case that wants it off sets
    # it itself.
    runtime_dir = tempfile.mkdtemp(prefix="czt-", dir="/tmp")
    monkeypatch.setenv("CRAZE_RUNTIME_DIR", runtime_dir)
    monkeypatch.delenv("XDG_RUNTIME_DIR", raising=False)
    monkeypatch.delenv("CRAZE_CONTROL_SOCKET", raising=False)
    # The ordinary craze runs its session in a detached host (plan 030 §3.5);
    # these cases were written for the TUI that hosts its own, and keep testing
    # it under the opt-out. The core cases (test_tui's basics, test_attach,
    # test_bridge) take `both_modes` below and run in each; test_detach.py
    # unsets the opt-out itself.
    monkeypatch.setenv("CRAZE_DETACH", "0")
    try:
        yield
    finally:
        shutil.rmtree(runtime_dir, ignore_errors=True)


@pytest.fixture(params=["detached", "in-process"])
def both_modes(request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch) -> str:
    """Run a case once with the ordinary detached host and once under the
    opt-out that hosts the session in the TUI (plan 030 §3.18, AC6).

    It returns the mode's name, for the few assertions that differ by it
    (whether a quit ends the session). isolate_run_env is autouse, so it has
    already set CRAZE_DETACH=0 by the time this runs.
    """
    if request.param == "detached":
        monkeypatch.delenv("CRAZE_DETACH", raising=False)
    else:
        monkeypatch.setenv("CRAZE_DETACH", "0")
    return request.param


def marker_pids() -> set[int]:
    """The pids of the processes this test started, and nothing else.

    Every process a test runs inherits its CRAZE_RUNTIME_DIR (isolate_run_env:
    one fresh mkdtemp per test), and a detached host and its fake agent are no
    exception -- they are spawned out of the TUI's environment. So that value
    is the marker: a process carries it if and only if it descends from this
    test's environment. A scan for a binary's name would also see every other
    craze on the machine (parallel sessions, a developer's own), and neither
    fail nor kill those. Without the variable set (tmux_smoke.py runs outside
    pytest) the set is empty and callers fall back to their old, wider scan.

    Linux reads /proc/<pid>/environ; macOS has no such file, and `ps -E`
    appends each process's environment to its command instead.
    """
    base = os.environ.get("CRAZE_RUNTIME_DIR")
    if not base:
        return set()
    me = os.getpid()
    found: set[int] = set()
    if sys.platform == "linux":
        needle = b"CRAZE_RUNTIME_DIR=" + base.encode() + b"\0"
        for path in Path("/proc").glob("[0-9]*/environ"):
            try:
                data = path.read_bytes()
            except OSError:
                continue  # gone, or not ours to read
            if data.startswith(needle) or b"\0" + needle in data:
                found.add(int(path.parent.name))
    else:
        out = subprocess.run(
            ["ps", "-axwwE", "-o", "pid=,command="], capture_output=True, check=True
        ).stdout
        needle = b" CRAZE_RUNTIME_DIR=" + base.encode()
        for line in out.splitlines():
            pid_str, _, rest = line.strip().partition(b" ")
            if pid_str.isdigit() and needle in rest:
                found.add(int(pid_str))
    found.discard(me)
    return found


def _argv(pid: int) -> list[str]:
    try:
        raw = Path(f"/proc/{pid}/cmdline").read_bytes()
    except OSError:
        return []
    return [a.decode("utf-8", "replace") for a in raw.split(b"\0") if a]


def _is_host_or_agent(pid: int, fake_agent: str | None) -> bool:
    """A `craze serve` host, or the fake agent, by argv (Linux; elsewhere the
    caller's marker set is itself the leftover list)."""
    argv = _argv(pid)
    if not argv:
        return sys.platform != "linux"
    if argv[1:2] == ["serve"]:
        return True
    return argv[0] == fake_agent


def _registry_entries(root: Path) -> list[dict]:
    """Every host registry entry under any HOME beneath root (a test's
    tmp_path: HOME is either tmp_path itself or tmp_path/home)."""
    entries = []
    for path in root.glob("**/.cache/craze/hosts/*.json"):
        try:
            entries.append(json.loads(path.read_text(encoding="utf-8")))
        except (OSError, ValueError):
            continue
    return entries


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    # A zombie still answers signal 0; only its /proc state says it is gone.
    try:
        state = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[0]
    except (OSError, IndexError):
        return True
    return state != "Z"


@pytest.fixture(autouse=True)
def host_cleanup(isolate_run_env: None, tmp_path: Path, fake_agent_bin: Path) -> Iterator[None]:
    """After every test, stop every host that test started and fail on what
    will not go (plan 030 §3.18).

    A detached host outlives the terminal that started it, so a test that
    ends early -- a failed assertion, a hung-up pty on purpose -- would leave
    one running past the suite. This is the guarantee the Go tests get from
    their child watchdog (X49): each host registered under the test's HOME is
    sent SIGTERM (the resumable stop), given a bounded wait, and whatever is
    then left is SIGKILLed and reported. The leftovers checked are the
    registry (an entry still there), and any `craze serve` or fake agent
    carrying this test's marker (marker_pids) -- so a process another session
    owns is never looked at, let alone killed.
    """
    yield
    fake = str(fake_agent_bin)
    mine = marker_pids()
    hosts = {e["pid"] for e in _registry_entries(tmp_path) if isinstance(e.get("pid"), int)}
    for pid in sorted(hosts & mine):
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + 8.0
    left: list[str] = []
    while True:
        left = []
        live = {p for p in marker_pids() if pid_alive(p)}
        left += [f"process {p}: {' '.join(_argv(p))[:200]}" for p in sorted(live) if _is_host_or_agent(p, fake)]
        left += [
            f"registry entry {e.get('hostId')} (pid {e.get('pid')})" for e in _registry_entries(tmp_path)
        ]
        if not left or time.monotonic() >= deadline:
            break
        time.sleep(0.05)
    if left:
        # Whatever the assertion below says, nothing is left running.
        for p in marker_pids():
            try:
                os.kill(p, signal.SIGKILL)
            except ProcessLookupError:
                pass
        pytest.fail("a test left craze behind: " + "; ".join(left), pytrace=False)


def without_seq(events: list[dict], subject: dict | list[dict]) -> dict | list[dict]:
    """``subject`` with its ``seq`` key taken off, the run's seqs checked first.

    Every ``craze prompt --json`` line that came from the session's event
    stream carries, as its second key, the sequence number the session's event
    log gave that event (plan 020 §3.6, A21). The numbers are increasing but
    **not** contiguous -- the JSON projection drops kinds it has nothing to say
    about, and a dropped event still took its number -- and how many of those a
    run produces is a matter of timing, so a case that spelled an exact value
    would be pinning a flake. What every run owes is the order, and that is
    what is checked here, across the whole run: each seq is an integer strictly
    greater than the one on the line before it.

    The key is then taken off ``subject`` -- one line, or a list of them -- so
    the rest can be compared exactly as it was before ``seq`` existed. Nothing
    else is loosened. Each subject line must carry a seq: these are the
    session's own. The lines craze writes itself (a startup error, the queue
    row a signal took between the take and the send) carry none, and the Go
    tests in internal/cli assert that where they are written.
    """
    previous: int | None = None
    for event in events:
        if "seq" not in event:
            continue
        seq = event["seq"]
        assert isinstance(seq, int) and not isinstance(seq, bool), f"seq is not an integer: {event}"
        assert previous is None or seq > previous, f"seq {seq} came after {previous}: {event}"
        previous = seq

    def strip(line: dict) -> dict:
        assert "seq" in line, f"a line from the session's stream must carry a seq: {line}"
        return {key: value for key, value in line.items() if key != "seq"}

    if isinstance(subject, list):
        return [strip(line) for line in subject]
    return strip(subject)


def host_env_names(env: Mapping[str, str]) -> list[str]:
    """Every herdr and roost variable in env: the hosts craze may report to."""
    return [name for name in env if name.startswith(("HERDR_", "ROOST_"))]


def require_rg() -> None:
    """The repo's rule for a test that needs ripgrep (plan 019 §3.9, D-41).

    The native harness's grep and glob run the rg found on PATH, so a laptop
    without it still passes by skipping. In CI that skip would be a quiet
    pass on a runner that lost rg, so the `cli` job sets CRAZE_REQUIRE_RG=1
    and the skip becomes a failure -- the same pair internal/tui's
    requireFrameRG and internal/harness/tool/opencode's own tests use.
    """
    if shutil.which("rg"):
        return
    if os.environ.get("CRAZE_REQUIRE_RG") == "1":
        pytest.fail("CRAZE_REQUIRE_RG=1, and ripgrep (rg) is not on PATH")
    pytest.skip("ripgrep (rg) is not on PATH (set CRAZE_REQUIRE_RG=1 to fail instead)")
