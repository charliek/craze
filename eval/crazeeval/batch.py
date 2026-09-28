"""``crazeeval run``: a batch of sandboxed runs through one in-process proxy.

Each run: materialise the workspace, register a route (random token, target model
only), generate the harness home, run it under bwrap in its own network namespace
(the proxy's Unix socket, relayed to 127.0.0.1:<port>, is the only way out) with a
private Go cache, close the route, extract the answer (no link followed), score it,
run post-run git in a sandbox, scan the capture for contamination and every file for
keys, and write ``result.json``. Infrastructure failures are re-run up to twice; a
contaminated run once (plan 029 §3.1.5, §3.1.8).

A batch directory is created exclusively and locked for the batch's life; nothing in
it is ever deleted and re-made. ``--resume`` reopens one whose identity fingerprint
matches (task definitions and testdata, fixtures, the config snapshot, effective model
settings and prices, every executable's hash and version, execution settings, the
evaluator's own code) and runs only what has no final result, in fresh attempt
directories.
"""

from __future__ import annotations

import asyncio
import dataclasses
import fcntl
import fnmatch
import hashlib
import json
import shutil
import tempfile
import time
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path

from crazeeval import capture as cap
from crazeeval import keys as keymod
from crazeeval import manifest as manifestmod
from crazeeval import paths, safefs
from crazeeval.checks import ScoreContext, contamination_scan, objective_pass, run_checks
from crazeeval.config import EvalModel, Snapshot
from crazeeval.gitpost import add_errors_benign, git_postrun
from crazeeval.ledger import BUDGET_CAPPED, BUDGET_STOP, Ledger
from crazeeval.pricing import Price
from crazeeval.proxy import KEY_EXPOSURE, Proxy
from crazeeval.runners import RUNNERS
from crazeeval.runners.base import words
from crazeeval.sandbox import Relay, SandboxSpec, Toolchains, run_sandboxed
from crazeeval.tasks import Task
from crazeeval.validate import validate_task
from crazeeval.workspace import build_manifest, compare, fixture_hash, ignores_for, materialise, oversize

INFRA_RERUNS = 2
CONTAMINATION_RERUNS = 1
DEFAULT_CAPS = {"zai-coding-plan": 2}
DEFAULT_CAP_OTHER = 3
OVERSIZE = "oversize"
TERMINAL = {"ok", "crashed", "timeout", "contaminated", KEY_EXPOSURE, BUDGET_CAPPED, BUDGET_STOP, "infra", "launch-error",
            OVERSIZE}
# The statuses objective_pass counts as a crash.
CRASHED = ("crashed", "infra", "launch-error")


def run_objective_pass(checks: list[dict], timed_out: bool, status: str | None) -> bool:
    """A run's ``objective_pass``: every check passed, no timeout, no crash, and the
    status ``ok`` (so a key-exposure run never passes). ``crazeeval rescore`` recomputes
    it with this too."""
    return objective_pass(checks, timed_out, status in CRASHED) and status == "ok"


class BatchDirError(RuntimeError):
    pass


@dataclass
class RunSpec:
    harness: str
    em: EvalModel
    task: Task
    rep: int

    @property
    def key(self) -> str:
        return f"{self.harness}/{self.em.slug}/{self.task.id}/rep{self.rep}"


@dataclass
class BatchConfig:
    harnesses: list[str]
    models: list[EvalModel]
    tasks: list[Task]
    reps: int
    out: Path
    label: str
    snap: Snapshot
    first_rep: int = 1
    craze_bin: Path | None = None
    parallel: int = 4
    caps: dict[str, int] = field(default_factory=dict)
    cap_other: int = DEFAULT_CAP_OTHER
    ledger_path: Path = paths.DEFAULT_LEDGER
    budget_cap: float = 95.0
    run_cap: float = 3.0
    timeout_s: int | None = None
    cache: Path | None = None
    resume: bool = False
    opencode_seed: Path | None = None


