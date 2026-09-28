"""bubblewrap sandboxes for agent runs and scoring commands (plan 029 §3.1.4).

Inside a sandbox nothing of /home exists. Toolchains are bound read-only under
/sandbox/tools, the owner's GOMODCACHE read-only at /sandbox/gomodcache, and only the
run's workspace (/sandbox/work/<repo>), temp home (/sandbox/home) and Go cache
(/sandbox/gocache) are writable. Every sandbox has its own network namespace: an
agent run reaches the proxy -- and nothing else -- through an in-sandbox relay on
127.0.0.1:<port> to the proxy's Unix socket (relay.py); scoring, validation and
post-run git have no network at all. Every sandbox that runs submitted or task code --
each agent run, each scoring and each validation invocation -- gets a private, empty
Go cache deleted afterwards: nothing writable is shared between them. The
environment is built from
nothing -- never from this process's ``os.environ`` -- and is checked for keys before
every launch. A PID namespace takes every descendant down with the harness; the
runner kills bwrap at the timeout and asserts quiescence afterwards.
"""

from __future__ import annotations

import asyncio
import fcntl
import json
import os
import re
import shutil
import signal
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Mapping

from crazeeval import paths

BWRAP = "bwrap"
DEFAULT_TIMEOUT_S = 20 * 60
TOOL_VENV_PYTEST = "pytest==9.1.1"
FORBIDDEN_VAR_RE = re.compile(r"KEY|TOKEN|SECRET", re.I)

GIT_IDENTITY = {
    "GIT_AUTHOR_NAME": "craze-eval",
    "GIT_AUTHOR_EMAIL": "eval@craze.invalid",
    "GIT_COMMITTER_NAME": "craze-eval",
    "GIT_COMMITTER_EMAIL": "eval@craze.invalid",
}

# Inside locations of each toolchain.
T = paths.SANDBOX_TOOLS
TOOL_INSIDE = {
    "venv": f"{T}/venv",
    "go": f"{T}/go",
    "node": f"{T}/node",
    "grok": f"{T}/grok",
    "opencode": f"{T}/opencode",
    "uv": f"{T}/bin/uv",
    "craze": f"{T}/bin/craze",
}
# PATH order: the eval venv (python, pytest) first, then the toolchains.
PATH_ORDER = [
    ("venv", f"{T}/venv/bin"),
    ("bin", f"{T}/bin"),
    ("go", f"{T}/go/bin"),
    ("node", f"{T}/node/bin"),
    ("grok", f"{T}/grok"),
    ("opencode", f"{T}/opencode"),
]
BASE_TOOLS = frozenset({"venv", "go", "uv"})
RELAY_INSIDE = f"{paths.SANDBOX_ROOT}/relay.py"
SOCKET_INSIDE = f"{paths.SANDBOX_ROOT}/proxy.sock"
RELAY_SOURCE = Path(__file__).with_name("relay.py")
# Host helpers (uv, the opencode seed) get only these variables from this process,
# each value checked against the key ring (review r1-c1 finding 6).
HELPER_ENV_ALLOW = (
    "HOME", "USER", "LANG", "LC_ALL", "TMPDIR", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME",
    "UV_CACHE_DIR", "UV_PYTHON_INSTALL_DIR", "SSL_CERT_FILE", "SSL_CERT_DIR",
)
# Merged-/usr hosts: these top-level names are symlinks into /usr.
USR_MERGE = (("/bin", "usr/bin"), ("/lib", "usr/lib"), ("/lib64", "usr/lib64"), ("/sbin", "usr/sbin"))


def _usr_merge_links() -> list[str]:
    a = []
    for link, target in USR_MERGE:
        if Path(link).is_symlink():
            a += ["--symlink", target, link]
    return a


def _credential_name(name: str) -> bool:
    return bool(FORBIDDEN_VAR_RE.search(name)) and name != paths.DUMMY_KEY_VAR


def bwrap_available() -> bool:
    if shutil.which(BWRAP) is None:
        return False
    try:
        argv = [BWRAP, "--unshare-pid", "--ro-bind", "/usr", "/usr", *_usr_merge_links()]
        r = subprocess.run(
            argv + ["--proc", "/proc", "--dev", "/dev", "--", "/usr/bin/true"],
            env={"PATH": "/usr/bin:/bin"},
            capture_output=True,
            timeout=20,
        )
        return r.returncode == 0
    except (OSError, subprocess.SubprocessError):
        return False


