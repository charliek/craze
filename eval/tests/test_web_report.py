"""Reporting a web campaign (craze plan 051 E5; C10): the report's ``web`` section and
summary.json's ``web`` object -- pass per model and arm on web tasks, the web route's
counters per run, their totals and medians -- absent for every other campaign, whose
report, summary and archive are what they were; craze-web runs archive with their
counters."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from fakes import keyring

from crazeeval import archive as arch
from crazeeval import report as rp
from crazeeval import summary as sm
from crazeeval.tasks import Task, load_tasks, select_tasks
from test_report import _write_batch, world

M, N = "m-1", "m-2"
SPLITS = {"W1": "web", "W2": "web", "D1": "dev"}


def _tasks(root: Path | None = None) -> dict[str, Task]:
    out = {}
    for t, s in SPLITS.items():
        d = Path(".") if root is None else root / t
        if root is not None:
            d.mkdir(parents=True, exist_ok=True)
            (d / "task.toml").write_text(f'id = "{t}"\n')
        out[t] = Task(id=t, dir=d, category="feature", split=s, mode="build", prompt="p", repo_kind="fixture",
                      fixture="x", setup_patch=None, timeout_s=1, rubric=["r"], checks=[], validate={},
                      harnesses=["craze", "craze-web"] if s == "web" else None)
    return out


def _web(searches=0, fetches=0, misses=0, bytes_served=0, malformed=0, refused=0) -> dict:
    return {"searches": searches, "fetches": fetches, "misses": misses, "bytes_served": bytes_served,
            "malformed": malformed, "refused": refused}


def run(h, task, passed=True, status="ok", rep=1, model=M, web: dict | None = None, batch="B") -> dict:
    key = f"{h}/{model}/{task}/rep{rep}"
    r = {"harness": h, "model": model, "wire_model": "w", "task": task, "rep": rep, "status": status,
         "objective_pass": passed, "split": SPLITS.get(task, "web"), "category": "feature", "mode": "build",
         "plan_mode": None, "run_key": key, "run_id": f"{batch}:{key}:a1", "answer_words": 10,
         "checks": [{"name": "hidden", "type": "tests", "passed": passed}],
         "metrics": {"main_requests": 4, "tool_calls": 6, "tokens": {"input": 1000, "output": 100}, "cost": 0.01},
         "proxy": {"requests": 4, "forwarded": 4, "refused": {}, "key_exposure_refusals": 0, "upstream_errors": 0,
                   "flags": []},
         "wall_s": 20.0}
    if web is not None:
        r["proxy"]["web"] = web
    return r


def web_world() -> list[dict]:
    """A web campaign: M on craze and craze-web (two tasks, two reps), N on craze-web
    alone, plus an infra run (unscored) and a dev run (not a web run)."""
    return [
        run("craze", "W1", False), run("craze", "W1", False, rep=2), run("craze", "W2", True),
        run("craze-web", "W1", True, web=_web(3, 2, 1, 9000)),
        run("craze-web", "W1", True, rep=2, web=_web(1, 1, 0, 3000)),
        run("craze-web", "W2", False, web=_web(4, 0, 0, 6000, malformed=1)),
        run("craze-web", "W2", False, rep=2, status="infra", web=_web(9, 9, 9, 99999)),
        run("craze-web", "W1", True, model=N, web=_web(2, 3, 0, 7000)),
        run("craze", "D1", True),
    ]


def _report(runs, tasks=None, **kw) -> dict:
    return rp.build(rp.ReportInput(runs=runs, verdicts=[], tasks=_tasks() if tasks is None else tasks, unseal=False,
                                   batches=["/home/someone/camp/eval-runs/web-x"], **kw))


def test_the_report_web_section():
    w = _report(web_world())["web"]
    assert w["tasks"] == ["W1", "W2"]
    assert w["pass"][f"{M}/craze"] == {"runs": 3, "unscored": 0, "passed": 1, "rate": 0.3333,
                                       "ci90": [round(x, 4) for x in rp.wilson(1, 3)], "tasks": {"W1": 0.0, "W2": 1.0}}
    # The infra run is unscored: out of the rate and the counters, still listed per run.
    assert w["pass"][f"{M}/craze-web"] == {"runs": 3, "unscored": 1, "passed": 2, "rate": 0.6667,
                                           "ci90": [round(x, 4) for x in rp.wilson(2, 3)],
                                           "tasks": {"W1": 1.0, "W2": 0.0}}
    assert w["pass"][f"{N}/craze-web"]["rate"] == 1.0 and f"{N}/craze" not in w["pass"]
    # Counters: craze-web only, scored runs; totals and per-run medians.
    assert set(w["counters"]) == {f"{M}/craze-web", f"{N}/craze-web"}
    c = w["counters"][f"{M}/craze-web"]
    assert c["runs"] == 3
    assert c["totals"] == _web(8, 3, 1, 18000, malformed=1)
    assert c["medians"] == _web(3, 1, 0, 6000)
    two = rp.web_section([run("craze-web", "W1", web=_web(1, 0, 0, 10)), run("craze-web", "W1", rep=2, web=_web(2))],
                         _tasks())
    assert two["counters"][f"{M}/craze-web"]["medians"]["searches"] == 1.5  # two middles straddle a half
    assert all(isinstance(v, int) for cc in w["counters"].values() for v in cc["totals"].values())
    # The web runs' own metric medians; the dev run is not a web run.
    assert set(w["metrics"]) == {f"{M}/craze", f"{M}/craze-web", f"{N}/craze-web"}
    assert w["metrics"][f"{M}/craze-web"]["main requests"] == 4.0
    # Per run: every run with counters (the infra one too), by task, model, harness, rep.
    assert [(r["task"], r["model"], r["rep"], r["status"]) for r in w["runs"]] == [
        ("W1", M, 1, "ok"), ("W1", M, 2, "ok"), ("W1", N, 1, "ok"), ("W2", M, 1, "ok"), ("W2", M, 2, "infra")]
    assert w["runs"][0] == {"task": "W1", "model": M, "harness": "craze-web", "rep": 1, "status": "ok",
                            "objective_pass": True, **_web(3, 2, 1, 9000)}


def test_a_web_run_is_known_by_its_task_or_its_route():
    tasks = _tasks()
    assert rp.is_web_run(run("craze", "W1"), tasks) and not rp.is_web_run(run("craze", "D1"), tasks)
    gone = {**run("craze", "W9"), "split": "web"}  # a task since removed: as the run recorded it
    assert rp.is_web_run(gone, tasks)
    assert rp.is_web_run(run("craze-web", "D1", web=_web()), tasks)  # a route with a corpus
    assert rp.web_counters(run("craze", "W1")) is None and rp.web_counters({"harness": "craze"}) is None
    assert rp.web_counters({"proxy": {"web": {"searches": 2.0, "fetches": "x"}}}) == _web(2)


def test_the_recorded_split_decides_over_the_tasks_current_one():
    """Review C10 (sol, P2): a run is classified by the split its result.json recorded;
    the task's current split is read only when the run recorded none."""
    tasks = _tasks()
    # Recorded dev, no corpus route, its task since moved to web: not a web run, and an
    # old campaign of such runs gains no web section.
    was_dev = {**run("craze", "W1"), "split": "dev"}
    assert not rp.is_web_run(was_dev, tasks)
    assert "web" not in _report([was_dev], tasks)
    # Recorded web on plain craze, its task since moved to dev: still a web run.
    was_web = {**run("craze", "D1"), "split": "web"}
    assert rp.is_web_run(was_web, tasks)
    assert _report([was_web], tasks)["web"]["pass"][f"{M}/craze"]["runs"] == 1
    # No split recorded (a result written before the attempt ran): the task's current one.
    unrecorded = {k: v for k, v in run("craze", "W1").items() if k != "split"}
    assert rp.is_web_run(unrecorded, tasks)
    assert not rp.is_web_run({k: v for k, v in run("craze", "D1").items() if k != "split"}, tasks)


