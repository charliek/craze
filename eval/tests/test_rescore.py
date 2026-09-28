"""``crazeeval rescore`` (plan 029 X15): a finished batch's diff and diff-derived checks
recomputed from its manifests, on a synthetic batch written by hand."""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from crazeeval import rescore as rescoremod
from crazeeval.batch import run_objective_pass, tree_hash
from crazeeval.checks import check_diff_scope, check_no_writes
from crazeeval.cli import main
from crazeeval.rescore import IGNORES, LOG, PRE_RESCORE, Skip, final_attempt_dir, rescore
from crazeeval.tasks import load_tasks
from crazeeval.workspace import GENERATED_IGNORES, compare

ANSWER = "ANSWER-TEXT-THAT-MUST-NEVER-BE-PRINTED"
PKG = "internal/harness/redact"

BUILD_TASK = """
id = "{id}"
category = "bugfix"
split = "{split}"
mode = "build"
prompt = "Fix the redact package."

[repo]
kind = "craze"

[[checks]]
type = "tests"
name = "package-tests"
runner = "go"
args = ["./internal/harness/redact/"]
expect = ["::TestString"]

[[checks]]
type = "diff_scope"
name = "scope"
allow = ["internal/harness/redact/*.go"]
deny = ["internal/harness/redact/*_test.go"]
"""

EXPLAIN_TASK = """
id = "{id}"
category = "explain"
split = "dev"
mode = "build"
prompt = "Explain the redact package."

[repo]
{repo}

[[checks]]
type = "facts"
name = "facts"
items = [{{ regex = "longest" }}]

[[checks]]
type = "no_writes"
name = "no-writes"
"""

START = {
    f"{PKG}/redact.go": "f:0644:a",
    f"{PKG}/redact_test.go": "f:0644:a",
    "internal/harness/system.go": "f:0644:a",
    "go.mod": "f:0644:a",
}
# What `make build` and `make test-cli` leave in a craze workspace (the repo's .gitignore).
BUILD_OUTPUTS = {"bin/craze": "f:0755:b", "bin/craze-fake-agent": "f:0755:b", "tests/cli/.venv/x": "f:0644:b"}


def _tasks(root: Path) -> dict:
    specs = {
        "T-XB": BUILD_TASK.format(id="T-XB", split="dev"),
        "T-XH": BUILD_TASK.format(id="T-XH", split="heldout"),
        "T-XE": EXPLAIN_TASK.format(id="T-XE", repo='kind = "craze"'),
        "T-XN": EXPLAIN_TASK.format(id="T-XN", repo='kind = "fixture"\nname = "smoke-py"'),
    }
    for tid, text in specs.items():
        (root / tid).mkdir(parents=True)
        (root / tid / "task.toml").write_text(text)
    return load_tasks(root)


def _old_scoring(task, start: dict, final: dict) -> tuple[list[dict], dict]:
    """The checks and diff as a batch scored them before X15 (no repository ignores)."""
    diff = compare(start, final, list(GENERATED_IGNORES) + task.ignore)
    ctx = SimpleNamespace(diff=diff)
    checks = []
    for c in task.checks:
        if c["type"] == "diff_scope":
            checks.append(check_diff_scope(c, ctx))
        elif c["type"] == "no_writes":
            checks.append(check_no_writes(c, ctx))
        else:  # the tests and facts checks passed
            checks.append({"name": c["name"], "type": c["type"], "passed": True, "details": {"exit": 0}})
    return checks, diff


