"""Plan 029 W2: the per-task judge hash. Adding a task, or changing another task's
rubric, leaves a verdict standing -- judge-batch still skips its pair and the report still
counts it; changing its own task's rubric does not; a record from before per-task hashes
stands on the global hash."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path

from crazeeval import report as rp
from crazeeval.judge import Judge, judge_hash, task_judge_hash, task_judge_hashes, verdict_current
from crazeeval.judging import done_pairs, find_runs, judge_batch, make_pairs
from crazeeval.tasks import Task

M = "m1"


def _task(tid: str, rubric: list[str], split: str = "dev", prompt: str = "Explain it.") -> Task:
    return Task(id=tid, dir=Path("."), category="explain", split=split, mode="build", prompt=prompt,
                repo_kind="fixture", fixture="x", setup_patch=None, timeout_s=1, rubric=list(rubric), checks=[],
                validate={}, false_claims=["a false claim"])


def _tasks(**extra) -> dict[str, Task]:
    t = {"A": _task("A", ["a1", "a2"]), "B": _task("B", ["b1"])}
    t.update(extra)
    return t


def _write_run(batch: Path, harness: str, task: str) -> None:
    rep = batch / "runs" / harness / M / task / "rep1"
    (rep / "attempt-1").mkdir(parents=True)
    res = {"run_key": f"{harness}/{M}/{task}/rep1", "run_id": f"{batch.name}:{harness}/{M}/{task}/rep1:a1",
           "harness": harness, "model": M, "task": task, "rep": 1, "status": "ok", "objective_pass": True,
           "split": "dev", "answer": f"{harness} answer", "answer_words": 2, "checks": [],
           "attempts": [{"attempt": 1, "dir": "attempt-1", "status": "ok"}]}
    (rep / "result.json").write_text(json.dumps(res))
    (rep / "attempt-1" / "result.json").write_text(json.dumps(res))
    (rep / "attempt-1" / "capture.jsonl").write_text("\n")
    (rep / "attempt-1" / "diff.patch").write_text("")


async def _fake_codex(model, prompt, n_items):
    v = {"winner": "A", "confidence": "high", "score_a": 8, "score_b": 5,
         "rubric_a": [{"item": i, "grade": "met"} for i in range(1, n_items + 1)],
         "rubric_b": [{"item": i, "grade": "missed"} for i in range(1, n_items + 1)], "reasons": "item 1"}
    return 0, "", json.dumps(v)


def _judge(batch: Path, out: Path, tasks: dict[str, Task]) -> dict:
    runs = find_runs(batch)
    pairs = make_pairs(runs, runs, "craze", "gx", tasks)
    j = Judge(home=out.parent / "jh", runner=_fake_codex, log=lambda *_: None)
    return asyncio.run(judge_batch(pairs, tasks, out, j, "sol", "single", 7, log=lambda *_: None))


def _report(batch: Path, out: Path, tasks: dict[str, Task]) -> dict:
    verdicts, stale = rp.load_current_verdicts([out], tasks)
    return rp.build(rp.ReportInput(runs=rp.load_runs([batch]), verdicts=verdicts, tasks=tasks, unseal=False,
                                   stale_verdicts=len(stale), judge_hash_now=judge_hash(tasks)))


def test_task_judge_hash_covers_only_its_own_task():
    tasks = _tasks()
    h = task_judge_hashes(tasks)
    more = _tasks(C=_task("C", ["c1"]))
    assert judge_hash(more) != judge_hash(tasks)  # the global hash moves with any task
    assert task_judge_hashes(more)["A"] == h["A"] and task_judge_hashes(more)["B"] == h["B"]
    for change in ({"rubric": ["a1", "a2 changed"]}, {"prompt": "Explain it again."},
                   {"false_claims": ["another false claim"]}, {"mode": "plan"}):
        t = _task("A", ["a1", "a2"])
        for k, v in change.items():
            setattr(t, k, v)
        assert task_judge_hash(t) != h["A"], change
    assert task_judge_hash(_task("A", ["a1", "a2"])) == h["A"]


def test_verdict_currency_rule():
    tasks = _tasks()
    th, gh = task_judge_hashes(tasks), judge_hash(tasks)
    assert verdict_current({"task": "A", "task_judge_hash": th["A"], "judge_hash": "anything"}, th, gh)
    assert not verdict_current({"task": "A", "task_judge_hash": th["B"], "judge_hash": gh}, th, gh)
    assert not verdict_current({"task": "Z", "task_judge_hash": th["A"]}, th, gh)  # a task that is gone
    # An older record, without a task hash: the global hash decides.
    assert verdict_current({"task": "A", "judge_hash": gh}, th, gh)
    assert not verdict_current({"task": "A", "judge_hash": "OLD"}, th, gh)
    assert not verdict_current({"task": "A"}, th, gh)


def test_adding_or_changing_another_task_leaves_verdicts_standing(tmp_path):
    batch, out = tmp_path / "b", tmp_path / "b" / "judging"
    for t in ("A", "B"):
        for h in ("craze", "gx"):
            _write_run(batch, h, t)
    tasks = _tasks()
    assert _judge(batch, out, tasks) == {"judged": 2, "skipped": 0, "no_verdict": 0}
    recs = list(rp.read_jsonl(out / "verdicts.jsonl"))
    assert {r["task"]: r["task_judge_hash"] for r in recs} == {t: task_judge_hash(tasks[t]) for t in ("A", "B")}
    assert all(r["judge_hash"] == judge_hash(tasks) for r in recs)

    # A new, unrelated task: the global hash moves, nothing is re-judged, the report binds both.
    more = _tasks(C=_task("C", ["c1"]))
    assert _judge(batch, out, more) == {"judged": 0, "skipped": 2, "no_verdict": 0}
    rep = _report(batch, out, more)
    assert rep["verdicts_used"] == {"loaded": 2, "stale": 0, "bound_to_scored_runs": 2, "unbound": 0, "final": 2}
    assert rep["task_judge_hashes"] == {t: [task_judge_hash(more[t])] for t in ("A", "B")}

    # A's rubric changes (same length): only A's pair is judged again; B's stands.
    changed = _tasks(C=_task("C", ["c1"]))
    changed["A"].rubric = ["a1", "a2, reworded"]
    assert _judge(batch, out, changed) == {"judged": 1, "skipped": 1, "no_verdict": 0}
    cur, stale = rp.current_verdicts(list(rp.read_jsonl(out / "verdicts.jsonl")), changed)
    assert [v["task"] for v in stale] == ["A"] and sorted(v["task"] for v in cur) == ["A", "B"]
    assert stale[0]["task_judge_hash"] == task_judge_hash(tasks["A"])
    assert done_pairs(out, changed) == {(v["pair"], "single") for v in cur}
    rep = _report(batch, out, changed)
    assert rep["verdicts_used"]["final"] == 2 and rep["verdicts_used"]["stale"] == 1
    assert rep["task_judge_hashes"]["A"] == [task_judge_hash(changed["A"])]
    # Back to A's old rubric: the old A verdict stands again, and the newer one (made
    # under the reworded rubric) no longer hides it.
    cur, stale = rp.load_current_verdicts([out], _tasks(C=_task("C", ["c1"])))
    assert sorted(v["task"] for v in cur) == ["A", "B"] and [v["task"] for v in stale] == ["A"]
    assert next(v for v in cur if v["task"] == "A")["task_judge_hash"] == task_judge_hash(tasks["A"])


def test_an_old_record_follows_the_global_hash(tmp_path):
    batch, out = tmp_path / "b", tmp_path / "b" / "judging"
    for h in ("craze", "gx"):
        _write_run(batch, h, "A")
    tasks = _tasks()
    assert _judge(batch, out, tasks)["judged"] == 1
    # Rewrite the record as one from before per-task hashes (global hash only).
    rec = next(rp.read_jsonl(out / "verdicts.jsonl"))
    del rec["task_judge_hash"]
    (out / "verdicts.jsonl").write_text(json.dumps(rec) + "\n")
    assert done_pairs(out, tasks) == {(rec["pair"], "single")}
    assert _report(batch, out, tasks)["verdicts_used"]["final"] == 1
    # Any task added changes the global hash: the old record no longer stands.
    more = _tasks(C=_task("C", ["c1"]))
    assert done_pairs(out, more) == set()
    rep = _report(batch, out, more)
    assert rep["verdicts_used"]["stale"] == 1 and rep["verdicts_used"]["final"] == 0
    assert _judge(batch, out, more) == {"judged": 1, "skipped": 0, "no_verdict": 0}