def size_check(ws: Path, file_limit: int = safefs.FILE_LIMIT, tree_limit: int = safefs.TREE_LIMIT) -> tuple[list[str], int]:
    """The workspace's apparent size -- an ``lstat`` walk, no file reads (safefs's
    walker and limits) -- checked before the final manifest is ever built or hashed
    (review r3-c1 item G): an over-limit tree fails as oversize without hashing it.
    Returns the oversize notes (if any) and the tree's total apparent bytes."""
    stats = safefs.tree_stats(ws, file_limit)
    too_big = [f"<file: {o['path']} ({o['bytes']} bytes)>" for o in stats["over_file_limit"]]
    if stats["bytes"] > tree_limit:
        too_big.append(f"<workspace: {stats['bytes']} bytes>")
    return too_big, stats["bytes"]


def final_manifest(ws: Path, start_manifest: dict[str, str], ignores: list[str],
                    file_limit: int = safefs.FILE_LIMIT, tree_limit: int = safefs.TREE_LIMIT
                    ) -> tuple[dict[str, str], dict[str, list[str]], list[str], int]:
    """The run's final manifest and diff -- or, when the workspace is oversize, neither
    (review r3-c1 item G): the cheap aggregate size check runs first, and the manifest
    is only built and hashed when the tree fits. Returns ``(final, diff, too_big,
    workspace_bytes)``."""
    too_big, workspace_bytes = size_check(ws, file_limit, tree_limit)
    if too_big:
        return {}, {"added": [], "modified": [], "deleted": []}, too_big, workspace_bytes
    final = build_manifest(ws)
    diff = compare(start_manifest, final, ignores)
    too_big = oversize(final)  # defense in depth: a file build_manifest itself finds oversize
    return final, diff, too_big, workspace_bytes


def tree_hash(root: Path) -> str:
    """sha256 over every file under ``root`` (relative path and bytes), sorted."""
    h = hashlib.sha256()
    for p in sorted(Path(root).rglob("*")):
        if p.is_file() and "__pycache__" not in p.parts:
            h.update(p.relative_to(root).as_posix().encode() + b"\0" + p.read_bytes() + b"\0")
    return h.hexdigest()


def batch_identity(cfg: BatchConfig, executables: dict, prices: dict) -> dict:
    """Everything that makes two batches' results comparable, with its fingerprint
    (review r2-c1 item 16). A resumed batch must match it exactly."""
    wires = sorted({m.wire_model for m in cfg.models})
    ident = {
        "label": cfg.label,
        "harnesses": sorted(cfg.harnesses),
        "reps": cfg.reps,
        # Only when a batch numbers its reps from something other than 1 (a later
        # batch adding a rep to an earlier one) -- so a default batch's identity
        # FIELDS are what they were before this field's addition. The fingerprint
        # below also hashes the evaluator's own source tree (see "evaluator"), so
        # it -- and --resume, which requires an exact fingerprint match -- already
        # changes with any change under eval/, this one included.
        **({"first_rep": cfg.first_rep} if cfg.first_rep != 1 else {}),
        # Task definitions with their testdata (prompts, checks, hidden tests,
        # reference patches, setup patches, timeouts) and the fixtures they use.
        "tasks": {t.id: tree_hash(t.dir) for t in cfg.tasks},
        "fixtures": {f: fixture_hash(f) for f in sorted({t.fixture for t in cfg.tasks if t.fixture})},
        "config_snapshot": cfg.snap.hash,
        # Effective model settings: the eval table's entry (ids, effort), the price the
        # ledger charges, and the output maximum the proxy reserves against.
        "models": {m.key: dataclasses.asdict(m) for m in sorted(cfg.models, key=lambda m: m.key)},
        "prices": {w: dataclasses.asdict(prices[w]) for w in wires if w in prices},
        "max_output": cfg.snap.max_output(cfg.models),
        "executables": {k: {"sha256": v.get("sha256"), "version": v.get("version")} for k, v in sorted(executables.items())},
        # The seed's contents, not just its name (review r3-c1 item H): a resumed run
        # mounts the seed's node_modules and copies its package files, so a change
        # under the same directory name must not pass resume validation unnoticed.
        "opencode_seed": tree_hash(cfg.opencode_seed) if cfg.opencode_seed else None,
        # ``parallel`` and the per-provider concurrency caps affect concurrent
        # execution (review r3-c1 item H) and so belong in what makes two batches'
        # results comparable.
        "execution": {"timeout_s": cfg.timeout_s, "run_cap": cfg.run_cap, "budget_cap": cfg.budget_cap,
                      "parallel": cfg.parallel, "caps": dict(sorted(cfg.caps.items())), "cap_other": cfg.cap_other},
        # The evaluator itself: its code and its tables.
        "evaluator": tree_hash(Path(__file__).parent),
        "eval_tables": {n: manifestmod.sha256_file(paths.EVAL_DIR / n) for n in ("models.toml", "prices.toml")},
    }
    body = json.dumps(ident, sort_keys=True, default=str).encode()
    ident["fingerprint"] = hashlib.sha256(body).hexdigest()
    return ident