def _write_run(batch: Path, task, harness: str, model: str, final: dict | None, status: str = "ok",
               attempts: int = 1) -> Path:
    part = "heldout" if task.split == "heldout" else "runs"
    rep = batch / part / harness / model / task.id / "rep1"
    key = f"{harness}/{model}/{task.id}/rep1"
    for n in range(1, attempts):  # earlier attempts: an infra failure whose tree differs
        early = rep / f"attempt-{n}"
        early.mkdir(parents=True)
        other = {**START, "internal/harness/system.go": "f:0644:z", "bin/craze": "f:0755:z"}
        (early / "start-manifest.json").write_text(json.dumps({"commit": "c0", "files": START}))
        (early / "final-manifest.json").write_text(json.dumps({"files": other}))
        (early / "result.json").write_text(json.dumps({"run_key": key, "attempt": n, "status": "infra"}))
    n = attempts
    adir = rep / f"attempt-{n}"
    adir.mkdir(parents=True)
    checks, diff = _old_scoring(task, START, final or START)
    res = {
        "run_key": key, "run_id": f"{batch.name}:{key}:a{n}", "attempt": n, "harness": harness, "model": model,
        "task": task.id, "split": task.split, "status": status, "timed_out": False, "answer": ANSWER,
        "checks": checks, "objective_pass": run_objective_pass(checks, False, status),
        "diff": {k: len(v) for k, v in diff.items()}, "changed": diff,
    }
    if final is not None:
        (adir / "start-manifest.json").write_text(json.dumps({"commit": "c0", "files": START, "isolation": None}))
        (adir / "final-manifest.json").write_text(json.dumps({"files": final, "diff": diff}))
    (adir / "result.json").write_text(json.dumps(res, indent=2, sort_keys=True) + "\n")
    rep_res = {**res, "attempts": [{"attempt": i, "status": "infra" if i < n else status, "dir": f"attempt-{i}"}
                                   for i in range(1, n + 1)]}
    (rep / "result.json").write_text(json.dumps(rep_res, indent=2, sort_keys=True) + "\n")
    return rep


def _snapshot(root: Path) -> dict[str, bytes]:
    return {p.relative_to(root).as_posix(): p.read_bytes() for p in sorted(root.rglob("*")) if p.is_file()}


def _load(p: Path) -> dict:
    return json.loads(p.read_text())


def _check(res: dict, name: str) -> dict:
    return next(c for c in res["checks"] if c["name"] == name)


@pytest.fixture
def batch(tmp_path):
    tasks = _tasks(tmp_path / "tasks")
    b = tmp_path / "final-20260928-000000"
    b.mkdir()
    (b / "batch.json").write_text(json.dumps({"tasks": {t: tree_hash(tasks[t].dir) for t in tasks}}))
    xb, xh, xe, xn = tasks["T-XB"], tasks["T-XH"], tasks["T-XE"], tasks["T-XN"]
    edit = {f"{PKG}/redact.go": "f:0644:fixed"}
    reps = {
        # Only the build outputs are out of scope (the final of two attempts): flips to a pass.
        "only-build": _write_run(b, xb, "craze", "glm", {**START, **edit, **BUILD_OUTPUTS}, attempts=2),
        # A genuine out-of-scope edit besides them stays failed.
        "outside": _write_run(b, xb, "gx", "glm", {**START, **edit, "internal/harness/system.go": "f:0644:x",
                                                   **BUILD_OUTPUTS}),
        # A denied edit (the package's test file) stays failed.
        "denied": _write_run(b, xb, "opencode", "glm", {**START, **edit, f"{PKG}/redact_test.go": "f:0644:x",
                                                        **BUILD_OUTPUTS}),
        # The scope check flips, but a crashed or key-exposure run never passes.
        "crashed": _write_run(b, xb, "codex", "glm", {**START, **edit, **BUILD_OUTPUTS}, status="crashed"),
        "key": _write_run(b, xb, "craze", "glm-flash", {**START, **edit, **BUILD_OUTPUTS}, status="key-exposure"),
        # no_writes on a craze explain task: only the build outputs were written.
        "no-writes": _write_run(b, xe, "craze", "glm", {**START, **BUILD_OUTPUTS}),
        # A fixture task has no repository ignores: nothing changes.
        "fixture": _write_run(b, xn, "craze", "glm", {**START, **BUILD_OUTPUTS}),
        # No manifests: skipped and counted.
        "no-manifests": _write_run(b, xe, "gx", "glm", None),
        # Held-out: sealed unless unsealed.
        "heldout": _write_run(b, xh, "craze", "glm", {**START, **edit, **BUILD_OUTPUTS}),
    }
    return SimpleNamespace(dir=b, tasks=tasks, reps=reps)