def test_the_web_section_renders():
    rep = _report(web_world())
    md = rp.render_markdown(rep)
    assert "## Web tasks (craze plan 051 E5)" in md and "Tasks: W1, W2." in md
    assert f"| {M} | craze-web | 2 of 3 | 67% [" in md and f"| {M} | craze | 1 of 3 | 33% [" in md
    assert f"| {M} | craze-web | 3 | 8 (3) | 3 (1) | 1 (0) | 18000 (6000) | 1 (0) | 0 (0) |" in md
    assert f"| W2 | {M} | craze-web | 2 | infra | no | 9 | 9 | 9 | 99999 | 0 | 0 |" in md
    # A web campaign of plain craze alone still renders (no counters).
    md = rp.render_markdown(_report([run("craze", "W1")]))
    assert "| – | – | 0 | – | – | – | – | – | – |" in md and "Per run:" not in md


def test_no_web_section_without_web_runs(tmp_path):
    """A batch without web runs: no ``web`` key and no section -- the report's bytes are
    what they are with the web code taken out entirely."""
    runs, vs = world({}, {"D1": False}, {}, {})
    from test_report import _tasks as plain_tasks

    def build_and_write(out: Path) -> dict[str, bytes]:
        rep = rp.build(rp.ReportInput(runs=runs, verdicts=vs, tasks=plain_tasks(), unseal=True, batches=["b"],
                                      losses=True, compare_runs=runs, compare_verdicts=vs))
        assert "web" not in rep
        rp.write(rep, out)
        return {p.name: p.read_bytes() for p in sorted(out.iterdir())}

    now = build_and_write(tmp_path / "now")
    assert b"Web tasks" not in now["report.md"] and b'"web"' not in now["report.json"]
    mp = pytest.MonkeyPatch()
    try:
        mp.setattr(rp, "web_section", lambda runs, tasks: None)
        assert build_and_write(tmp_path / "without") == now
    finally:
        mp.undo()