def helper_env(keyring=None, extra_path: list[str] | None = None, source=None) -> dict[str, str]:
    """The environment for a host-side helper (uv, the opencode seed): an explicit
    allowlist of names, never a copy of ``os.environ``, and no value may hold a loaded
    key. The ring is loaded from the owner's providers when not given."""
    source = os.environ if source is None else source
    if keyring is None:
        try:
            from crazeeval.keys import load_providers

            _, keyring = load_providers()
        except OSError:
            keyring = None
    env = {k: source[k] for k in HELPER_ENV_ALLOW if source.get(k)}
    env["PATH"] = ":".join([*(extra_path or []), "/usr/bin", "/bin"])
    bad = env_problems(env, keyring)
    if bad:
        raise RuntimeError(f"refusing a helper environment: {bad} hold a key or look like credentials")
    return env


def ensure_tool_venv(cache_dir: Path | None = None) -> Path:
    """The eval's own tool venv (system python + pytest), relocatable so it can be
    bound at /sandbox/tools/venv."""
    cache_dir = Path(cache_dir or paths.CACHE_DIR)
    venv = cache_dir / "toolvenv"
    cache_dir.mkdir(parents=True, exist_ok=True)
    with open(cache_dir / ".toolvenv.lock", "w") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        marker = venv / ".crazeeval-ready"
        if marker.exists():
            return venv
        uv = shutil.which("uv") or str(Path.home() / ".local/bin/uv")
        env = helper_env(extra_path=[str(Path(uv).parent)])
        subprocess.run([uv, "venv", "--clear", "--relocatable", "--python", "/usr/bin/python3", str(venv)], check=True,
                       env=env, capture_output=True)
        subprocess.run([uv, "pip", "install", "--python", str(venv / "bin/python"), TOOL_VENV_PYTEST], check=True,
                       env=env, capture_output=True)
        marker.write_text("ok\n")
    return venv


def _mise_install(tool: str, pick: str) -> Path | None:
    p = Path.home() / ".local/share/mise/installs" / tool / pick
    return p.resolve() if p.exists() else None


def _repo_go_version() -> str | None:
    try:
        text = (paths.REPO_ROOT / ".mise.toml").read_text()
    except OSError:
        return None
    m = re.search(r'^\s*go\s*=\s*"([^"]+)"', text, re.M)
    return m.group(1) if m else None


@dataclass(frozen=True)
class Toolchains:
    go_root: Path
    venv: Path
    uv: Path
    gomodcache: Path
    node_root: Path | None = None
    grok_dir: Path | None = None
    opencode_dir: Path | None = None

    @classmethod
    def resolve(cls, cache_dir: Path | None = None, need_venv: bool = True) -> "Toolchains":
        cache_dir = Path(cache_dir or paths.CACHE_DIR)
        gov = _repo_go_version()
        go_root = (_mise_install("go", gov) if gov else None) or _mise_install("go", "latest")
        if go_root is None:
            go = shutil.which("go")
            if not go:
                raise RuntimeError("no Go toolchain found")
            go_root = Path(go).resolve().parent.parent
        node_root = _mise_install("node", "lts")
        node = shutil.which("node")
        if node_root is None and node:
            node_root = Path(node).resolve().parent.parent
        if node_root is not None and not (node_root / "bin" / "node").is_file():
            node_root = None
        # The install directory, not a mise shim (a shim resolves to mise itself).
        grok_dir = _mise_install("github-charliek-grok-build", "gx-vlatest")
        if grok_dir is None or not (grok_dir / "gx").is_file():
            gx = shutil.which("gx")
            grok_dir = Path(gx).resolve().parent if gx else None
            if grok_dir is not None and not (grok_dir / "gx").is_file():
                grok_dir = None
        oc = Path.home() / ".opencode/bin"
        uv = Path(shutil.which("uv") or Path.home() / ".local/bin/uv").resolve()
        gomodcache = Path(os.environ.get("GOMODCACHE") or Path.home() / "go/pkg/mod")
        venv = ensure_tool_venv(cache_dir) if need_venv else cache_dir / "toolvenv"
        return cls(
            go_root=go_root,
            venv=venv,
            uv=uv,
            gomodcache=gomodcache,
            node_root=node_root,
            grok_dir=grok_dir,
            opencode_dir=oc if oc.exists() else None,
        )

    def host_path(self, name: str, craze_bin: Path | None = None) -> Path | None:
        return {
            "venv": self.venv,
            "go": self.go_root,
            "node": self.node_root,
            "grok": self.grok_dir,
            "opencode": self.opencode_dir,
            "uv": self.uv,
            "craze": craze_bin,
        }.get(name)

    def describe(self) -> dict:
        return {k: str(v) if v else None for k, v in self.__dict__.items()}


