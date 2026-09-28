"""The opencode package seed (review r1-c1 finding 12).

Agent runs have no network. opencode's only fetch at start-up in this setup is an
npm install of ``@opencode-ai/plugin@<its version>`` into its config directory (the
provider SDKs are built in; the models catalog is the config snapshot's). So once per
opencode version the plugin is installed -- with network, in a sandbox, lifecycle
scripts off -- into ``~/.cache/crazeeval/opencode-seed/plugin-<version>/``, and every
run gets that ``package.json``, ``package-lock.json`` and a read-only
``node_modules`` (homes.opencode_seed_binds), which makes opencode skip its install.
"""

from __future__ import annotations

import fcntl
import json
import os
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

from crazeeval import paths
from crazeeval.sandbox import SandboxSpec, Toolchains, run_sandboxed_sync

PLUGIN = "@opencode-ai/plugin"


def opencode_version(tc: Toolchains) -> str:
    assert tc.opencode_dir is not None
    with tempfile.TemporaryDirectory(prefix="crazeeval-ocver-") as home:
        env = {"PATH": "/usr/bin:/bin", "HOME": home, "LANG": "C.UTF-8", "TMPDIR": home,
               **{f"XDG_{d}_HOME": f"{home}/{d.lower()}" for d in ("CONFIG", "DATA", "CACHE", "STATE")}}
        r = subprocess.run([str(tc.opencode_dir / "opencode"), "--version"], capture_output=True, timeout=60, env=env)
    v = (r.stdout or b"").decode().strip().splitlines()[-1] if r.stdout.strip() else ""
    if not re.fullmatch(r"\d+\.\d+\.\d+", v):
        raise RuntimeError(f"cannot read opencode's version ({v!r})")
    return v


def ensure_opencode_seed(tc: Toolchains, cache: Path | None = None) -> Path:
    version = opencode_version(tc)
    root = Path(cache or paths.CACHE_DIR) / "opencode-seed"
    dest = root / f"plugin-{version}"
    root.mkdir(parents=True, exist_ok=True)
    with open(root / f".plugin-{version}.lock", "w") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        if (dest / ".ready").exists():
            return dest
        tmp = root / f"plugin-{version}.building"
        shutil.rmtree(tmp, ignore_errors=True)
        tmp.mkdir()
        (tmp / "package.json").write_text(json.dumps({"dependencies": {PLUGIN: version}}, indent=2) + "\n")
        spec = SandboxSpec(workspace=tmp, ws_inside=f"{paths.SANDBOX_WORK}/seed", home=None, tools={"node"},
                           network=True)
        cmd = ["npm", "install", "--ignore-scripts", "--no-audit", "--no-fund", "--cache", "/tmp/npm-cache"]
        r = run_sandboxed_sync(spec, tc, cmd, stdout=root / "seed.stdout", stderr=root / "seed.stderr", timeout=600)
        lock_doc = json.loads((tmp / "package-lock.json").read_text()) if (tmp / "package-lock.json").exists() else {}
        locked = ((lock_doc.get("packages") or {}).get("") or {}).get("dependencies") or {}
        if r.exit_code != 0 or PLUGIN not in locked or not (tmp / "node_modules" / PLUGIN).is_dir():
            raise RuntimeError(f"opencode seed install failed (exit {r.exit_code}); see {root / 'seed.stderr'}")
        shutil.rmtree(dest, ignore_errors=True)
        os.replace(tmp, dest)
        (dest / ".ready").write_text(version + "\n")
    return dest
