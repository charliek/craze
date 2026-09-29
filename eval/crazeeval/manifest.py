"""The provenance manifest every batch writes (plan 029 §3.1.9)."""

from __future__ import annotations

import subprocess
import tempfile
import tomllib
from pathlib import Path

from crazeeval import paths
from crazeeval.config import EvalModel, Snapshot
from crazeeval.homes import HOMES, generated_files
from crazeeval.sandbox import Toolchains
from crazeeval.workspace import craze_template, fixture_hash, git, sha256_file, template_check

VERSION_ENV_BASE = {"LANG": "C.UTF-8", "TERM": "dumb"}


def _version(argv: list[str], path_dirs: list[str], home: str) -> str | None:
    env = dict(VERSION_ENV_BASE, PATH=":".join(path_dirs + ["/usr/bin", "/bin"]), HOME=home)
    try:
        r = subprocess.run(argv, capture_output=True, env=env, timeout=60)
        out = (r.stdout or r.stderr).decode(errors="replace").strip()
        return out.splitlines()[0][:200] if out else None
    except (OSError, subprocess.SubprocessError):
        return None


def _harness_binary(path: Path, home: str, argv: list[str] | None = None, path_dirs: list[str] | None = None) -> dict:
    version = _version(argv or [str(path), "--version"], path_dirs or [], home)
    return {"path": str(path), "sha256": sha256_file(path), "version": version}


def executables(tc: Toolchains, craze_bin: Path | None) -> dict:
    out = {}
    with tempfile.TemporaryDirectory(prefix="crazeeval-version-") as home:
        if craze_bin:
            repo = paths.REPO_ROOT
            out["craze"] = {
                **_harness_binary(craze_bin, home),
                "repo_head": git(repo, "rev-parse", "HEAD", check=False).strip(),
                "repo_dirty": bool(git(repo, "status", "--porcelain", "--", ".", ":(exclude)eval", check=False).strip()),
            }
        if tc.grok_dir:
            out["gx"] = _harness_binary(tc.grok_dir / "gx", home)
        if tc.opencode_dir:
            out["opencode"] = _harness_binary(tc.opencode_dir / "opencode", home)
        if tc.node_root:
            node_bin = [str(tc.node_root / "bin")]
            out["codex"] = _harness_binary((tc.node_root / "bin" / "codex").resolve(), home, ["codex", "--version"], node_bin)
            out["node"] = {"path": str(tc.node_root / "bin/node"), "version": _version(["node", "--version"], node_bin, home)}
        go = tc.go_root / "bin/go"
        out["go"] = {"path": str(go), "version": _version([str(go), "version"], [], home)}
        out["bwrap"] = {"version": _version(["bwrap", "--version"], [], home)}
        out["uv"] = {"path": str(tc.uv), "version": _version([str(tc.uv), "--version"], [], home)}
        out["tool_venv_pytest"] = {"version": _version([str(tc.venv / "bin/python"), "-m", "pytest", "--version"], [], home)}
    return out


def sample_configs(models: list[EvalModel], harnesses: list[str], snap: Snapshot) -> dict:
    """The generated configs (they hold no secrets), with a placeholder route."""
    out = {}
    with tempfile.TemporaryDirectory(prefix="crazeeval-configs-") as td:
        for em in models:
            for h in harnesses:
                if not em.supports(h):
                    continue
                home = Path(td) / h / em.slug
                base = f"http://127.0.0.1:PORT/r/TOKEN/<{em.provider}-upstream>"
                HOMES[h](home, em, snap, base)
                out[f"{h}/{em.key}"] = {
                    str(p.relative_to(home)): p.read_text(errors="replace") for p in generated_files(home)
                }
    return out


def _opencode_model(em: EvalModel, snap: Snapshot) -> dict:
    m = snap.model(em.key)
    return {"id": em.opencode, **(m.get("opencode_check") or {}), "variants": m.get("opencode_variants")}


def build(
    *,
    label: str,
    tc: Toolchains,
    craze_bin: Path | None,
    models: list[EvalModel],
    harnesses: list[str],
    tasks: list,
    snap: Snapshot,
    route_table: list[dict],
    run_order: list[str],
    prices_path: Path | None = None,
    executables_info: dict | None = None,
) -> dict:
    prices_path = prices_path or paths.PRICES_FILE
    with open(prices_path, "rb") as f:
        prices = tomllib.load(f)
    fixtures = sorted({t.fixture for t in tasks if t.repo_kind == "fixture"})
    # One template per craze commit the tasks use (``[repo] commit``, default 3eabb31).
    by_commit: dict[str, list[str]] = {}
    for t in tasks:
        if t.repo_kind == "craze":
            by_commit.setdefault(t.craze_commit, []).append(t.id)
    templates = []
    for commit, ids in sorted(by_commit.items()):
        d = craze_template(commit)
        templates.append({"commit": commit, "path": str(d), "isolation": template_check(d), "tasks": sorted(ids)})
    # ``craze_template``: the one template when every craze task shares a commit (the
    # field's shape before per-task commits); ``craze_templates`` lists them all.
    tmpl = {k: templates[0][k] for k in ("commit", "path", "isolation")} if len(templates) == 1 else None
    return {
        "label": label,
        "executables": executables_info if executables_info is not None else executables(tc, craze_bin),
        "toolchains": tc.describe(),
        "config_snapshot": {"dir": str(snap.dir), "hash": snap.hash},
        "generated_configs": sample_configs(models, harnesses, snap),
        "opencode_models": {m.key: _opencode_model(m, snap) for m in models},
        "fixtures": {name: fixture_hash(name) for name in fixtures},
        "craze_template": tmpl,
        "craze_templates": templates,
        "tasks": {t.id: {"split": t.split, "category": t.category, "mode": t.mode,
                         **({"craze_commit": t.craze_commit} if t.repo_kind == "craze" else {})} for t in tasks},
        "prices": prices,
        "judge": None,
        "run_order": run_order,
        "route_table": route_table,
    }
