"""The sandbox probe (plan 029 AC-A3): from inside a sandbox built exactly as an agent
run's (its own network namespace, the relay to the proxy socket, a private Go cache),
the owner's home, eval/tasks and the plan folder are unreachable, no key variable is
set, nothing outside is reachable over the network, and the proxy is (through the
relay on 127.0.0.1:<port>)."""

from __future__ import annotations

import asyncio
import json
import shutil
import tempfile
from pathlib import Path

from crazeeval import keys as keymod
from crazeeval import paths
from crazeeval.config import EvalModel, Snapshot
from crazeeval.homes import HOMES
from crazeeval.sandbox import Relay, SandboxSpec, Toolchains, run_sandboxed
from crazeeval.workspace import craze_template

PROBE_SCRIPT = r'''
import json, os, re, shutil, socket, subprocess, sys
targets = json.loads(sys.argv[1])
out = {}
def exists(p):
    try:
        os.stat(p)
        return True
    except OSError:
        return False
out["reachable"] = {name: exists(p) for name, p in targets.items()}
names = sorted(os.environ)
out["env_names"] = names
out["credential_like_names"] = [n for n in names if re.search("KEY|TOKEN|SECRET", n, re.I) and n != "CRAZE_EVAL_DUMMY"]
with open(os.path.join(os.environ["HOME"], "env.dump"), "wb") as f:
    for k, v in os.environ.items():
        f.write(k.encode() + b"=" + v.encode() + b"\0")
def run(cmd, timeout=900):
    try:
        r = subprocess.run(cmd, capture_output=True, timeout=timeout, text=True)
        return {"exit": r.returncode, "tail": (r.stdout + r.stderr)[-300:]}
    except Exception as e:
        return {"error": repr(e)}
out["go_version"] = run(["go", "version"])
out["go_build"] = run(["go", "build", "./..."]) if exists("go.mod") else None
out["pytest"] = run(["python", "-m", "pytest", "--version"])
out["which"] = {t: shutil.which(t) for t in ["craze", "gx", "opencode", "codex", "node", "python", "pytest", "uv", "go", "rg", "git", "bash"]}
try:
    out["dns_api_meta_ai"] = bool(socket.getaddrinfo("api.meta.ai", 443))
except Exception:
    out["dns_api_meta_ai"] = False
def connect(host, port):
    try:
        socket.create_connection((host, port), timeout=5).close()
        return "connected"
    except Exception as e:
        return type(e).__name__ + ": " + str(e)[:80]
out["egress_1.1.1.1:443"] = connect("1.1.1.1", 443)
out["egress_8.8.8.8:53"] = connect("8.8.8.8", 53)
import urllib.request
try:
    with urllib.request.urlopen("http://127.0.0.1:%s/relay-check" % os.environ["PROBE_PORT"], timeout=10) as r:
        out["relay"] = r.read().decode()
except Exception as e:
    out["relay"] = "failed: " + repr(e)[:200]
out["pid1"] = open("/proc/1/comm").read().strip()
out["pids"] = len([p for p in os.listdir("/proc") if p.isdigit()])
out["usr_local_bin"] = os.listdir("/usr/local/bin") if exists("/usr/local/bin") else None
out["root_entries"] = sorted(os.listdir("/"))
print(json.dumps(out))
'''


async def run_probe(out: Path, tc: Toolchains, keyring: keymod.KeyRing, snap: Snapshot, em: EvalModel,
                    craze_bin: Path | None, cache: Path | None = None) -> dict:
    out.mkdir(parents=True, exist_ok=True)
    ws = out / "ws" / "craze"
    home = out / "home"
    shutil.rmtree(out / "ws", ignore_errors=True)
    shutil.rmtree(home, ignore_errors=True)
    shutil.copytree(craze_template(cache=cache), ws, symlinks=True)
    home.mkdir(parents=True)
    # Every harness's variables, exactly as a run would set them.
    env: dict[str, str] = {}
    for h, fn in HOMES.items():
        if em.supports(h):
            env.update(fn(home, em, snap, f"http://127.0.0.1:1/r/probe/{em.provider}"))
    (home / "probe.py").write_text(PROBE_SCRIPT)
    tools = {"craze", "grok", "opencode", "node"} if craze_bin else {"grok", "opencode", "node"}
    # A stand-in proxy on a Unix socket: the relay must reach it and nothing else.
    sockdir = Path(tempfile.mkdtemp(prefix="crazeeval-probe-"))
    sock = sockdir / "proxy.sock"
    port = 18080

    async def answer(reader, writer):
        await reader.readuntil(b"\r\n\r\n")
        body = b"relay-ok"
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s" % (len(body), body))
        await writer.drain()
        writer.close()

    server = await asyncio.start_unix_server(answer, path=str(sock))
    gocache = out / "gocache"
    shutil.rmtree(gocache, ignore_errors=True)
    gocache.mkdir()
    env["PROBE_PORT"] = str(port)
    spec = SandboxSpec(workspace=ws, ws_inside=f"{paths.SANDBOX_WORK}/craze", home=home, tools=tools, env=env,
                       network=False, relay=Relay(port=port, socket=sock), craze_bin=craze_bin, gocache=gocache)
    targets = {
        "/home": "/home",
        "owner home": str(Path.home()),
        "eval/tasks": str(paths.TASKS_DIR),
        "campaign folder": str(paths.campaign_dir()),
        "owner ~/.claude/plans": str(Path.home() / ".claude/plans"),
        "owner ~/.craze": str(Path.home() / ".craze"),
        "owner ~/.grok": str(Path.home() / ".grok"),
        "owner ~/.codex": str(Path.home() / ".codex"),
        "repo": str(paths.REPO_ROOT),
    }
    stdout, stderr = out / "probe.stdout", out / "probe.stderr"
    try:
        r = await run_sandboxed(spec, tc, ["python", f"{paths.SANDBOX_HOME}/probe.py", json.dumps(targets)],
                                stdout=stdout, stderr=stderr, timeout=900, keyring=keyring)
    finally:
        server.close()
        shutil.rmtree(sockdir, ignore_errors=True)
        shutil.rmtree(gocache, ignore_errors=True)
    try:
        inside = json.loads(stdout.read_text().strip().splitlines()[-1])
    except (ValueError, IndexError):
        inside = {"error": "no probe output", "stderr": stderr.read_text(errors="replace")[-2000:]}
    dump = home / "env.dump"
    env_key_hit = keyring.contains_key(dump.read_bytes()) if dump.exists() else None
    scan = keymod.scan_tree(out, keyring)
    reachable = inside.get("reachable") or {}
    result = {
        "exit": r.exit_code,
        "quiescent": r.quiescent,
        "inside": inside,
        "env_value_is_a_key": env_key_hit,
        "key_scan": {"files_scanned": scan["files_scanned"], "files_with_key": scan["files_with_key"]},
        "env_samples": r.env_samples,
        "unreachable_ok": bool(reachable) and not any(reachable.values()),
        "no_key_variable": env_key_hit is False and not inside.get("credential_like_names"),
        "egress_closed": not inside.get("dns_api_meta_ai")
        and all(not str(inside.get(k, "connected")).startswith("connected") for k in ("egress_1.1.1.1:443", "egress_8.8.8.8:53")),
        "relay_ok": inside.get("relay") == "relay-ok",
    }
    result["ok"] = (result["unreachable_ok"] and result["no_key_variable"] and not scan["files_with_key"]
                    and result["egress_closed"] and result["relay_ok"])
    (out / "probe.json").write_text(json.dumps(result, indent=2) + "\n")
    return result