@dataclass(frozen=True)
class Relay:
    """An agent run's only way out: 127.0.0.1:``port`` inside -> the proxy's ``socket``."""

    port: int
    socket: Path


@dataclass
class SandboxSpec:
    workspace: Path  # host path, bound read-write (read-only with ws_readonly)
    ws_inside: str  # e.g. /sandbox/work/craze
    home: Path | None  # host temp home, bound read-write; None = a tmpfs home
    tools: set[str] = field(default_factory=set)  # beyond BASE_TOOLS
    env: dict[str, str] = field(default_factory=dict)  # the harness's own variables
    # False (every run, scoring, validation, git) unshares the network; True keeps
    # the host's and is used only by the opencode seed, which runs no agent.
    network: bool = False
    relay: Relay | None = None
    extra_ro: list[tuple[Path, str]] = field(default_factory=list)
    extra_rw: list[tuple[Path, str]] = field(default_factory=list)
    goflags: str | None = None
    craze_bin: Path | None = None
    # The writable Go cache at /sandbox/gocache, always private to this one sandbox
    # (an agent run's, or one scoring/validation invocation's); None binds none.
    gocache: Path | None = None
    ws_readonly: bool = False
    home_ro: list[tuple[Path, str]] = field(default_factory=list)


def inside_path(tools: set[str]) -> str:
    have = set(tools) | set(BASE_TOOLS)
    parts = []
    for name, p in PATH_ORDER:
        if name == "bin":
            if have & {"uv", "craze"}:
                parts.append(p)
        elif name in have:
            parts.append(p)
    parts += ["/usr/bin", "/usr/sbin"]
    return ":".join(parts)


def build_env(spec: SandboxSpec) -> dict[str, str]:
    """The child environment, from nothing (never os.environ)."""
    env = {
        "HOME": paths.SANDBOX_HOME,
        "PATH": inside_path(spec.tools),
        "USER": "eval",
        # Not in the plan's list: without it gx reports "Shell: /bin/sh" (dash) and
        # runs its terminal tool there, while craze always uses /bin/bash. A real
        # user has SHELL set; bash for every harness keeps them comparable.
        "SHELL": "/bin/bash",
        "LANG": "C.UTF-8",
        "TERM": "dumb",
        "TMPDIR": "/tmp",
        "GOMODCACHE": paths.SANDBOX_GOMODCACHE,
        "GOCACHE": paths.SANDBOX_GOCACHE,
        "GOTOOLCHAIN": "local",
        "GOPROXY": "off",
        **GIT_IDENTITY,
    }
    if spec.goflags:
        env["GOFLAGS"] = spec.goflags
    env.update(spec.env)
    return env


def env_problems(env: Mapping[str, str], keyring=None) -> list[str]:
    """Names of variables that equal/contain a loaded key or look like a credential
    (KEY|TOKEN|SECRET), the dummy variable excepted. Never returns a value."""
    bad = []
    for name, value in env.items():
        if keyring is not None and (keyring.contains_key(value) or keyring.contains_key(name)):
            bad.append(name)
        elif _credential_name(name):
            bad.append(name)
    return bad


