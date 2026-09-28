"""Validators: per-category controls a task must pass before it may run (plan 029 §3.1.5).

- implementation tasks (bugfix, feature, refactor, multi-step): every ``tests`` and
  ``structural_count`` check fails on the untouched workspace and passes with
  ``reference.patch``; a craze task with a ``setup.patch`` and no reference patch
  uses the base commit (3eabb31) as its reference instead;
- investigate: the fact checks pass on a reference answer and fail on a decoy;
- verify: the execution check passes on a synthetic capture with a code-running
  tool call and fails without one;
- explain / answer / plan: fact checks pass on the reference and fail on the decoy,
  and the no-writes check passes untouched and fails with a planted edit.
"""

from __future__ import annotations

import json
import shutil
import tempfile
from dataclasses import dataclass, field
from pathlib import Path

from crazeeval import paths
from crazeeval.checks import (
    ScoreContext,
    check_executed_code,
    check_facts,
    check_no_writes,
    check_structural_count,
    check_tests,
)
from crazeeval.sandbox import Toolchains
from crazeeval.tasks import ANSWER_LIKE, IMPLEMENTATION, Task
from crazeeval.workspace import craze_template, git, ignores_for, materialise


@dataclass
class Control:
    name: str
    expected: bool
    got: bool | None
    detail: str = ""

    @property
    def ok(self) -> bool:
        return self.got is not None and self.got == self.expected


@dataclass
class Validation:
    task: str
    category: str
    controls: list[Control] = field(default_factory=list)
    error: str | None = None

    @property
    def ok(self) -> bool:
        return self.error is None and bool(self.controls) and all(c.ok for c in self.controls)

    def to_dict(self) -> dict:
        return {
            "task": self.task,
            "category": self.category,
            "ok": self.ok,
            "error": self.error,
            "controls": [
                {"name": c.name, "expected": c.expected, "got": c.got, "ok": c.ok, "detail": c.detail} for c in self.controls
            ],
        }


def _synthetic_capture(command: str | None) -> list[dict]:
    """A one-request capture whose response calls a code-running tool (or none)."""
    events = []
    if command is not None:
        events.append(
            {
                "choices": [
                    {
                        "index": 0,
                        "delta": {
                            "tool_calls": [
                                {"index": 0, "id": "call_1", "function": {"name": "bash", "arguments": json.dumps({"command": command})}}
                            ]
                        },
                    }
                ]
            }
        )
    else:
        events.append({"choices": [{"index": 0, "delta": {"content": "It handles empty input."}}]})
    return [{"seq": 1, "path": "/chat/completions", "request": {"model": "m", "tools": []}, "response": {"events": events}}]


def _ctx(task: Task, ws: Path, start, work: Path, tc, cache) -> ScoreContext:
    """A scoring context for a control (no answer, no capture)."""
    return ScoreContext(
        task=task,
        ws=ws,
        start=start,
        answer="",
        records=[],
        scoring_dir=work / "scoring",
        tc=tc,
        ignores=ignores_for(task),
        ws_inside=f"{paths.SANDBOX_WORK}/{task.repo_name}",
        cache=cache,
    )


async def validate_task(task: Task, tc: Toolchains | None, cache: Path | None = None, keep: Path | None = None) -> Validation:
    v = Validation(task=task.id, category=task.category)
    work = Path(tempfile.mkdtemp(prefix=f"crazeeval-validate-{task.id}-", dir=keep))
    try:
        await _validate(task, v, work, tc, cache)
    except Exception as e:  # a validator that cannot run is a failed validation
        v.error = f"{type(e).__name__}: {e}"
    finally:
        if keep is None:
            shutil.rmtree(work, ignore_errors=True)
    return v


