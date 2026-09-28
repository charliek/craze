"""Validators: per-category controls a task must pass before it may run (plan 029 §3.1.5).

- implementation tasks (bugfix, feature, refactor, multi-step): every ``tests`` and
  ``structural_count`` check fails on the untouched workspace and passes with
  ``reference.patch``; a craze task with a ``setup.patch`` and no reference patch
  uses the base commit's content (3eabb31) as its reference instead -- the planted
  workspace with ``setup.patch`` reversed. A refactor preserves
  behaviour, so its ``tests`` checks pass on both (its structural checks carry the
  control). A ``test_discrimination`` check passes with the reference (whose tests
  must fail on the original) and fails untouched (no tests added). A
  ``shared_helper`` check fails untouched and passes with the reference, like a
  structural count. A ``diff_scope`` check passes with the reference and fails when an
  edit to a denied file, or to a file outside the scope, is added to it;
- investigate: the fact checks pass on a reference answer and fail on a decoy;
- verify: the execution check passes on a synthetic capture whose code-running call
  exercised the case and had its result sent back, and fails with no such call, with an
  irrelevant call (``python --version``) that completed, and with the right call
  attempted but never answered;
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
    check_diff_scope,
    check_no_writes,
    check_shared_helper,
    check_structural_count,
    check_test_discrimination,
    check_tests,
)
from crazeeval.sandbox import Toolchains
from crazeeval.tasks import ANSWER_LIKE, IMPLEMENTATION, Task
from crazeeval.workspace import git, ignores_for, materialise


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


def _synthetic_capture(command: str | None, result: str | None = None) -> list[dict]:
    """A capture whose first response calls a code-running tool (or none); with
    ``result``, a second request sends that call's result back to the model, as a
    harness does once the call completed."""
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
    recs = [{"seq": 1, "path": "/chat/completions", "request": {"model": "m", "tools": []}, "response": {"events": events}}]
    if command is not None and result is not None:
        msgs = [{"role": "user", "content": "q"},
                {"role": "assistant", "content": None, "tool_calls": [
                    {"id": "call_1", "type": "function", "function": {"name": "bash", "arguments": json.dumps({"command": command})}}]},
                {"role": "tool", "tool_call_id": "call_1", "content": result}]
        recs.append({"seq": 2, "path": "/chat/completions", "request": {"model": "m", "tools": [], "messages": msgs},
                     "response": {"events": [{"choices": [{"index": 0, "delta": {"content": "done"}}]}]}})
    return recs


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

    # Execution controls (verify): a completed call that exercised the case passes; no
    # call, an irrelevant completed call, and the right call never answered all fail.
    if "executed_code" in by_type:
        cmd = val.get("synthetic_command")
        res = val.get("synthetic_result")
        if not cmd or res is None:
            raise ValueError("executed_code checks need validate.synthetic_command and validate.synthetic_result")
        other = val.get("irrelevant_command", "python --version")
        other_res = val.get("irrelevant_result", "Python 3.12.3")
        for c in by_type["executed_code"]:
            n = c["name"]
            v.controls.append(Control(f"{n}: completed call that exercised the case", True,
                                      check_executed_code(c, _synthetic_capture(cmd, res))["passed"]))
            v.controls.append(Control(f"{n}: no code-running call", False, check_executed_code(c, _synthetic_capture(None))["passed"]))
            v.controls.append(Control(f"{n}: irrelevant completed call ({other})", False,
                                      check_executed_code(c, _synthetic_capture(other, other_res))["passed"]))
            if c.get("require_result", True):
                v.controls.append(Control(f"{n}: the call attempted, no result", False,
                                          check_executed_code(c, _synthetic_capture(cmd))["passed"]))

    # Implementation controls: tests/structure fail untouched, pass with the reference.
    impl_checks = (by_type.get("tests", []) + by_type.get("structural_count", []) + by_type.get("shared_helper", [])
                   + by_type.get("test_discrimination", []) + by_type.get("diff_scope", []))
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
            # The base commit's content, as the planted workspace with setup.patch
            # reversed: the same files, modes and layout as a run's workspace, so a
            # diff against the start shows only the fix (a fresh checkout of the
            # template would differ in every file's mode, by the umask).
            materialise(task, reference, cache=cache)
            git(reference, "apply", "-R", "--whitespace=nowarn", "-", input=task.setup_patch.read_bytes())
            ref_label = "base commit (setup.patch reversed)"
        else:
            raise ValueError("implementation checks need validate.reference_patch")
        # A refactor must not change behaviour: its tests pass before and after.
        untouched_tests_pass = task.category == "refactor"
        for c in impl_checks:
            if c["type"] == "tests":
                ctx_u = _ctx(task, untouched, start, work / "u", tc, cache)
                ctx_r = _ctx(task, reference, start, work / "r", tc, cache)
                ru = await check_tests(c, ctx_u, label=f"validate-untouched-{c['name']}")
                rr = await check_tests(c, ctx_r, label=f"validate-reference-{c['name']}")
                v.controls.append(Control(f"{c['name']}: untouched workspace", untouched_tests_pass, ru["passed"], _tail(ru)))
                v.controls.append(Control(f"{c['name']}: {ref_label}", True, rr["passed"], _tail(rr)))
            elif c["type"] == "test_discrimination":
                ru = await check_test_discrimination(c, _ctx(task, untouched, start, work / "du", tc, cache))
                rr = await check_test_discrimination(c, _ctx(task, reference, start, work / "dr", tc, cache))
                v.controls.append(Control(f"{c['name']}: untouched workspace (no tests added)", False, ru["passed"], _tail(ru)))
                v.controls.append(Control(f"{c['name']}: {ref_label}'s tests", True, rr["passed"], _tail(rr)))
            elif c["type"] == "diff_scope":
                v.controls += _scope_controls(task, c, val, start, untouched, reference, ref_label, work, tc, cache)
            else:
                fn = check_shared_helper if c["type"] == "shared_helper" else check_structural_count
                v.controls.append(Control(f"{c['name']}: untouched workspace", False, fn(c, untouched)["passed"]))
                v.controls.append(Control(f"{c['name']}: {ref_label}", True, fn(c, reference)["passed"]))

    if not v.controls:
        raise ValueError(f"no validator controls for category {task.category!r}")
    if task.category in IMPLEMENTATION and not (by_type.get("tests") or by_type.get("structural_count")
                                                or by_type.get("shared_helper")):
        raise ValueError("an implementation task needs a tests, structural_count or shared_helper check")
    if task.category == "refactor" and not (by_type.get("structural_count") or by_type.get("shared_helper")):
        raise ValueError("a refactor task needs a structural check (its tests pass untouched)")
    if task.category in ANSWER_LIKE | {"investigate"} and "facts" not in by_type:
        raise ValueError(f"a {task.category} task needs a facts check")


def _scope_controls(task: Task, c: dict, val: dict, start, untouched: Path, reference: Path, ref_label: str,
                    work: Path, tc, cache) -> list[Control]:
    """diff_scope controls (review r1-c2 §4): the reference's edit is in scope; the
    reference plus an edit to a denied file (``validate.scope_denied_edit``), or plus an
    edit outside the scope (``validate.scope_outside_edit``), is not."""
    denied, outside = val.get("scope_denied_edit"), val.get("scope_outside_edit")
    if not denied or not outside:
        raise ValueError("diff_scope checks need validate.scope_denied_edit and validate.scope_outside_edit")
    n = c["name"]
    out = [Control(f"{n}: {ref_label}", True, check_diff_scope(c, _ctx(task, reference, start, work / "sr", tc, cache))["passed"])]
    for label, rel in (("a denied edit", denied), ("an edit outside the scope", outside)):
        ws = work / f"scope-{label.split()[1]}" / task.repo_name
        ws.parent.mkdir(parents=True)
        shutil.copytree(reference, ws, symlinks=True)
        target = ws / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        with open(target, "a") as f:
            f.write("\n// scope control edit\n")
        got = check_diff_scope(c, _ctx(task, ws, start, work / f"s-{label.split()[1]}", tc, cache))["passed"]
        out.append(Control(f"{n}: {ref_label} plus {label} ({rel})", False, got))
    return out


def _tail(r: dict) -> str:
    d = r.get("details") or {}
    if "original_exit" in d or "reason" in d:
        text = d.get("reason") or (f"original_exit={d.get('original_exit')} agent_exit={d.get('agent_exit')} "
                                   + (d.get("agent_tail") or "")[-200:])
        return text.replace("\n", " | ")
    return f"exit={d.get('exit')} " + (d.get("tail") or "")[-300:].replace("\n", " | ")