def bwrap_argv(spec: SandboxSpec, tc: Toolchains, cmd: list[str], env: dict[str, str], info_fd: int | None = None) -> list[str]:
    a = [
        BWRAP,
        "--die-with-parent",
        "--unshare-pid",
        "--unshare-ipc",
        "--unshare-uts",
        "--new-session",
        "--clearenv",
    ]
    if spec.relay is not None and spec.network:
        raise ValueError("a relayed sandbox has no host network")
    if not spec.network:
        a.append("--unshare-net")
    a += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"]
    a += ["--ro-bind", "/usr", "/usr", "--ro-bind", "/etc", "/etc"]
    # The resolver's sockets answer DNS for anyone who can reach them -- a way out
    # of a network namespace. Only the host-network seed gets them.
    if spec.network and Path("/run/systemd/resolve").exists():
        a += ["--ro-bind", "/run/systemd/resolve", "/run/systemd/resolve"]
    a += _usr_merge_links()
    # /usr/local/bin holds a stale craze and the owner's own tools; nothing a run needs.
    if Path("/usr/local/bin").is_dir():
        a += ["--tmpfs", "/usr/local/bin"]
    for name in sorted(set(spec.tools) | set(BASE_TOOLS)):
        host = tc.host_path(name, spec.craze_bin)
        if host is None:
            raise RuntimeError(f"toolchain {name!r} is not available")
        a += ["--ro-bind", str(host), TOOL_INSIDE[name]]
    a += ["--ro-bind", str(tc.gomodcache), paths.SANDBOX_GOMODCACHE]
    if spec.gocache is not None:
        a += ["--bind", str(spec.gocache), paths.SANDBOX_GOCACHE]
    for host, inside in spec.extra_ro:
        a += ["--ro-bind", str(host), inside]
    for host, inside in spec.extra_rw:
        a += ["--bind", str(host), inside]
    if spec.relay is not None:
        a += ["--ro-bind", str(RELAY_SOURCE), RELAY_INSIDE, "--bind", str(spec.relay.socket), SOCKET_INSIDE]
    a += ["--ro-bind" if spec.ws_readonly else "--bind", str(spec.workspace), spec.ws_inside]
    if spec.home is not None:
        a += ["--bind", str(spec.home), paths.SANDBOX_HOME]
    else:
        a += ["--tmpfs", paths.SANDBOX_HOME]
    # Read-only binds inside the home (the opencode seed's node_modules): after it.
    for host, inside in spec.home_ro:
        a += ["--ro-bind", str(host), inside]
    for k in sorted(env):
        a += ["--setenv", k, env[k]]
    a += ["--chdir", spec.ws_inside]
    if info_fd is not None:
        a += ["--info-fd", str(info_fd)]
    if spec.relay is not None:
        cmd = [f"{TOOL_INSIDE['venv']}/bin/python", "-I", "-S", RELAY_INSIDE, "--port", str(spec.relay.port),
               "--unix", SOCKET_INSIDE, "--", *cmd]
    a += ["--"] + list(cmd)
    return a


# -- processes ---------------------------------------------------------------------


def _pidns(pid: int) -> str | None:
    try:
        return os.readlink(f"/proc/{pid}/ns/pid")
    except OSError:
        return None


def _all_pids() -> list[int]:
    return [int(d) for d in os.listdir("/proc") if d.isdigit()]


def pids_in_ns(ns: str) -> list[int]:
    me = os.getpid()
    return [p for p in _all_pids() if p != me and _pidns(p) == ns]


def _cwd_in(pid: int, ws_host: Path, ws_inside: str) -> bool:
    try:
        link = os.readlink(f"/proc/{pid}/cwd")
    except OSError:
        return False
    host = str(ws_host)
    if link == host or link.startswith(host + "/"):
        return True
    if link == ws_inside or link.startswith(ws_inside + "/"):
        rel = link[len(ws_inside):].lstrip("/")
        try:
            a = os.stat(f"/proc/{pid}/cwd")
            b = os.stat(ws_host / rel) if rel else os.stat(ws_host)
            return (a.st_dev, a.st_ino) == (b.st_dev, b.st_ino)
        except OSError:
            return False
    return False


def _root_id(pid: int) -> tuple[int, int] | None:
    try:
        st = os.stat(f"/proc/{pid}/root")
        return (st.st_dev, st.st_ino)
    except OSError:
        return None


def _in_sandbox(pid: int, ns: str | None, root: tuple[int, int] | None) -> bool:
    """In this sandbox: the same PID namespace AND the same root filesystem (a dead
    namespace's inode can be reused by a concurrent run's sandbox; its root cannot)."""
    return ns is not None and _pidns(pid) == ns and (root is None or _root_id(pid) == root)


def leftovers(ns: str | None, ws_host: Path, ws_inside: str, root: tuple[int, int] | None = None) -> list[int]:
    """Processes still alive in the sandbox or with the workspace as cwd."""
    me = os.getpid()
    out = []
    for p in _all_pids():
        if p == me:
            continue
        if _in_sandbox(p, ns, root) or _cwd_in(p, ws_host, ws_inside):
            out.append(p)
    return sorted(set(out))


@dataclass
class SandboxResult:
    exit_code: int | None
    timed_out: bool
    wall_s: float
    pidns: str | None
    quiescent: bool
    leftover_pids: list[int]
    env_samples: dict
    argv_tools: list[str]


