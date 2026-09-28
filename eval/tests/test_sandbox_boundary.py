"""The sandbox hides /home; the eval module boundary holds (plan 029 §3.1.1, §3.1.4)."""

from __future__ import annotations

import asyncio
import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

from crazeeval import paths
from crazeeval.keys import KeyRing
from crazeeval.sandbox import Relay, SandboxSpec, Toolchains, bwrap_argv, build_env, bwrap_available, run_sandboxed


@pytest.fixture(scope="module")
def tc():
    if not bwrap_available():
        pytest.skip("bubblewrap is not usable here")
    return Toolchains.resolve()


SCRIPT = r"""
import json, os, sys
def exists(p):
    try:
        os.stat(p); return True
    except OSError:
        return False
t = json.loads(sys.argv[1])
print(json.dumps({"reach": {k: exists(v) for k, v in t.items()}, "env": sorted(os.environ),
                  "pid1": open("/proc/1/comm").read().strip(), "cwd": os.getcwd()}))
"""


def test_sandbox_hides_home_and_the_eval(tmp_path, tc):
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "probe.py").write_text(SCRIPT)
    home = tmp_path / "home"
    home.mkdir()
    ring = KeyRing({"p": "sk-FAKE-probe-9a8b7c6d5e4f"})
    spec = SandboxSpec(workspace=ws, ws_inside="/sandbox/work/ws", home=home, env={"CRAZE_HOME": "/sandbox/home/.craze"})
    targets = {"home": "/home", "owner": str(Path.home()), "tasks": str(paths.TASKS_DIR), "tmp_path": str(tmp_path),
               "plan": str(paths.PLAN_DIR), "usr_local_craze": "/usr/local/bin/craze"}
    out, err = tmp_path / "out", tmp_path / "err"
    r = asyncio.run(run_sandboxed(spec, tc, ["python", "probe.py", json.dumps(targets)], stdout=out, stderr=err,
                                  timeout=60, keyring=ring))
    assert r.exit_code == 0, err.read_text()
    got = json.loads(out.read_text())
    assert got["reach"] == {k: False for k in targets}
    assert got["cwd"] == "/sandbox/work/ws"
    assert got["pid1"] == "bwrap"  # a PID namespace of its own
    assert "CRAZE_HOME" in got["env"] and "SSH_AUTH_SOCK" not in got["env"]
    assert r.quiescent and r.leftover_pids == []


def test_timeout_kills_the_whole_tree(tmp_path, tc):
    ws = tmp_path / "ws"
    ws.mkdir()
    spec = SandboxSpec(workspace=ws, ws_inside="/sandbox/work/ws", home=None)
    # A setsid'd grandchild that would outlive a process-group kill.
    cmd = ["bash", "-c", "setsid sleep 300 & sleep 300"]
    r = asyncio.run(run_sandboxed(spec, tc, cmd, stdout=tmp_path / "o", stderr=tmp_path / "e", timeout=2))
    assert r.timed_out
    assert r.quiescent and r.leftover_pids == []