def identity_diff(saved: dict, mine: dict) -> list[str]:
    out = []
    for k in sorted(set(saved) | set(mine)):
        if k == "fingerprint" or saved.get(k) == mine.get(k):
            continue
        a, b = saved.get(k), mine.get(k)
        if isinstance(a, dict) and isinstance(b, dict):
            out += [f"{k}.{sub}" for sub in sorted(set(a) | set(b)) if a.get(sub) != b.get(sub)]
        else:
            out.append(k)
    return out


def plan_runs(cfg: BatchConfig) -> list[RunSpec]:
    runs = []
    ordered = [m for m in cfg.models if not m.last] + [m for m in cfg.models if m.last]
    for em in ordered:
        for task in cfg.tasks:
            for h in cfg.harnesses:
                if not RUNNERS[h].supports(em, task):
                    continue
                for rep in range(cfg.first_rep, cfg.first_rep + cfg.reps):
                    runs.append(RunSpec(h, em, task, rep))
    return runs


def run_dir(cfg: BatchConfig, spec: RunSpec) -> Path:
    part = "heldout" if spec.task.split == "heldout" else "runs"
    return cfg.out / part / spec.harness / spec.em.slug / spec.task.id / f"rep{spec.rep}"


def _json_write(p: Path, obj) -> None:
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_name(p.name + ".tmp")
    tmp.write_text(json.dumps(obj, indent=2, sort_keys=True, default=str) + "\n")
    tmp.replace(p)


def _jsonl_append(p: Path, obj) -> None:
    p.parent.mkdir(parents=True, exist_ok=True)
    with open(p, "a") as f:
        f.write(json.dumps(obj) + "\n")


def open_batch_dir(cfg: BatchConfig, identity: dict):
    """Create ``cfg.out`` exclusively -- or, with ``resume``, reopen one whose saved
    identity has the same fingerprint -- and hold its lock. Returns the open lock file
    (keep it open for the batch's life). Review r1-c1 finding 18, r2-c1 item 16."""
    out = cfg.out
    if cfg.resume:
        if not out.is_dir():
            raise BatchDirError(f"--resume: {out} does not exist")
        try:
            saved = json.loads((out / "batch.json").read_text())
        except (OSError, ValueError) as e:
            raise BatchDirError(f"--resume: {out} has no readable batch.json") from e
        if saved.get("fingerprint") != identity["fingerprint"]:
            raise BatchDirError(f"--resume: {out} was a different batch (differs in {identity_diff(saved, identity)})")
    else:
        try:
            out.mkdir(parents=True, exist_ok=False)
        except FileExistsError as e:
            raise BatchDirError(f"{out} already exists: choose another --out, or --resume it") from e
    lock = open(out / ".lock", "a")
    try:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as e:
        lock.close()
        raise BatchDirError(f"{out} is in use by another crazeeval process") from e
    if not cfg.resume:
        _json_write(out / "batch.json", identity)
    return lock


def _next_attempt(rdir: Path) -> int:
    n = 0
    if rdir.is_dir():
        for p in rdir.iterdir():
            if p.name.startswith("attempt-") and p.name[8:].isdigit():
                n = max(n, int(p.name[8:]))
    return n


def done_result(rdir: Path) -> dict | None:
    """A run's final result, when a previous (resumed) batch finished it."""
    try:
        res = json.loads((rdir / "result.json").read_text())
    except (OSError, ValueError):
        return None
    return res if res.get("status") in TERMINAL else None


