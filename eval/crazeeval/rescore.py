"""``crazeeval rescore``: re-derive a finished batch's diff-derived checks (plan 029 X15).

A run's diff is its start manifest against its final one, minus the additions its
ignore patterns cover (``workspace.compare``). X15 added a craze-repo task's own
``.gitignore`` to those patterns, so a batch scored before it can be rescored from what
it kept: each rep's final attempt's ``start-manifest.json`` and ``final-manifest.json``.
Only the checks that read the diff (``no_writes``, ``diff_scope``) are re-run, through
the same check functions; every other recorded check stands, and ``objective_pass`` is
recomputed as batch.py computes it. Only manifests, ``batch.json`` and ``result.json``
are read -- no capture, workspace or log -- and nothing of a result but its checks,
status and identifying fields is printed or logged. Held-out runs are read only when
unsealed.
"""

from __future__ import annotations

import fcntl
import json
import os
from pathlib import Path
from types import SimpleNamespace

from crazeeval.batch import OVERSIZE, _json_write, _jsonl_append, run_objective_pass, tree_hash
from crazeeval.checks import check_diff_scope, check_no_writes
from crazeeval.report import iter_results
from crazeeval.runners import RUNNERS
from crazeeval.tasks import Task, load_tasks
from crazeeval.workspace import compare, ignores_for

# The checks whose verdict is a function of the diff alone.
DIFF_CHECKS = {"no_writes": check_no_writes, "diff_scope": check_diff_scope}
PRE_RESCORE = "result.pre-rescore.json"
LOG = "rescore.jsonl"
IGNORES = "craze-repo .gitignore (X15)"
REASON = "diff recomputed from the run's manifests with the task repository's own .gitignore as addition ignores"


class RescoreError(RuntimeError):
    """The batch cannot be rescored at all (not a batch directory, or in use)."""


class Skip(Exception):
    """One run that cannot be rescored; the message is the counted reason."""


def final_attempt_dir(rep_dir: Path, res: dict) -> Path:
    """The attempt directory a rep-level result came from. ``Batch.run`` writes the
    final attempt's result at the rep level with the ``attempts`` list added, so the
    result's own ``attempt`` is the last entry of ``attempts``, whose ``dir`` is that
    attempt's directory. A result with no attempt behind it (a budget stop before one
    started) is skipped."""
    n = res.get("attempt")
    attempts = res.get("attempts") or []
    last = attempts[-1] if attempts and isinstance(attempts[-1], dict) else {}
    if not isinstance(n, int) or last.get("attempt") != n or last.get("dir") != f"attempt-{n}":
        raise Skip("no attempt behind the result")
    return rep_dir / last["dir"]


def rescore_result(res: dict, task: Task, start: dict[str, str], final: dict[str, str],
                   workspace_state: list[str]) -> tuple[dict, list[str]]:
    """``res`` with its diff recomputed (the ignores a run gets today) and its
    diff-derived checks re-run from the task's definitions; the other checks are kept
    as recorded. Returns the new result and the names of the checks whose pass changed."""
    diff = compare(start, final, ignores_for(task, workspace_state))
    ctx = SimpleNamespace(diff=diff)  # the diff checks read ctx.diff and nothing else
    # load_task rejects a task whose checks repeat a name (review r1-c11/c12 P2), so this
    # is a real 1:1 index, never a last-one-wins collision.
    defs = {c.get("name", c["type"]): c for c in task.checks}
    checks, flipped = [], []
    for old in res.get("checks") or []:
        fn = DIFF_CHECKS.get(old.get("type"))
        if fn is None:
            checks.append(old)
            continue
        spec = defs.get(old.get("name"))
        if spec is None or spec["type"] != old["type"]:
            raise Skip("a recorded check has no definition in the task")
        new = json.loads(json.dumps(fn(spec, ctx), default=str))  # as result.json holds it
        if new["passed"] != old.get("passed"):
            flipped.append(new["name"])
        checks.append(new)
    out = {
        **res,
        "checks": checks,
        "objective_pass": run_objective_pass(checks, bool(res.get("timed_out")), res.get("status")),
        "diff": {k: len(v) for k, v in diff.items()},
        "changed": diff,
    }
    return out, flipped


def _read_json(p: Path) -> dict:
    v = json.loads(p.read_text())
    if not isinstance(v, dict):
        raise ValueError(f"{p.name}: not an object")
    return v


def _manifest_files(p: Path) -> dict[str, str]:
    files = _read_json(p)["files"]
    if not isinstance(files, dict):
        raise ValueError(f"{p.name}: no files")
    return files


def _preserve_and_write(d: Path, new: dict) -> None:
    """Keep ``d/result.json`` as it was the first time it is rescored (never
    overwritten: a second rescore keeps the true original), then write ``new``."""
    pre = d / PRE_RESCORE
    if not pre.exists():
        tmp = pre.with_name(pre.name + ".tmp")
        tmp.write_bytes((d / "result.json").read_bytes())
        os.replace(tmp, pre)
    _json_write(d / "result.json", new)


