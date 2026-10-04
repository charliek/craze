from __future__ import annotations

import functools
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import tomllib
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

# The model catalog craze ships (plan 031 §3.1), compiled into the binary.
SHIPPED_CATALOG = ROOT / "internal" / "harness" / "modeltable" / "catalog.toml"


@functools.cache
def catalog_env_names() -> tuple[str, ...]:
    """Every environment variable a shipped provider takes a key from, sorted.

    Read from the catalog itself, so a catalog change needs no edit here -- the
    Go twin is modeltable.CatalogEnvNames. An empty list means the file moved
    or changed shape, and fails loudly rather than scrubbing nothing.
    """
    with SHIPPED_CATALOG.open("rb") as f:
        doc = tomllib.load(f)
    names = sorted({n for p in doc.get("providers", {}).values() for n in p.get("env_keys", [])})
    if not names:
        pytest.fail(f"{SHIPPED_CATALOG} names no env_keys to scrub")
    return tuple(names)


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


@pytest.fixture(scope="session")
def craze_fake_host_bin() -> Path:
    """cmd/craze-fake-host: protocol 1's deterministic host, standalone (plan
    036 §3.7)."""
    return _bin("CRAZE_FAKE_HOST_BIN", "bin", "craze-fake-host")


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
    # The shipped model catalog brings its providers' variable names into
    # every native table (plan 031 §3.13): a key exported in the developer's
    # shell would fund a test's native session, change its model list and be
    # redacted in its output. Every one goes; a case that wants one sets it.
    for name in catalog_env_names():
        monkeypatch.delenv(name, raising=False)
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
def both_modes(request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> str:
    """Run a case once with the ordinary detached host and once under the
    opt-out that hosts the session in the TUI (plan 030 §3.18, AC6).

    It returns the mode's name, for the few assertions that differ by it
    (whether a quit ends the session). isolate_run_env is autouse, so it has
    already set CRAZE_DETACH=0 by the time this runs. A detached case's HOME
    gets the suite's short idle exit (seed_host_idle_exit).
    """
    if request.param == "detached":
        monkeypatch.delenv("CRAZE_DETACH", raising=False)
        seed_host_idle_exit(tmp_path)
    else:
        monkeypatch.setenv("CRAZE_DETACH", "0")
    return request.param


# The idle exit of every detached host this suite starts (plan 030 C7r), in
# place of the default hour. host_cleanup stops a test's hosts however the
# test ends -- but not when pytest itself is SIGKILLed, and a host left then
# (its terminal hung up with the pty) would otherwise run for that hour. With
# this it exits once it has been unattended and idle for 30 s (after the 60 s
# startup grace if no client ever attached). No detached case leaves a host
# unattended anywhere near that long: the longest window is test_detach's
# hang-up and `craze -c`, bounded by the case's own waits at ~16 s; the idle
# case writes its own "2s".
TEST_HOST_IDLE_EXIT = "30s"


def seed_host_idle_exit(home: Path) -> Path:
    """Write HOME's config.toml (<home>/.craze/config.toml: every detached case
    runs with HOME at its tmp_path and CRAZE_HOME unset, the way PTYCraze does)
    with the suite's host_idle_exit, and return its path.

    Only detached cases get it: no host outlives an in-process case, and some
    of those assert that no config.toml was written. A case that writes its
    own config.toml replaces this one (and so chooses its own idle exit); the
    host's own writes (the provider it persists) keep the key.
    """
    config = home / ".craze" / "config.toml"
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text(f'host_idle_exit = "{TEST_HOST_IDLE_EXIT}"\n', encoding="utf-8")
    return config


MARKER_ENV = "CRAZE_RUNTIME_DIR"

# How long one `ps` of the machine's processes may take (macOS; Linux reads
# /proc). A scan that has not answered by then, or that failed, is a
# ProcessScanError -- unknown, never an empty set -- so the cleanup reports it
# instead of hanging on it, or reading it as "nothing left" (sol r13-c7r).
PS_TIMEOUT = 5.0


class ProcessScanError(RuntimeError):
    """The machine's processes could not be listed or read: whether any of
    this test's are still running is unknown."""


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

    The match is one whole entry of the process's environment, exactly
    ``CRAZE_RUNTIME_DIR=<this test's dir>`` (_environ) -- never a substring of
    what `ps -E` prints, which runs the arguments and the environment together,
    so a process whose arguments merely mention the marker would be taken for
    this test's and signalled (sol r12-c7).

    A scan that cannot be made -- macOS's `ps` failed or did not answer within
    PS_TIMEOUT, or its kernel will not say how large a process's arguments can
    be -- raises ProcessScanError (sol r13-c7r). One process whose environment
    cannot be read (gone meanwhile, another user's) is not this test's.
    """
    base = os.environ.get(MARKER_ENV)
    if not base:
        return set()
    if sys.platform == "darwin" and _darwin_argmax() is None:
        raise ProcessScanError("sysctl KERN_ARGMAX failed: no process's environment can be read")
    want = f"{MARKER_ENV}={base}".encode()
    me = os.getpid()
    found: set[int] = set()
    for pid in _all_pids():
        if pid == me:
            continue
        env = _environ(pid)
        if env is not None and want in env:
            found.add(pid)
    return found


def _all_pids() -> list[int]:
    """Every pid on the machine, as this user can list them, or a
    ProcessScanError when `ps` fails or does not answer within PS_TIMEOUT
    (it is killed then)."""
    if sys.platform == "linux":
        return [int(p.name) for p in Path("/proc").iterdir() if p.name.isdigit()]
    try:
        out = subprocess.run(["ps", "-axo", "pid="], capture_output=True, check=True, timeout=PS_TIMEOUT).stdout
    except subprocess.TimeoutExpired as e:
        raise ProcessScanError(f"`ps` did not answer within {PS_TIMEOUT:g}s") from e
    except (OSError, subprocess.CalledProcessError) as e:
        raise ProcessScanError(f"`ps` failed: {e}") from e
    return [int(tok) for tok in out.split() if tok.isdigit()]


def _environ(pid: int) -> list[bytes] | None:
    """pid's environment, one ``NAME=value`` entry each, or None when it
    cannot be read (gone, or another user's).

    Linux: /proc/<pid>/environ, NUL-separated. macOS has no such file; its
    kernel hands the arguments and the environment apart (_darwin_procargs).
    """
    if sys.platform == "linux":
        try:
            data = Path(f"/proc/{pid}/environ").read_bytes()
        except OSError:
            return None
        return [e for e in data.split(b"\0") if e]
    args = _darwin_procargs(pid)
    return None if args is None else args[1]


def _argv(pid: int) -> list[str]:
    """pid's arguments, [] when they cannot be read."""
    if sys.platform == "linux":
        try:
            raw = Path(f"/proc/{pid}/cmdline").read_bytes()
        except OSError:
            return []
        return [a.decode("utf-8", "replace") for a in raw.split(b"\0") if a]
    args = _darwin_procargs(pid)
    return [] if args is None else [a.decode("utf-8", "replace") for a in args[0]]


# sysctl(3) names on macOS (<sys/sysctl.h>): CTL_KERN, KERN_ARGMAX,
# KERN_PROCARGS2.
_CTL_KERN, _KERN_ARGMAX, _KERN_PROCARGS2 = 1, 8, 49


@functools.cache
def _darwin_sysctl_fn():  # noqa: ANN202 -- a ctypes function pointer
    """libc's sysctl(3), typed, loaded once."""
    import ctypes
    import ctypes.util

    fn = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True).sysctl
    fn.argtypes = [
        ctypes.POINTER(ctypes.c_int),
        ctypes.c_uint,
        ctypes.c_void_p,
        ctypes.POINTER(ctypes.c_size_t),
        ctypes.c_void_p,
        ctypes.c_size_t,
    ]
    fn.restype = ctypes.c_int
    return fn


def _darwin_sysctl(mib: list[int], size: int) -> bytes | None:
    """sysctl(mib) into a buffer of size bytes: the bytes it wrote, or None."""
    import ctypes

    name = (ctypes.c_int * len(mib))(*mib)
    buf = ctypes.create_string_buffer(size)
    n = ctypes.c_size_t(size)
    if _darwin_sysctl_fn()(name, len(mib), buf, ctypes.byref(n), None, 0) != 0:
        return None
    return buf.raw[: n.value]


@functools.cache
def _darwin_argmax() -> int | None:
    """KERN_ARGMAX: the most a process's KERN_PROCARGS2 can hold."""
    raw = _darwin_sysctl([_CTL_KERN, _KERN_ARGMAX], 4)
    if raw is None or len(raw) < 4:
        return None
    return int.from_bytes(raw[:4], sys.byteorder)


def _darwin_procargs(pid: int) -> tuple[list[bytes], list[bytes]] | None:
    """pid's (arguments, environment) on macOS, from KERN_PROCARGS2, or None.

    The buffer is argc (a native int), the executable's path, NUL padding,
    argc NUL-terminated arguments, then the environment's NUL-terminated
    entries up to an empty one (the kernel's own strings follow it). Only
    this user's processes can be read.
    """
    if sys.platform != "darwin":
        return None
    argmax = _darwin_argmax()
    if argmax is None:
        return None
    data = _darwin_sysctl([_CTL_KERN, _KERN_PROCARGS2, pid], argmax)
    if data is None or len(data) < 4:
        return None
    argc = int.from_bytes(data[:4], sys.byteorder)
    pos = data.find(b"\0", 4)  # the end of the executable's path
    if pos < 0:
        return None
    while pos < len(data) and data[pos] == 0:
        pos += 1
    argv: list[bytes] = []
    for _ in range(argc):
        end = data.find(b"\0", pos)
        if end < 0:
            return None
        argv.append(data[pos:end])
        pos = end + 1
    env: list[bytes] = []
    while pos < len(data):
        end = data.find(b"\0", pos)
        if end <= pos:
            break
        env.append(data[pos:end])
        pos = end + 1
    return argv, env


# The craze processes a test can leave running (plan 030 §3.18, plan 032
# §3.18): a detached `craze serve` host, and the per-machine `craze hub` (plan
# 032 C10), which outlives whoever spawned it until it is idle. Both are found
# by the test's marker, never by name alone.
_LINGERING = (["serve"], ["hub"])


def _is_lingering(pid: int) -> bool:
    """A `craze serve` host or a `craze hub`, by its argv."""
    return _argv(pid)[1:2] in _LINGERING


def _is_host_or_agent(pid: int, fake_agent: str | None) -> bool:
    """A `craze serve` host, a `craze hub`, or the fake agent, by argv."""
    argv = _argv(pid)
    return argv[1:2] in _LINGERING or (bool(argv) and argv[0] == fake_agent)


def _registry_files(root: Path) -> list[Path]:
    """Every host registry entry file under any HOME beneath root (a test's
    tmp_path: HOME is either tmp_path itself or tmp_path/home) -- whatever it
    holds: a file that does not parse is still an entry left behind (sol
    r12-c7). A host writes its entry by rename (rundir's temporaries are
    `.<id>.json.<n>`), so no reader ever sees one half-written."""
    return sorted(root.glob("**/.cache/craze/hosts/*.json"))


def _entry_pid(path: Path) -> int | None:
    """The pid a registry entry names, or None when it names none it can be
    read for (gone meanwhile, or not an entry craze wrote)."""
    try:
        pid = json.loads(path.read_text(encoding="utf-8")).get("pid")
    except (OSError, ValueError, AttributeError):
        return None
    return pid if isinstance(pid, int) and not isinstance(pid, bool) else None


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
    their child watchdog (X49): every host of the test's -- each one
    registered under its HOME, and each `craze serve` carrying its marker
    whose entry is already gone -- is sent SIGTERM (the resumable stop), given
    a bounded wait, and whatever is then left is SIGKILLed, the kill itself
    checked within a bound of its own, and reported. A `craze hub` carrying
    the marker is stopped the same way (plan 032 C10): its SIGTERM is its
    teardown, which removes its record and socket. The leftovers checked are
    the registry (any entry file still there, whether or not it parses), and
    any `craze serve`, `craze hub` or fake agent carrying this test's marker
    (marker_pids) -- so a process another session owns is never looked at,
    let alone killed. A hub's record is not a leftover: one a test killed on
    purpose is stale, and the next hub writes over it. (A pytest that is
    itself SIGKILLed runs none of this: TEST_HOST_IDLE_EXIT bounds the hosts
    it leaves, and a hub with no host and no client exits after its grace.)

    A scan of the machine's processes that fails or does not answer
    (ProcessScanError) ends the cleanup there, and the test fails with it and
    the registry entries still present: what it could not check is unknown,
    never "nothing left", and a pid it could not match to this test is never
    signalled (sol r13-c7r). So each wait is bounded by its own limit plus at
    most one scan (PS_TIMEOUT on macOS). TEST_HOST_IDLE_EXIT bounds what such
    a cleanup leaves.
    """
    yield
    try:
        why = _stop_hosts(tmp_path, str(fake_agent_bin))
    except ProcessScanError as e:
        left = "".join(f"; {_describe_entry(p)}" for p in _registry_files(tmp_path))
        why = f"a test's cleanup could not check for craze left behind: {e}{left}"
    if why:
        pytest.fail(why, pytrace=False)


def _stop_hosts(tmp_path: Path, fake: str) -> str | None:
    """host_cleanup's stop and check: None when nothing of the test's is left,
    and otherwise why the test fails. A failed scan raises ProcessScanError."""
    mine = marker_pids()
    registered = {pid for pid in map(_entry_pid, _registry_files(tmp_path)) if pid is not None}
    _signal((registered & mine) | {p for p in mine if _is_lingering(p)}, signal.SIGTERM)
    deadline = time.monotonic() + 8.0
    while True:
        left = _stray_processes(fake) + [_describe_entry(p) for p in _registry_files(tmp_path)]
        if not left or time.monotonic() >= deadline:
            break
        time.sleep(0.05)
    if not left:
        return None
    # Whatever the failure says, nothing is left running -- and that is
    # checked, not assumed.
    _signal(marker_pids(), signal.SIGKILL)
    deadline = time.monotonic() + 5.0
    while (alive := _stray_processes(fake)) and time.monotonic() < deadline:
        time.sleep(0.05)
    why = "a test left craze behind: " + "; ".join(left)
    if alive:
        why += "; still running after SIGKILL: " + "; ".join(alive)
    return why


def _signal(pids: set[int], sig: signal.Signals) -> None:
    for pid in sorted(pids):
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            pass


def _stray_processes(fake_agent: str) -> list[str]:
    """This test's `craze serve` hosts, `craze hub`s and fake agents still
    running."""
    live = sorted(p for p in marker_pids() if pid_alive(p))
    return [f"process {p}: {' '.join(_argv(p))[:200]}" for p in live if _is_host_or_agent(p, fake_agent)]


def _describe_entry(path: Path) -> str:
    pid = _entry_pid(path)
    return f"registry entry {path.stem} ({'unreadable' if pid is None else f'pid {pid}'})"


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