class Batch:
    def __init__(self, cfg: BatchConfig, providers, keyring: keymod.KeyRing, prices: dict[str, Price], tc: Toolchains):
        self.cfg = cfg
        self.providers = providers
        self.keyring = keyring
        self.prices = prices
        self.tc = tc
        self.ledger = Ledger(cfg.ledger_path, cap=cfg.budget_cap, run_cap=cfg.run_cap, scrub=keyring.scrub)
        self.proxy = Proxy(providers, keyring, self.ledger, prices, refusal_log=cfg.out / "proxy-refusals.jsonl",
                           max_output=cfg.snap.max_output(cfg.models))
        # The proxy's Unix socket, relayed into every sandbox (a short path: sockets
        # are limited to ~108 bytes; the directory is private to this user).
        self.sockdir = Path(tempfile.mkdtemp(prefix="crazeeval-"))
        self.socket = self.sockdir / "proxy.sock"
        self.results: list[dict] = []
        self._sem = asyncio.Semaphore(max(1, cfg.parallel))
        self._provider_sems: dict[str, asyncio.Semaphore] = {}
        self.stop_reason: str | None = None

    async def start(self) -> None:
        await self.proxy.start(unix_path=self.socket)

    async def stop(self) -> None:
        await self.proxy.stop()
        shutil.rmtree(self.sockdir, ignore_errors=True)

    def _psem(self, provider: str) -> asyncio.Semaphore:
        if provider not in self._provider_sems:
            n = self.cfg.caps.get(provider, DEFAULT_CAPS.get(provider, self.cfg.cap_other))
            self._provider_sems[provider] = asyncio.Semaphore(max(1, n))
        return self._provider_sems[provider]

    # -- one attempt -------------------------------------------------------------

    async def attempt(self, spec: RunSpec, adir: Path, n: int) -> dict:
        cfg = self.cfg
        runner = RUNNERS[spec.harness]
        task, em = spec.task, spec.em
        ws_inside = f"{paths.SANDBOX_WORK}/{task.repo_name}"
        ws = adir / "ws" / task.repo_name
        home = adir / "home"
        home.mkdir(parents=True)
        # Each agent run's own Go cache: never shared with another run or with scoring.
        gocache = adir / "gocache"
        gocache.mkdir()
        ignores = ignores_for(task, runner.workspace_state)
        start = await asyncio.to_thread(materialise, task, ws, cfg.cache)
        _json_write(adir / "start-manifest.json", {"commit": start.commit, "files": start.manifest, "isolation": start.isolation})
        # Unique per batch (the batch directory carries its timestamp), so neither the
        # ledger's per-run grouping nor the per-run cap mixes two batches that share a label.
        run_id = f"{cfg.out.name}:{spec.key}:a{n}"
        route = self.proxy.register_run(run_id, spec.harness, {em.wire_model}, adir / cap.CAPTURE_PLAIN)
        stdout, stderr = adir / "stdout.jsonl", adir / "stderr.txt"
        launch_error = None
        cmd: list[str] = []
        sres = None
        try:
            base_url = self.proxy.base_url(route, em.provider)
            env = runner.home(home, em, cfg.snap, base_url)
            cmd = runner.command(em, task, ws_inside, cfg.snap)
            sspec = SandboxSpec(
                workspace=ws,
                ws_inside=ws_inside,
                home=home,
                tools=set(runner.tools),
                env=env,
                network=False,
                relay=Relay(port=self.proxy.port, socket=self.socket),
                extra_ro=runner.extra_ro(cfg.snap),
                home_ro=runner.seed_binds(home, cfg.opencode_seed),
                goflags=task.goflags,
                craze_bin=cfg.craze_bin,
                gocache=gocache,
            )
            sres = await run_sandboxed(
                sspec, self.tc, cmd, stdout=stdout, stderr=stderr,
                timeout=cfg.timeout_s or task.timeout_s, keyring=self.keyring,
            )
        except Exception as e:  # a home or sandbox that cannot start is an infra failure
            launch_error = f"{type(e).__name__}: {e}"
        finally:
            # The route closes (its token stops working) however the run ended.
            proxy_summary = await self.proxy.end_run(route)
        records = await asyncio.to_thread(cap.read_capture, adir)
        ext = await asyncio.to_thread(runner.extract, stdout, home, task)
        # A file over the per-file limit, or a tree over the total, fails the run as
        # oversize before anything is copied or hashed further (review r2-c1 item 14,
        # r3-c1 item G): the cheap lstat-only size check runs before the final
        # manifest is built, so an oversize tree is never hashed.
        final, diff, too_big, workspace_bytes = await asyncio.to_thread(final_manifest, ws, start.manifest, ignores)
        _json_write(adir / "final-manifest.json", {"files": final, "diff": diff})
        ctx = ScoreContext(
            task=task, ws=ws, start=start, answer=ext.answer, records=records, scoring_dir=adir / "scoring",
            tc=self.tc, ignores=ignores, ws_inside=ws_inside, cache=cfg.cache, keyring=self.keyring,
            _final=final,
        )
        post: dict = {}
        checks: list[dict] = []
        check_error = None
        evidence_error = None
        gitpost_note_ = None
        if not too_big:
            try:
                pristine = await asyncio.to_thread(ctx.pristine)
                post = await git_postrun(ws, ws_inside, start.commit, pristine, adir / "gitpost", self.tc,
                                         keyring=self.keyring)
                (adir / "diff.patch").write_text(post.get("diff", ""))
                evidence_error = gitpost_problem(post)
                gitpost_note_ = gitpost_note(post) if evidence_error is None else None
            except Exception as e:
                evidence_error = f"post-run git: {type(e).__name__}: {e}"
            if sres is not None and evidence_error is None:
                try:
                    checks = await run_checks(ctx)
                except safefs.Oversize as e:
                    too_big.append(str(e)[:300])
                except Exception as e:
                    check_error = f"{type(e).__name__}: {e}"
        await asyncio.to_thread(_prune_scoring, adir / "scoring")
        contamination = contamination_scan(records, ws_inside)
        metrics = cap.metrics(records)
        metrics["roundtrips"] = tool_roundtrips(records)
        status = classify(sres, launch_error, proxy_summary, ext, records, contamination, check_error,
                          evidence_error=evidence_error, oversize=too_big)
        res = {
            "label": cfg.label,
            "run_key": spec.key,
            "run_id": run_id,
            "attempt": n,
            "harness": spec.harness,
            "model": em.key,
            "wire_model": em.wire_model,
            "effort": em.effort,
            "task": task.id,
            "category": task.category,
            "split": task.split,
            "mode": task.mode,
            "plan_mode": runner.plan_mode(task),
            "rep": spec.rep,
            "command": _redact_cmd(cmd, task.prompt),
            "exit": sres.exit_code if sres else None,
            "timed_out": bool(sres and sres.timed_out),
            "wall_s": sres.wall_s if sres else None,
            "answer": ext.answer,
            "answer_words": words(ext.answer),
            "completed": ext.completed,
            "failure": ext.failure,
            "notes": ext.notes + ([launch_error] if launch_error else []) + ([f"check error: {check_error}"] if check_error else [])
            + ([f"evidence error: {evidence_error}"] if evidence_error else []) + ([f"oversize: {too_big[:5]}"] if too_big else [])
            + ([gitpost_note_] if gitpost_note_ else []),
            "extract": ext.extra,
            "status": status,
            "objective_pass": run_objective_pass(checks, bool(sres and sres.timed_out), status),
            "checks": checks,
            "contamination": contamination,
            "metrics": metrics,
            "proxy": proxy_summary,
            "sandbox": {
                "quiescent": sres.quiescent if sres else None,
                "leftover_pids": sres.leftover_pids if sres else None,
                "env_samples": sres.env_samples if sres else None,
                "tools": sres.argv_tools if sres else None,
                "network": "netns + relay to the proxy socket",
            },
            "start_commit": start.commit,
            "head": {k: post.get(k) for k in ("head", "moved", "tree_diff")},
            "diff_bytes": post.get("diff_bytes"),
            "gitpost": {k: post.get(k) for k in ("exit", "timed_out", "diff_rc", "add_rc", "add_errors")},
            "workspace_bytes": workspace_bytes,
            "diff": {k: len(v) for k, v in diff.items()},
            "changed": diff,
            "config_snapshot": cfg.snap.hash,
        }
        _json_write(adir / "result.json", res)
        # Every file of the run -- caches included -- is grepped before any pruning.
        scan = await asyncio.to_thread(keymod.scan_tree, adir, self.keyring)
        res["key_scan"] = {"files_scanned": scan["files_scanned"], "files_with_key": scan["files_with_key"], "skipped": scan["skipped"]}
        if scan["files_with_key"]:
            res["status"] = KEY_EXPOSURE
            res["objective_pass"] = False
        # Keep per-run disk small (a baseline batch is ~250 runs): the Go cache, the
        # harness's download and state caches, and -- for a craze-repo task -- the
        # workspace itself (the repository with its history); diff.patch, the
        # manifests, the capture, result.json and the logs stay.
        prune = [f"home/{p}" for p in runner.prune_paths] + ["gocache"]
        if task.repo_kind == "craze":
            prune.append("ws")
        res["pruned"] = await asyncio.to_thread(_prune, adir, prune)
        _json_write(adir / "result.json", res)
        return res

    # -- one run (with re-runs) ------------------------------------------------------

    async def run(self, spec: RunSpec) -> dict:
        rdir = run_dir(self.cfg, spec)
        if self.cfg.resume:
            prior = done_result(rdir)
            if prior is not None:
                return {**prior, "resumed": True}
        attempts = []
        infra_left, contam_left = INFRA_RERUNS, CONTAMINATION_RERUNS
        n = _next_attempt(rdir)
        res: dict = {}
        # The provider's slot first: a run waiting on its provider must not hold one
        # of the batch's global slots.
        async with self._psem(spec.em.provider), self._sem:
            while True:
                if self.proxy.budget_stopped:
                    res = {"run_key": spec.key, "status": BUDGET_STOP, "objective_pass": False, "attempts": attempts}
                    break
                n += 1
                adir = rdir / f"attempt-{n}"
                adir.mkdir(parents=True, exist_ok=False)
                t0 = time.time()
                try:
                    res = await self.attempt(spec, adir, n)
                except Exception as e:
                    res = {"run_key": spec.key, "status": "launch-error", "objective_pass": False,
                           "notes": [f"{type(e).__name__}: {e}"], "attempt": n}
                    _json_write(adir / "result.json", res)
                attempts.append({"attempt": n, "status": res.get("status"), "dir": adir.name, "started": t0})
                st = res.get("status")
                if st in ("infra", "launch-error") and infra_left > 0:
                    infra_left -= 1
                    continue
                if st == "contaminated" and contam_left > 0:
                    contam_left -= 1
                    continue
                break
        res = dict(res)
        res.setdefault("run_key", spec.key)
        res.setdefault("split", spec.task.split)
        res["attempts"] = attempts
        _json_write(rdir / "result.json", res)
        if res.get("status") == BUDGET_STOP:
            self.stop_reason = BUDGET_STOP
        return res