def _no_samples() -> dict:
    return {"sampled": 0, "unreadable": 0, "key_hits": 0, "forbidden_names": []}


def sample_environ(ns: str, keyring=None) -> dict:
    """Read /proc/<pid>/environ of every process in the sandbox. Reports counts only."""
    out = _no_samples()
    for p in pids_in_ns(ns):
        try:
            data = Path(f"/proc/{p}/environ").read_bytes()
        except OSError:
            out["unreadable"] += 1
            continue
        out["sampled"] += 1
        if keyring is not None and keyring.contains_key(data):
            out["key_hits"] += 1
        for item in data.split(b"\0"):
            name = item.split(b"=", 1)[0].decode("utf-8", errors="replace")
            if name and _credential_name(name) and name not in out["forbidden_names"]:
                out["forbidden_names"].append(name)
    return out


def _merge_samples(acc: dict, s: dict) -> None:
    acc["sampled"] += s["sampled"]
    acc["unreadable"] += s["unreadable"]
    acc["key_hits"] += s["key_hits"]
    for n in s["forbidden_names"]:
        if n not in acc["forbidden_names"]:
            acc["forbidden_names"].append(n)


async def run_sandboxed(
    spec: SandboxSpec,
    tc: Toolchains,
    cmd: list[str],
    *,
    stdout: Path,
    stderr: Path,
    timeout: float = DEFAULT_TIMEOUT_S,
    keyring=None,
    sample_env: bool = True,
) -> SandboxResult:
    env = build_env(spec)
    bad = env_problems(env, keyring)
    if bad:
        raise RuntimeError(f"refusing to launch: credential-like variables {bad}")
    r_fd, w_fd = os.pipe()
    argv = bwrap_argv(spec, tc, cmd, env, info_fd=w_fd)
    t0 = time.monotonic()
    with open(stdout, "wb") as out, open(stderr, "wb") as err:
        proc = await asyncio.create_subprocess_exec(
            *argv,
            stdin=asyncio.subprocess.DEVNULL,
            stdout=out,
            stderr=err,
            env={"PATH": "/usr/bin:/bin"},
            pass_fds=(w_fd,),
            start_new_session=True,
        )
    os.close(w_fd)
    ns = None
    root = None
    loop = asyncio.get_running_loop()
    try:
        raw = await asyncio.wait_for(loop.run_in_executor(None, os.read, r_fd, 4096), timeout=15)
        child = json.loads(raw.decode().split("}", 1)[0] + "}").get("child-pid")
        if child:
            ns = _pidns(int(child))
            root = _root_id(int(child))
    except (asyncio.TimeoutError, ValueError, OSError):
        ns = None
    samples = _no_samples()

    async def sampler():
        if ns is None or not sample_env:
            return
        delays = [1.0, 4.0] + [20.0] * 10_000
        for d in delays:
            await asyncio.sleep(d)
            _merge_samples(samples, sample_environ(ns, keyring))

    stask = asyncio.create_task(sampler())
    timed_out = False
    try:
        await asyncio.wait_for(proc.wait(), timeout=timeout)
    except asyncio.TimeoutError:
        timed_out = True
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        try:
            proc.kill()
        except ProcessLookupError:
            pass
        await proc.wait()
    finally:
        stask.cancel()
        try:
            await stask
        except (asyncio.CancelledError, Exception):
            pass
        try:
            os.close(r_fd)
        except OSError:
            pass
    wall = time.monotonic() - t0
    left = leftovers(ns, spec.workspace, spec.ws_inside, root)
    if left:
        for p in left:
            if _in_sandbox(p, ns, root):
                try:
                    os.kill(p, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    pass
        await asyncio.sleep(1.0)
        left = leftovers(ns, spec.workspace, spec.ws_inside, root)
    return SandboxResult(
        exit_code=proc.returncode,
        timed_out=timed_out,
        wall_s=round(wall, 3),
        pidns=ns,
        quiescent=not left,
        leftover_pids=left,
        env_samples=samples,
        argv_tools=sorted(set(spec.tools) | set(BASE_TOOLS)),
    )


def run_sandboxed_sync(spec: SandboxSpec, tc: Toolchains, cmd: list[str], *, stdout: Path, stderr: Path,
                       timeout: float = 600, keyring=None) -> SandboxResult:
    return asyncio.run(run_sandboxed(spec, tc, cmd, stdout=stdout, stderr=stderr, timeout=timeout, keyring=keyring,
                                     sample_env=False))