async def _validate(task: Task, v: Validation, work: Path, tc, cache) -> None:
    val = task.validate
    by_type = {}
    for c in task.checks:
        by_type.setdefault(c["type"], []).append(c)

    # Answer controls (facts): reference passes, decoy fails.
    if "facts" in by_type:
        ref = val.get("reference_answer")
        decoy = val.get("decoy_answer")
        if not ref or not decoy:
            raise ValueError("facts checks need validate.reference_answer and validate.decoy_answer")
        ref_text = task.testdata(ref).read_text()
        decoy_text = task.testdata(decoy).read_text()
        for c in by_type["facts"]:
            v.controls.append(Control(f"{c['name']}: reference answer", True, check_facts(c, ref_text)["passed"]))
            v.controls.append(Control(f"{c['name']}: decoy answer", False, check_facts(c, decoy_text)["passed"]))

    # No-writes controls: untouched passes, a planted edit fails.
    if "no_writes" in by_type:
        ws = work / "nowrites" / task.repo_name
        ws.parent.mkdir(parents=True)
        start = materialise(task, ws, cache=cache)
        ctx = _ctx(task, ws, start, work, tc, cache)
        for c in by_type["no_writes"]:
            v.controls.append(Control(f"{c['name']}: untouched workspace", True, check_no_writes(c, ctx)["passed"]))
        plant = val.get("planted_edit") or {"path": "PLANTED.txt", "append": "planted\n"}
        target = ws / plant["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        with open(target, "a") as f:
            f.write(plant.get("append", "planted\n"))
        ctx = _ctx(task, ws, start, work, tc, cache)
        for c in by_type["no_writes"]:
            v.controls.append(Control(f"{c['name']}: planted edit", False, check_no_writes(c, ctx)["passed"]))

    # Execution controls (verify): a synthetic code-running capture passes, none fails.
    if "executed_code" in by_type:
        cmd = val.get("synthetic_command", "python -c 'print(1)'")
        for c in by_type["executed_code"]:
            v.controls.append(Control(f"{c['name']}: synthetic code-running call", True, check_executed_code(c, _synthetic_capture(cmd))["passed"]))
            v.controls.append(Control(f"{c['name']}: no code-running call", False, check_executed_code(c, _synthetic_capture(None))["passed"]))

    # Implementation controls: tests/structure fail untouched, pass with the reference.
    impl_checks = by_type.get("tests", []) + by_type.get("structural_count", [])
    if impl_checks:
        untouched = work / "untouched" / task.repo_name
        untouched.parent.mkdir(parents=True)
        start = materialise(task, untouched, cache=cache)
        reference = work / "reference" / task.repo_name
        reference.parent.mkdir(parents=True)
        if val.get("reference_patch"):
            materialise(task, reference, cache=cache)
            git(reference, "apply", "--whitespace=nowarn", "-", input=task.testdata(val["reference_patch"]).read_bytes())
            ref_label = "reference.patch"
        elif task.repo_kind == "craze" and task.setup_patch is not None:
            shutil.copytree(craze_template(cache=cache), reference, symlinks=True)
            ref_label = "base commit (no setup.patch)"
        else:
            raise ValueError("implementation checks need validate.reference_patch")
        for c in impl_checks:
            if c["type"] == "tests":
                ctx_u = _ctx(task, untouched, start, work / "u", tc, cache)
                ctx_r = _ctx(task, reference, start, work / "r", tc, cache)
                ru = await check_tests(c, ctx_u, label=f"validate-untouched-{c['name']}")
                rr = await check_tests(c, ctx_r, label=f"validate-reference-{c['name']}")
                v.controls.append(Control(f"{c['name']}: untouched workspace", False, ru["passed"], _tail(ru)))
                v.controls.append(Control(f"{c['name']}: {ref_label}", True, rr["passed"], _tail(rr)))
            else:
                v.controls.append(Control(f"{c['name']}: untouched workspace", False, check_structural_count(c, untouched)["passed"]))
                v.controls.append(Control(f"{c['name']}: {ref_label}", True, check_structural_count(c, reference)["passed"]))

    if not v.controls:
        raise ValueError(f"no validator controls for category {task.category!r}")
    if task.category in IMPLEMENTATION and not impl_checks:
        raise ValueError("an implementation task needs a tests or structural_count check")
    if task.category in ANSWER_LIKE | {"investigate"} and "facts" not in by_type:
        raise ValueError(f"a {task.category} task needs a facts check")


def _tail(r: dict) -> str:
    d = r.get("details") or {}
    return f"exit={d.get('exit')} " + (d.get("tail") or "")[-300:].replace("\n", " | ")