def _prune_scoring(d: Path) -> None:
    """Keep the scoring commands' output, drop the workspace copies (rmtree never
    follows a link; the entries are our own directories)."""
    if not d.is_dir():
        return
    for p in d.iterdir():
        if p.is_dir() and not p.is_symlink():
            shutil.rmtree(p, ignore_errors=True)


def _expand(adir: Path, rel: str) -> list[str]:
    """``rel``, or -- when its last component is a glob -- the entries of its parent
    directory matching it (listed without following a link: a planted symlink as the
    parent lists nothing)."""
    parent, _, name = rel.rpartition("/")
    if not any(ch in name for ch in "*?["):
        return [rel]
    base = adir / parent if parent else adir
    try:
        # Top-level entries only: a second-level directory is neither yielded nor entered.
        entries = [e.rel for e in safefs.walk(base, skip_dir=lambda r: "/" in r)
                   if "/" not in e.rel and fnmatch.fnmatch(e.rel, name)]
    except OSError:
        return []
    return [f"{parent}/{e}" if parent else e for e in sorted(entries)]


def _prune(adir: Path, rels: list[str]) -> dict[str, int]:
    """Delete caches under the attempt directory; bytes freed per path. Every
    component is walked without following a link (a planted symlink anywhere on the
    way means there is nothing of ours to delete). Review r1-c1 finding 3. A last
    component may be a glob (``.codex/*.sqlite``)."""
    out = {}
    for pattern in rels:
        for rel in _expand(adir, pattern):
            size = safefs.tree_size(adir, rel)
            if safefs.remove(adir, rel):
                out[rel] = size
    return out