def test_dry_run_computes_and_writes_nothing(batch):
    before = _snapshot(batch.dir)
    s = rescore(batch.dir, dry_run=True, tasks=batch.tasks)
    assert _snapshot(batch.dir) == before
    assert s["dry_run"] and s["examined"] == 8 and s["skipped"] == 1 and s["changed"] == 6
    assert s["skipped_reasons"] == {"no start and final manifests": 1}
    assert not (batch.dir / LOG).exists() and not (batch.dir / ".lock").exists()


def test_rescore_flips_only_what_the_build_outputs_failed(batch):
    reps = batch.reps
    originals = {k: (r / "result.json").read_bytes() for k, r in reps.items()}
    heldout_before = _snapshot(batch.dir / "heldout")
    s = rescore(batch.dir, tasks=batch.tasks)
    assert (s["examined"], s["changed"], s["skipped"]) == (8, 6, 1)
    changed = {c["run_key"]: c for c in s["changed_runs"]}
    assert set(changed) == {"craze/glm/T-XB/rep1", "gx/glm/T-XB/rep1", "opencode/glm/T-XB/rep1",
                            "codex/glm/T-XB/rep1", "craze/glm-flash/T-XB/rep1", "craze/glm/T-XE/rep1"}

    # Only the build outputs: diff_scope and objective_pass flip, in both result files.
    rep = reps["only-build"]
    adir = rep / "attempt-2"  # the final attempt, not attempt-1
    for p in (rep / "result.json", adir / "result.json"):
        r = _load(p)
        assert _check(r, "scope")["passed"] and r["objective_pass"]
        assert _check(r, "package-tests") == {"name": "package-tests", "type": "tests", "passed": True,
                                              "details": {"exit": 0}}  # kept as recorded
        assert r["changed"] == {"added": [], "modified": [f"{PKG}/redact.go"], "deleted": []}
        assert r["diff"] == {"added": 0, "modified": 1, "deleted": 0}
        assert r["rescored"] == {"reason": rescoremod.REASON, "ignores": IGNORES, "checks": ["scope"],
                                 "objective_pass_was": False}
        assert r["answer"] == ANSWER  # every other field kept
    assert "attempts" in _load(rep / "result.json") and "attempts" not in _load(adir / "result.json")
    assert (rep / PRE_RESCORE).read_bytes() == originals["only-build"]
    assert not _check(json.loads(originals["only-build"]), "scope")["passed"]
    assert (adir / PRE_RESCORE).exists() and not (rep / "attempt-1" / PRE_RESCORE).exists()
    assert changed["craze/glm/T-XB/rep1"]["checks"] == ["scope"]
    assert changed["craze/glm/T-XB/rep1"]["objective_pass_after"] is True

    # A genuine out-of-scope edit and a denied edit stay failed (their details lose the build outputs).
    out = _load(reps["outside"] / "result.json")
    assert not _check(out, "scope")["passed"] and not out["objective_pass"]
    assert _check(out, "scope")["details"]["outside"] == ["internal/harness/system.go"]
    den = _load(reps["denied"] / "result.json")
    assert not _check(den, "scope")["passed"] and not den["objective_pass"]
    assert _check(den, "scope")["details"]["denied"] == [f"{PKG}/redact_test.go"]
    assert changed["gx/glm/T-XB/rep1"]["checks"] == [] and changed["opencode/glm/T-XB/rep1"]["checks"] == []

    # A non-ok status stays objective-fail although its scope check now passes.
    for k in ("crashed", "key"):
        r = _load(reps[k] / "result.json")
        assert _check(r, "scope")["passed"] and r["objective_pass"] is False
        assert r["rescored"]["checks"] == ["scope"] and r["status"] in ("crashed", "key-exposure")

    nw = _load(reps["no-writes"] / "result.json")
    assert _check(nw, "no-writes")["passed"] and nw["objective_pass"]

    # Untouched: the fixture task's run, the run without manifests, and all of held-out.
    for k in ("fixture", "no-manifests"):
        assert (reps[k] / "result.json").read_bytes() == originals[k] and not (reps[k] / PRE_RESCORE).exists()
    assert not _check(_load(reps["fixture"] / "result.json"), "no-writes")["passed"]
    assert _snapshot(batch.dir / "heldout") == heldout_before

    lines = [json.loads(ln) for ln in (batch.dir / LOG).read_text().splitlines()]
    assert {ln["run_key"] for ln in lines} == set(changed) and len(lines) == 6
    first = next(ln for ln in lines if ln["run_key"] == "craze/glm/T-XB/rep1")
    assert first["run_id"] == _load(rep / "result.json")["run_id"]
    assert (first["task"], first["harness"], first["model"]) == ("T-XB", "craze", "glm")
    assert (first["objective_pass_before"], first["objective_pass_after"]) == (False, True)
    assert first["additions_ignored"] == 3
    assert ANSWER not in (batch.dir / LOG).read_text()
    assert not (batch.dir / "heldout" / LOG).exists()