def _lock(batch: Path):
    """The batch directory's lock (as ``crazeeval run`` holds it): no rescore of a
    batch that is running."""
    fh = open(batch / ".lock", "a")
    try:
        fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as e:
        fh.close()
        raise RescoreError(f"{batch} is in use by another crazeeval process") from e
    return fh


def _rescore_rep(rep_dir: Path, res: dict, tasks: dict[str, Task], task_hashes: dict, dry_run: bool) -> dict | None:
    """Rescore one rep; the rescore.jsonl line when it changed, ``None`` when not."""
    adir = final_attempt_dir(rep_dir, res)
    try:
        start = _manifest_files(adir / "start-manifest.json")
        final = _manifest_files(adir / "final-manifest.json")
    except (OSError, ValueError, KeyError):
        raise Skip("no start and final manifests") from None
    try:
        att = _read_json(adir / "result.json")
    except (OSError, ValueError):
        raise Skip("no attempt result") from None
    if att.get("run_id") != res.get("run_id") or att.get("attempt") != res.get("attempt"):
        raise Skip("the attempt's result is not the rep's")
    task = tasks.get(res.get("task"))
    if task is None:
        raise Skip("unknown task")
    # The check definitions and ignores come from the task as it is now: it must be the
    # task the batch ran (its fingerprint in batch.json).
    if task.id not in task_hashes["now"]:
        task_hashes["now"][task.id] = tree_hash(task.dir)
    if task_hashes["batch"].get(task.id) != task_hashes["now"][task.id]:
        raise Skip("the task changed since the batch")
    runner = RUNNERS.get(res.get("harness"))
    if runner is None:
        raise Skip("unknown harness")
    if "changed" not in res:
        raise Skip("no recorded diff")
    if res.get("status") == OVERSIZE and not final:
        raise Skip("oversize: no final manifest was built")
    new_rep, flipped = rescore_result(res, task, start, final, runner.workspace_state)
    new_att, att_flipped = rescore_result(att, task, start, final, runner.workspace_state)
    if new_rep == res and new_att == att:
        return None
    if not dry_run:
        for d, old, new, fl in ((adir, att, new_att, att_flipped), (rep_dir, res, new_rep, flipped)):
            if new != old:
                _preserve_and_write(d, {**new, "rescored": {
                    "reason": REASON,
                    "ignores": IGNORES,
                    "checks": fl,
                    "objective_pass_was": bool(old.get("objective_pass")),
                }})
    return {
        "run_id": res.get("run_id"),
        "run_key": res.get("run_key"),
        "task": res.get("task"),
        "harness": res.get("harness"),
        "model": res.get("model"),
        "checks": flipped,
        "objective_pass_before": bool(res.get("objective_pass")),
        "objective_pass_after": new_rep["objective_pass"],
        "additions_ignored": len(set((res.get("changed") or {}).get("added") or []) - set(new_rep["changed"]["added"])),
        "ignores": IGNORES,
    }


def rescore(batch: Path, unseal: bool = False, dry_run: bool = False, tasks: dict[str, Task] | None = None) -> dict:
    """Rescore every rep under ``batch``'s ``runs/`` -- and ``heldout/`` only with
    ``unseal``. A changed run's rep-level and attempt-level ``result.json`` are rewritten
    (each original kept once as ``result.pre-rescore.json``) with a ``rescored`` record,
    and one line per changed run is appended to ``rescore.jsonl`` (a held-out run's to
    ``heldout/rescore.jsonl``: it stays in the sealed part). ``dry_run`` computes and
    writes nothing. A second rescore of a rescored batch changes nothing."""
    batch = Path(batch)
    try:
        ident = _read_json(batch / "batch.json")
    except (OSError, ValueError) as e:
        raise RescoreError(f"{batch}: no readable batch.json (not a batch directory)") from e
    tasks = load_tasks() if tasks is None else tasks
    task_hashes = {"batch": ident.get("tasks") or {}, "now": {}}
    summary: dict = {"examined": 0, "changed": 0, "skipped": 0, "dry_run": dry_run, "changed_runs": [],
                     "skipped_reasons": {}}
    lock = None if dry_run else _lock(batch)
    try:
        for root, rep_dir, res in iter_results(batch, unseal):
            summary["examined"] += 1
            try:
                line = _rescore_rep(rep_dir, res, tasks, task_hashes, dry_run)
            except Skip as e:
                summary["skipped"] += 1
                reasons = summary["skipped_reasons"]
                reasons[str(e)] = reasons.get(str(e), 0) + 1
                continue
            if line is None:
                continue
            summary["changed"] += 1
            summary["changed_runs"].append({k: line[k] for k in (
                "run_key", "checks", "objective_pass_before", "objective_pass_after")})
            if not dry_run:
                _jsonl_append((root if root.name == "heldout" else batch) / LOG, line)
    finally:
        if lock is not None:
            lock.close()
    return summary