def _redact_cmd(cmd: list[str], prompt: str) -> list[str]:
    return ["<prompt>" if c == prompt else c for c in cmd]


def tool_roundtrips(records: list[dict]) -> int:
    """Tool calls whose result was sent back to the model in a later request."""
    # Each request's conversation, serialised once, kept when it carries tool results.
    returned: list[tuple[int, str]] = []
    for r in records:
        req = r.get("request")
        if not isinstance(req, dict):
            continue
        blob = json.dumps(req.get("messages") or req.get("input") or [])
        if '"tool_call_id"' in blob or "function_call_output" in blob or '"role": "tool"' in blob:
            returned.append((r.get("seq") or 0, blob))
    return sum(
        1
        for c in cap.all_tool_calls(records)
        if c.get("id") and any(seq > c.get("request_seq", 0) and c["id"] in blob for seq, blob in returned)
    )


def gitpost_problem(post: dict) -> str | None:
    """Why the post-run git evidence cannot be accepted, if it cannot (review r2-c1
    item 17, r3-c1 item J): the sandbox must exit 0 in time, staging must have
    succeeded (or failed only for the one accepted embedded-repository case that
    ``--ignore-errors`` exists for), and the diff must have succeeded."""
    if post.get("timed_out"):
        return "post-run git timed out"
    if post.get("exit") != 0:
        return f"post-run git exited {post.get('exit')}"
    add_rc = post.get("add_rc")
    if add_rc not in (None, "", "0") and not add_errors_benign(post.get("add_errors") or ""):
        return f"post-run git add failed ({add_rc}): {(post.get('add_errors') or '').strip()[:300]}"
    if post.get("diff_rc") != "0":
        return f"post-run git diff failed ({post.get('diff_rc') or 'no status'})"
    return None