# -- summary.json ---------------------------------------------------------------------------------------------------


def test_the_summary_web_object(tmp_path):
    rdir = tmp_path / "report-web"
    rp.write(_report(web_world()), rdir)
    out = tmp_path / "summary.json"
    sm.summarize([rdir], out, "2026-11-web", allow_sealed=True)
    text = out.read_text()
    doc = json.loads(text)
    assert doc["schema"] == sm.SCHEMA == 1
    g = doc["groups"][0]
    # N ran on craze-web alone and is covered: its web rows are kept.
    assert g["models"] == [M, N] and g["other_models"] == []
    w = g["web"]
    assert set(w) == {"tasks", "pass", "counters", "metrics"} and w["tasks"] == ["W1", "W2"]
    assert set(w["pass"]) == {f"{M}/craze", f"{M}/craze-web", f"{N}/craze-web"}
    assert w["pass"][f"{N}/craze-web"]["passed"] == 1 and w["pass"][f"{M}/craze-web"]["unscored"] == 1
    assert w["counters"][f"{M}/craze-web"] == {"runs": 3, "totals": _web(8, 3, 1, 18000, malformed=1),
                                              "medians": _web(3, 1, 0, 6000)}
    assert w["counters"][f"{N}/craze-web"]["totals"]["fetches"] == 3
    assert all(type(v) is int for c in w["counters"].values() for v in c["totals"].values())
    assert "runs" not in w  # the per-run rows stay in the report and the archive
    assert sm.scan(doc, text) == [] and "/home/" not in text


def test_covered_models_count_craze_web_as_craze():
    rep = {"objective": {f"{M}/craze-web": {}, f"{N}/gx": {}}, "success_bar": {M: {}, N: {}}}
    assert sm.covered_models(rep) == [M]
    rep["objective"][f"{N}/craze"] = {}
    assert sm.covered_models(rep) == [M, N]


def test_a_summary_without_web_runs_has_no_web_object(tmp_path):
    runs, vs = world({}, {}, {}, {})
    from test_report import _tasks as plain_tasks

    rep = rp.build(rp.ReportInput(runs=runs, verdicts=vs, tasks=plain_tasks(), unseal=True, batches=["b"]))
    g = sm.group("r", rep)
    assert "web" not in g
    # And a report written before the web section existed (no key at all) summarizes as before.
    assert sm.group("r", {k: v for k, v in rep.items() if k != "web"}) == g


# -- archive ----------------------------------------------------------------------------------------------------------


def test_craze_web_runs_archive_with_their_counters(tmp_path):
    tasks = _tasks(tmp_path / "tasks")
    batch = tmp_path / "camp" / "eval-runs" / "web-x"
    runs = [run(r["harness"], r["task"], r["objective_pass"], r["status"], r["rep"], r["model"],
                (r.get("proxy") or {}).get("web"), batch=batch.name) for r in web_world()]
    _write_batch(batch, runs, [])
    (batch / "batch.json").write_text(json.dumps({
        "label": "web-x", "harnesses": ["craze", "craze-web"], "reps": 2, "models": {M: {"key": M}},
        "tasks": {}, "executables": {"craze": {"sha256": "11" * 32, "version": "0.0.1"}}, "fingerprint": "fp"}))
    s = arch.archive([batch], tmp_path / "arch", keyring(), tasks=tasks)
    assert s["ok"], s["errors"]
    rows = {(r["harness"], r["model"], r["task"], r["rep"]): r
            for r in map(json.loads, (tmp_path / "arch" / arch.RUNS_FILE).read_text().splitlines())}
    assert len(rows) == 8  # the infra run is not scored
    web = rows[("craze-web", M, "W1", 1)]
    assert web["web"] == _web(3, 2, 1, 9000) and web["harness_version"] == "0.0.1" and web["split"] == "web"
    assert "web" not in rows[("craze", M, "W1", 1)] and "web" not in rows[("craze", M, "D1", 1)]


def test_old_rows_archive_as_they_did():
    """A run with no web route (every run of an old campaign) gets no ``web`` key."""
    info = {"executables": {"craze": {"version": "v", "sha256": "s"}}, "craze_build": None}
    old = {"harness": "craze", "metrics": {}, "proxy": {"requests": 1, "flags": []}}
    assert "web" not in arch.run_row(old, info) and "web" not in arch.run_row({"harness": "gx"}, info)


# -- the web split in selection -------------------------------------------------------------------------------------


def test_web_tasks_run_only_when_named():
    tasks = load_tasks()
    web = sorted(t.id for t in tasks.values() if t.split == "web")
    assert web and all(tasks[t].harnesses == ["craze", "craze-web"] for t in web)
    for spec in (None, "split:all", "split:dev,split:heldout"):
        assert not [t for t in select_tasks(tasks, spec) if t.split == "web"], spec
    assert sorted(t.id for t in select_tasks(tasks, "split:web")) == web