def test_bwrap_argv_shape(tmp_path):
    tc = Toolchains(go_root=Path("/g"), venv=Path("/v"), uv=Path("/u"), gomodcache=Path("/m"), node_root=Path("/n"))
    private = tmp_path / "gocache"
    spec = SandboxSpec(workspace=tmp_path / "ws", ws_inside="/sandbox/work/x", home=tmp_path / "h", tools={"node"},
                       relay=Relay(port=4321, socket=tmp_path / "proxy.sock"), gocache=private)
    env = build_env(spec)
    a = bwrap_argv(spec, tc, ["true"], env)
    for flag in ("--die-with-parent", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--new-session", "--clearenv",
                 "--unshare-net"):
        assert flag in a
    joined = " ".join(a)
    assert "--ro-bind /m /sandbox/gomodcache" in joined
    # An agent run's Go cache is its own (there is no shared cache to bind at all).
    assert f"--bind {private} /sandbox/gocache" in joined and not hasattr(tc, "gocache")
    assert "--ro-bind /n /sandbox/tools/node" in joined and "--ro-bind /v /sandbox/tools/venv" in joined
    assert f"--bind {tmp_path / 'ws'} /sandbox/work/x" in joined
    # The relay: the proxy socket bound in, the harness run under it, no resolver.
    assert f"--bind {tmp_path / 'proxy.sock'} /sandbox/proxy.sock" in joined
    assert a[a.index("--") + 1 :][:4] == ["/sandbox/tools/venv/bin/python", "-I", "-S", "/sandbox/relay.py"]
    assert a[-1] == "true" and "/run/systemd/resolve" not in joined
    # Nothing under /home is bound (the test's own tmp_path and the relay source aside).
    from crazeeval.sandbox import RELAY_SOURCE

    assert not [x for x in a if x.startswith("/home") and not x.startswith(str(tmp_path)) and x != str(RELAY_SOURCE)]
    assert "--ro-bind /etc /etc" in joined and "--tmpfs /tmp" in joined
    with pytest.raises(ValueError):
        bwrap_argv(SandboxSpec(workspace=tmp_path, ws_inside="/w", home=None, network=True,
                               relay=Relay(port=1, socket=tmp_path / "s")), tc, ["true"], env)


def test_relay_is_the_only_way_out(tmp_path, tc):
    """Review r1-c1 finding 12: an agent sandbox reaches the proxy socket through the
    relay on 127.0.0.1:<port>, and nothing else -- no DNS, no outside address."""
    import asyncio

    sock = tmp_path / "proxy.sock"

    async def answer(reader, writer):
        await reader.readuntil(b"\r\n\r\n")
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nrelay-ok")
        await writer.drain()
        writer.close()

    script = (
        "import socket, urllib.request, json\n"
        "out = {}\n"
        "out['relay'] = urllib.request.urlopen('http://127.0.0.1:4321/r/x', timeout=10).read().decode()\n"
        "def c(h, p):\n"
        "    try:\n"
        "        socket.create_connection((h, p), timeout=3).close(); return 'connected'\n"
        "    except OSError as e: return type(e).__name__\n"
        "out['out'] = c('1.1.1.1', 443)\n"
        "try:\n"
        "    socket.getaddrinfo('api.meta.ai', 443); out['dns'] = 'resolved'\n"
        "except OSError as e: out['dns'] = type(e).__name__\n"
        "print(json.dumps(out))\n"
    )
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "p.py").write_text(script)

    async def go():
        server = await asyncio.start_unix_server(answer, path=str(sock))
        try:
            spec = SandboxSpec(workspace=ws, ws_inside="/sandbox/work/ws", home=None,
                               relay=Relay(port=4321, socket=sock))
            return await run_sandboxed(spec, tc, ["python", "p.py"], stdout=tmp_path / "o", stderr=tmp_path / "e",
                                       timeout=60)
        finally:
            server.close()

    r = asyncio.run(go())
    assert r.exit_code == 0, (tmp_path / "e").read_text()
    got = json.loads((tmp_path / "o").read_text())
    assert got == {"relay": "relay-ok", "out": "OSError", "dns": "gaierror"}


def test_go_module_boundary():
    """`go list ./...` from the repo root lists nothing under eval/ (AC-A7)."""
    go = shutil.which("go") or str(Toolchains.resolve(need_venv=False).go_root / "bin/go")
    if not os.path.exists(go):
        pytest.skip("no go toolchain")
    env = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "GOPATH", "GOMODCACHE", "GOCACHE", "GOFLAGS")}
    env["GOTOOLCHAIN"] = "local"
    r = subprocess.run([go, "list", "./..."], cwd=paths.REPO_ROOT, capture_output=True, text=True, env=env, timeout=300)
    assert r.returncode == 0, r.stderr[-2000:]
    assert [line for line in r.stdout.splitlines() if "/eval/" in line or line.endswith("/eval")] == []
    assert (paths.EVAL_DIR / "go.mod").exists()