def gitpost_note(post: dict) -> str | None:
    """A note when the post-run git evidence was accepted despite a nonzero ``git
    add`` -- the one legitimate case, an embedded repository with no commit skipped
    by ``--ignore-errors`` (review r3-c1 item J)."""
    add_rc = post.get("add_rc")
    if add_rc not in (None, "", "0") and add_errors_benign(post.get("add_errors") or ""):
        return f"post-run git: skipped an embedded repository with no commit (add_rc={add_rc})"
    return None


def classify(sres, launch_error, proxy_summary, ext, records, contamination, check_error,
             evidence_error: str | None = None, oversize: list | None = None) -> str:
    """A run is ``ok`` only on the harness's explicit successful completion with exit
    0 (review r1-c1 finding 17) and accepted post-run evidence (r2-c1 item 17); an
    over-limit workspace fails as ``oversize`` (r2-c1 item 14)."""
    if launch_error or sres is None:
        return "launch-error"
    flags = set(proxy_summary.get("flags") or [])
    if proxy_summary.get("key_exposure_refusals") or KEY_EXPOSURE in flags:
        return KEY_EXPOSURE
    # A key's value in a sandboxed process's environment. (Credential-looking names
    # a harness sets for itself are recorded in env_samples, not failed here: the
    # launch environment is checked before bwrap starts.)
    if (sres.env_samples or {}).get("key_hits"):
        return KEY_EXPOSURE
    if BUDGET_STOP in flags:
        return BUDGET_STOP
    if BUDGET_CAPPED in flags:
        return BUDGET_CAPPED
    if not sres.quiescent:
        return "infra"
    if oversize:
        return OVERSIZE
    if contamination:
        return "contaminated"
    if evidence_error:
        return "infra"
    if sres.timed_out:
        return "timeout"
    succeeded = sres.exit_code == 0 and ext.completed
    if not succeeded:
        last = next((r for r in reversed(records) if not r.get("refused")), None)
        # Upstream trouble only: a harness hanging up on a stream ("cancelled") is not.
        if last is not None:
            st = r_status(last) or 0
            if st == 429 or st >= 500 or str(last.get("error") or "").startswith("upstream") or last.get("incomplete"):
                return "infra"
        return "crashed"
    if check_error:
        return "infra"
    return "ok"


def r_status(r: dict) -> int | None:
    try:
        return int(r.get("status"))
    except (TypeError, ValueError):
        return None


async def run_batch(cfg: BatchConfig, providers, keyring, prices, tc: Toolchains, log=print) -> dict:
    executables = await asyncio.to_thread(manifestmod.executables, tc, cfg.craze_bin)
    identity = batch_identity(cfg, executables, prices)
    lock = open_batch_dir(cfg, identity)
    try:
        return await _run_batch(cfg, providers, keyring, prices, tc, log, executables)
    finally:
        lock.close()