def test_a_second_rescore_changes_nothing_and_keeps_the_original(batch):
    rep = batch.reps["only-build"]
    original = (rep / "result.json").read_bytes()
    rescore(batch.dir, tasks=batch.tasks)
    after_first = _snapshot(batch.dir)
    s = rescore(batch.dir, tasks=batch.tasks)
    assert (s["examined"], s["changed"], s["skipped"]) == (8, 0, 1)
    assert _snapshot(batch.dir) == after_first  # nothing written, rescore.jsonl included

    # A rescore that does rewrite a result again never overwrites the preserved original.
    r = _load(rep / "result.json")
    _check(r, "scope")["passed"] = False
    (rep / "result.json").write_text(json.dumps(r))
    s = rescore(batch.dir, tasks=batch.tasks)
    assert s["changed"] == 1 and _check(_load(rep / "result.json"), "scope")["passed"]
    assert (rep / PRE_RESCORE).read_bytes() == original


def test_heldout_is_rescored_only_unsealed(batch):
    rep = batch.reps["heldout"]
    rescore(batch.dir, tasks=batch.tasks)
    assert not _check(_load(rep / "result.json"), "scope")["passed"]
    s = rescore(batch.dir, unseal=True, tasks=batch.tasks)
    assert (s["examined"], s["changed"]) == (9, 1)
    assert s["changed_runs"][0]["run_key"] == "craze/glm/T-XH/rep1"
    r = _load(rep / "result.json")
    assert _check(r, "scope")["passed"] and r["objective_pass"] and (rep / PRE_RESCORE).exists()
    # Its line stays in the sealed part.
    held = [json.loads(ln) for ln in (batch.dir / "heldout" / LOG).read_text().splitlines()]
    assert [ln["run_key"] for ln in held] == ["craze/glm/T-XH/rep1"]
    assert all(json.loads(ln)["task"] != "T-XH" for ln in (batch.dir / LOG).read_text().splitlines())


def test_a_changed_task_or_a_result_without_an_attempt_is_skipped(batch):
    ident = _load(batch.dir / "batch.json")
    ident["tasks"]["T-XE"] = "0" * 64
    (batch.dir / "batch.json").write_text(json.dumps(ident))
    s = rescore(batch.dir, dry_run=True, tasks=batch.tasks)
    assert s["skipped_reasons"] == {"no start and final manifests": 1, "the task changed since the batch": 1}
    assert "craze/glm/T-XE/rep1" not in {c["run_key"] for c in s["changed_runs"]}
    # A budget stop before any attempt ran has no attempt behind it.
    with pytest.raises(Skip):
        final_attempt_dir(batch.dir, {"status": "budget-stop", "attempts": [{"attempt": 1, "dir": "attempt-1"}]})
    with pytest.raises(Skip):
        final_attempt_dir(batch.dir, {"attempt": 2, "attempts": [{"attempt": 1, "dir": "attempt-1"}]})


def test_cli_rescore_prints_the_summary_and_no_answer(batch, monkeypatch, capsys):
    monkeypatch.setattr(rescoremod, "load_tasks", lambda: batch.tasks)
    before = _snapshot(batch.dir)
    assert main(["rescore", "--batch", str(batch.dir), "--dry-run"]) == 0
    out = capsys.readouterr().out
    s = json.loads(out)
    assert (s["examined"], s["changed"], s["skipped"]) == (8, 6, 1) and ANSWER not in out
    assert _snapshot(batch.dir) == before
    assert main(["rescore", "--batch", str(batch.dir.parent / "no-such-batch")]) == 2