async def _run_batch(cfg: BatchConfig, providers, keyring, prices, tc: Toolchains, log, executables: dict) -> dict:
    # Validation: a task failing its controls cannot run.
    valid, invalid = [], []
    for t in cfg.tasks:
        v = await validate_task(t, tc, cache=cfg.cache)
        (valid if v.ok else invalid).append((t, v))
        log(f"validate {t.id}: {'ok' if v.ok else 'FAILED'}")
    _json_write(cfg.out / "validation.json", [v.to_dict() for _, v in valid + invalid])
    cfg.tasks = [t for t, _ in valid]
    runs = plan_runs(cfg)
    summary = {
        "label": cfg.label,
        "out": str(cfg.out),
        "runs_planned": len(runs),
        "invalid_tasks": [t.id for t, _ in invalid],
    }
    if invalid or not runs:
        summary["refused"] = ("tasks failed validation: " + ", ".join(t.id for t, _ in invalid)) if invalid else "nothing to run"
        _json_write(cfg.out / "summary.json", summary)
        return summary
    batch = Batch(cfg, providers, keyring, prices, tc)
    # Refuse a batch whose estimate would pass the cap.
    tot = batch.ledger.totals()
    estimate = sum(batch.ledger.run_estimate(r.em.wire_model, prices[r.em.wire_model].prior_run_cost) for r in runs)
    summary.update({"ledger_before": tot, "estimate": round(estimate, 4)})
    if tot["total"] + estimate > cfg.budget_cap:
        summary["refused"] = f"estimate ${estimate:.2f} + ledger ${tot['total']:.2f} would pass the ${cfg.budget_cap:.2f} cap"
        _json_write(cfg.out / "summary.json", summary)
        batch.ledger.close()
        await batch.stop()
        return summary
    await batch.start()
    try:
        man = await asyncio.to_thread(
            manifestmod.build,
            label=cfg.label, tc=tc, craze_bin=cfg.craze_bin, models=cfg.models, harnesses=cfg.harnesses,
            tasks=cfg.tasks, snap=cfg.snap, route_table=batch.proxy.route_table(), run_order=[r.key for r in runs],
            executables_info=executables,
        )
        man["opencode_seed"] = str(cfg.opencode_seed) if cfg.opencode_seed else None
        man["max_output_reserved"] = cfg.snap.max_output(cfg.models)
        man["identity_fingerprint"] = json.loads((cfg.out / "batch.json").read_text()).get("fingerprint")
        _json_write(cfg.out / ("manifest.resume.json" if cfg.resume else "manifest.json"), man)
        tasks = [asyncio.create_task(batch.run(r)) for r in runs]
        by_key = {r.key: r for r in runs}
        for fut in asyncio.as_completed(tasks):
            res = await fut
            batch.results.append(res)
            spec = by_key.get(res.get("run_key"))
            sealed = spec is not None and spec.task.split == "heldout"
            line = {k: res.get(k) for k in ("run_key", "status", "objective_pass")}
            line["cost"] = (res.get("metrics") or {}).get("cost")
            # Held-out verdicts and failure details stay in the sealed part (§3.1.5).
            _jsonl_append(cfg.out / ("heldout" if sealed else "") / "results.jsonl", line)
            if sealed:
                _jsonl_append(cfg.out / "results.jsonl", {"run_key": line["run_key"], "sealed": True})
                log(f"[sealed] {line['run_key']}: done")
            else:
                log(f"{line['run_key']}: {line['status']} pass={line['objective_pass']} cost=${line['cost'] or 0:.4f}")
    finally:
        await batch.stop()
        summary["ledger_after"] = batch.ledger.totals()
        summary["unrouted_refusals"] = batch.proxy.unrouted_refusals
        summary["stop_reason"] = batch.stop_reason
        open_results = [r for r in batch.results if r.get("split") != "heldout"]
        summary["statuses"] = dict(Counter(r.get("status") for r in open_results))
        summary["sealed_runs"] = len(batch.results) - len(open_results)
        summary["objective_pass"] = sum(1 for r in open_results if r.get("objective_pass"))
        _json_write(cfg.out / ("summary.resume.json" if cfg.resume else "summary.json"), summary)
        batch.ledger.close()
    return summary
