"""The report (plan 029 §3.1.8): Wilson intervals, the success bar on synthetic batches
(meets / misses / inconclusive), best-open-harness choice fixed from the baseline,
verdicts bound to the scored runs and one final verdict per pair, sealing, compare,
length bias."""

from __future__ import annotations

import json
import math
from pathlib import Path

import pytest

from crazeeval import report as rp
from crazeeval.report import WinRate, success_bar, wilson

DEV = ["D1", "D2", "D3"]
HELD = ["H1", "H2"]
SPLITS = {**{t: "dev" for t in DEV}, **{t: "heldout" for t in HELD}}
M = "muse-spark-1.3"


def rid(h, task, rep=1, model=M, batch="B"):
    return f"{batch}:{h}/{model}/{task}/rep{rep}:a1"


def run(h, task, passed=True, status="ok", rep=1, model=M, words=100, batch="B"):
    return {"harness": h, "model": model, "task": task, "rep": rep, "status": status, "objective_pass": passed,
            "split": SPLITS.get(task), "run_key": f"{h}/{model}/{task}/rep{rep}", "answer_words": words,
            "run_id": rid(h, task, rep, model, batch),
            "metrics": {"main_requests": 3, "tool_calls": 5, "tokens": {"input": 1000, "output": 100}, "cost": 0.01},
            "wall_s": 30.0}


def verdict(task, hx, hy, winner, model=M, both=True, conf="high", agree=True, xb="B", yb="B", judge="sol"):
    return {"pair": f"{task}|{model}|{hx}|{hy}", "task": task, "model": model, "hx": hx, "hy": hy,
            "x": f"{hx}/{model}/{task}/rep1", "y": f"{hy}/{model}/{task}/rep1", "split": SPLITS.get(task),
            "x_run_id": rid(hx, task, model=model, batch=xb), "y_run_id": rid(hy, task, model=model, batch=yb),
            "both_orders": both, "judge_model": judge,
            "result": None if winner is None else {"winner": winner, "agree": agree, "confidence": conf,
                                                   "score_x": 5, "score_y": 5}}


def world(native_pass: dict, gx_pass: dict, oc_pass: dict, winners: dict):
    runs = []
    for t in DEV + HELD:
        runs.append(run("craze", t, native_pass.get(t, True)))
        runs.append(run("gx", t, gx_pass.get(t, True)))
        runs.append(run("opencode", t, oc_pass.get(t, True)))
    vs = [verdict(t, "craze", "gx", winners.get(t, "x")) for t in DEV + HELD]
    vs += [verdict(t, "craze", "opencode", winners.get(t, "x")) for t in DEV + HELD]
    return runs, vs


def test_wilson_interval():
    lo, hi = wilson(5, 10)
    assert 0.268 < lo < 0.270 and 0.730 < hi < 0.732  # z = 1.645 around 0.5
    assert wilson(0, 0) == (0.0, 1.0)
    lo, hi = wilson(10, 10)
    assert hi == pytest.approx(1.0) and 0.78 < lo < 0.80
    w = WinRate()
    for o in (1, 0.5, 0.5, 0):
        w.add(o)
    assert (w.wins, w.ties, w.losses, w.n, w.rate) == (1, 2, 1, 4, 0.5)  # ties count half
    assert w.ci == wilson(2.0, 4)


def test_success_bar_meets():
    runs, vs = world({}, {"D1": False}, {"D1": False, "D2": False}, {})
    sb = success_bar(runs, vs, SPLITS, M, unsealed=True)
    assert sb["best_open_harness"]["harness"] == "gx"  # more objective passes than opencode
    assert sb["outcome"] == "meets", sb
    assert sb["all_tasks"]["win_rate"]["rate"] == 1.0 and sb["heldout"]["objective_ok"]


def test_success_bar_misses_on_the_held_out_tasks_alone():
    # All fifteen look fine, but on the held-out tasks the native harness loses.
    runs, vs = world({}, {}, {}, {"H1": "y", "H2": "y"})
    sb = success_bar(runs, vs, SPLITS, M, unsealed=True)
    assert sb["outcome"] == "misses"
    assert sb["all_tasks"]["win_rate"]["rate"] == pytest.approx(0.6) and sb["all_tasks"]["win_ok"]
    assert not sb["heldout"]["win_ok"] and "held-out: win rate" in sb["reason"]


def test_success_bar_misses_on_objective_count():
    runs, vs = world({"D1": False}, {}, {}, {})
    sb = success_bar(runs, vs, SPLITS, M, unsealed=True)
    assert sb["outcome"] == "misses" and "objective 4 < 5" in sb["reason"]


def test_success_bar_inconclusive_without_coverage():
    runs, vs = world({}, {}, {}, {"H2": None})  # one pair never got a verdict
    sb = success_bar(runs, vs, SPLITS, M, unsealed=True)
    assert sb["outcome"] == "inconclusive" and sb["coverage"]["missing"]["verdict"] == ["H2"]
    runs, vs = world({}, {}, {}, {})
    runs = [r if not (r["harness"] == "craze" and r["task"] == "D2") else {**r, "status": "infra"} for r in runs]
    sb = success_bar(runs, vs, SPLITS, M, unsealed=True)
    assert sb["outcome"] == "inconclusive" and sb["coverage"]["missing"]["native_run"] == ["D2"]


def test_success_bar_sealed_is_provisional_and_dev_only():
    runs, vs = world({}, {}, {}, {"H1": "y", "H2": "y"})
    dev_runs = [r for r in runs if r["split"] == "dev"]
    dev_vs = [v for v in vs if v["split"] == "dev"]
    sb = success_bar(dev_runs, dev_vs, SPLITS, M, unsealed=False)
    assert sb["outcome"] == "provisional-meets" and "heldout" not in sb and sb["sealed"]


def test_best_open_harness_tiebreak_by_their_head_to_head():
    runs, vs = world({}, {"D1": False}, {"D2": False}, {})
    counts = rp.objective_counts(runs, SPLITS)
    vs.append(verdict("D3", "gx", "opencode", "y"))
    assert rp.best_open_harness(counts, vs, M)["harness"] == "opencode"
    vs[-1] = verdict("D3", "gx", "opencode", "x")
    assert rp.best_open_harness(counts, vs, M)["harness"] == "gx"


def test_two_reps_count_as_halves():
    runs = [run("craze", "D1", True, rep=1), run("craze", "D1", False, rep=2), run("craze", "D2", True)]
    c = rp.objective_counts(runs, SPLITS)[(M, "craze")]
    assert c["dev"] == 1.5 and c["tasks"] == {"D1": 0.5, "D2": 1.0}


def test_verdicts_bind_to_the_exact_scored_runs():
    runs, vs = world({}, {}, {}, {})
    # A verdict on an older build's craze run (same task, same key, another batch) and
    # one on a run that ended as infra: neither counts.
    stale = verdict("D1", "craze", "gx", "y", xb="OLD")
    runs = [r if not (r["harness"] == "opencode" and r["task"] == "D3") else {**r, "status": "infra"} for r in runs]
    bound, dropped = rp.bind_verdicts(vs + [stale], runs)
    assert dropped == 2 and stale not in bound
    assert all(not (v["hy"] == "opencode" and v["task"] == "D3") for v in bound)
    # Only the stale verdict for D2's gx pair: coverage is incomplete, never met by it.
    runs2, vs2 = world({}, {}, {}, {})
    vs2 = [v for v in vs2 if not (v["task"] == "D2" and v["hy"] == "gx")] + [verdict("D2", "craze", "gx", "x", xb="OLD")]
    bound2, _ = rp.bind_verdicts(vs2, runs2)
    sb = success_bar(runs2, rp.final_verdicts(bound2), SPLITS, M, unsealed=True)
    assert sb["outcome"] == "inconclusive" and sb["coverage"]["missing"]["verdict"] == ["D2"]


def test_one_final_verdict_per_pair():
    single = verdict("D1", "craze", "gx", "y", both=False)
    sol = verdict("D1", "craze", "gx", "x")
    astra = verdict("D1", "craze", "gx", "tie", judge="astra")
    for order in ([single, sol, astra], [astra, sol, single], [sol, astra, single]):
        final = rp.final_verdicts(order)
        assert len(final) == 1 and final[0]["judge_model"] == "astra"
    assert rp.final_verdicts([single, sol])[0] is sol
    later_sol = verdict("D1", "craze", "gx", "y")
    assert rp.final_verdicts([sol, later_sol])[0] is later_sol  # the same rank: the later record
    # A failed final verdict is no verdict; a lesser one does not stand in for it.
    failed = verdict("D1", "craze", "gx", None, judge="astra")
    assert rp.final_verdicts([sol, failed])[0]["result"] is None
    # Win rates count each pair once.
    wr = rp.win_matrix(rp.final_verdicts([single, sol, astra]), SPLITS, model=M)[("craze", "gx")]
    assert wr.n == 1 and wr.ties == 1


def test_best_open_harness_is_fixed_from_the_baseline_and_recorded(tmp_path):
    base_runs, base_vs = world({}, {}, {"D1": False}, {})  # baseline: gx 5, opencode 4
    base = _write_batch(tmp_path / "base", base_runs, base_vs)
    # The final batch re-runs the open harnesses on two tasks (a drift check): merged,
    # opencode would now lead gx 5 to 4 -- but the baseline's choice must stand.
    final_runs = [run("opencode", "D1", True, batch="F"), run("gx", "D2", False, batch="F"),
                  run("craze", "D1", True, batch="F")]
    final = _write_batch(tmp_path / "final", final_runs, [])
    tasks = _tasks()
    merged = rp.load_runs([base, final], unseal=True)
    assert rp.best_open_harness(rp.objective_counts(merged, SPLITS), [], M)["harness"] == "opencode"
    vs = rp.load_verdicts([base / "judging"], True)
    rep = rp.build(rp.ReportInput(runs=merged, verdicts=vs, tasks=tasks, unseal=True, batches=[str(base), str(final)],
                                  baseline_batch=base, baseline_runs=rp.load_runs([base], unseal=True)))
    sb = rep["success_bar"][M]
    assert sb["best_open_harness"]["harness"] == "gx" and "recorded" in sb["best_open_harness"]["source"]
    record = json.loads((base / rp.BEST_OPEN_FILE).read_text())
    assert record["models"][M]["harness"] == "gx" and record["baseline_batch"] == str(base)
    # A later change to the baseline's own results cannot move it: the record stands.
    flipped = [dict(r, objective_pass=(r["harness"] != "gx")) for r in rp.load_runs([base], unseal=True)]
    again = rp.fixed_best_open_harness(base, flipped, [], SPLITS, [M], unsealed=True)
    assert again[M]["harness"] == "gx" and again[M]["source"].startswith("recorded in")
    # Sealed, the choice is provisional (dev tasks of the baseline) and not recorded.
    other = tmp_path / "other"
    other.mkdir()
    prov = rp.fixed_best_open_harness(other, base_runs, [], SPLITS, [M], unsealed=False)
    assert prov[M]["source"].startswith("provisional") and not (other / rp.BEST_OPEN_FILE).exists()


def _write_batch(root: Path, runs: list[dict], verdicts: list[dict]) -> Path:
    for r in runs:
        part = "heldout" if r["split"] == "heldout" else "runs"
        d = root / part / r["harness"] / r["model"] / r["task"] / f"rep{r['rep']}"
        d.mkdir(parents=True)
        (d / "result.json").write_text(json.dumps(r))
    for v in verdicts:
        p = root / "judging" / ("heldout" if v["split"] == "heldout" else "") / "verdicts.jsonl"
        p.parent.mkdir(parents=True, exist_ok=True)
        with open(p, "a") as f:
            f.write(json.dumps(v) + "\n")
    return root


def _tasks():
    from crazeeval.tasks import Task

    return {t: Task(id=t, dir=Path("."), category="explain", split=s, mode="build", prompt="p", repo_kind="fixture",
                    fixture="x", setup_patch=None, timeout_s=1, rubric=["r"], checks=[], validate={})
            for t, s in SPLITS.items()}


def test_report_keeps_the_held_out_part_sealed(tmp_path):
    runs, vs = world({}, {}, {}, {})
    b = _write_batch(tmp_path / "b", runs, vs)
    sealed_runs = rp.load_runs([b], unseal=False)
    assert {r["task"] for r in sealed_runs} == set(DEV)
    assert {v["task"] for v in rp.load_verdicts([b / "judging"], unseal=False)} == set(DEV)
    rep = rp.build(rp.ReportInput(runs=sealed_runs, verdicts=rp.load_verdicts([b / "judging"]), tasks=_tasks(),
                                  unseal=False, batches=[str(b)]))
    md = rp.render_markdown(rep)
    assert rep["sealed"] and "**sealed**" in md
    assert "H1" not in md and "H2" not in md and "heldout|" not in json.dumps(rep["win_rates"])
    assert rep["success_bar"][M]["outcome"] == "provisional-meets"
    rep = rp.build(rp.ReportInput(runs=rp.load_runs([b], unseal=True), verdicts=rp.load_verdicts([b / "judging"], True),
                                  tasks=_tasks(), unseal=True, batches=[str(b)]))
    assert rep["success_bar"][M]["outcome"] == "meets"
    assert "H1" in rp.render_markdown(rep)
    rp.write(rep, tmp_path / "out")
    assert (tmp_path / "out" / "report.md").exists() and (tmp_path / "out" / "length_vs_verdict.csv").exists()


def test_a_later_batch_replaces_the_same_run(tmp_path):
    base = _write_batch(tmp_path / "base", [run("craze", "D1", False), run("gx", "D1", True)], [])
    final = _write_batch(tmp_path / "final", [run("craze", "D1", True)], [])
    runs = rp.load_runs([base, final])
    by = {(r["harness"], r["task"]): r for r in runs}
    assert by[("craze", "D1")]["objective_pass"] and by[("gx", "D1")]["batch"] == str(base)


def test_compare_two_builds():
    new = [run("craze", t, batch="N") for t in DEV]
    old = [run("craze", t, t != "D1", batch="O") for t in DEV]
    vs = [verdict(t, "craze", "craze", w, xb="N", yb="O") for t, w in zip(DEV, ["x", "x", "tie"], strict=True)]
    c = rp.compare(new, old, vs, SPLITS)
    assert c["pooled"]["rate"] == pytest.approx(5 / 6, abs=1e-4)
    assert c["per_model"][M]["objective_new"]["dev"] == 3 and c["per_model"][M]["objective_old"]["dev"] == 2
    assert c["keep_rule"] == {"conditional_keep": rp.KEEP, "requested_keep": rp.KEEP}
    c = rp.compare(old, new, [verdict(t, "craze", "craze", "y", xb="O", yb="N") for t in DEV], SPLITS)
    assert c["keep_rule"]["conditional_keep"] == rp.DROP and c["per_model"][M]["objective_drop"]


def test_compare_without_full_verdict_coverage_is_inconclusive():
    new = [run("craze", t, batch="N") for t in DEV]
    old = [run("craze", t, batch="O") for t in DEV]
    c = rp.compare(new, old, [], SPLITS)
    assert c["keep_rule"] == {"conditional_keep": rp.INCONCLUSIVE, "requested_keep": rp.INCONCLUSIVE}
    assert not c["coverage"]["complete"]
    part = [verdict("D1", "craze", "craze", "x", xb="N", yb="O")]
    c = rp.compare(new, old, part, SPLITS)
    assert c["keep_rule"]["requested_keep"] == rp.INCONCLUSIVE and c["coverage"]["missing"] == [f"{M}/D2", f"{M}/D3"]
    # Verdicts on other builds' runs do not count toward it.
    stale = [verdict(t, "craze", "craze", "x", xb="Z", yb="O") for t in DEV]
    assert rp.compare(new, old, stale, SPLITS)["keep_rule"]["conditional_keep"] == rp.INCONCLUSIVE


def test_length_bias_bins_and_the_longer_side():
    runs = [run("craze", "D1", words=300), run("gx", "D1", words=100), run("craze", "D2", words=100),
            run("gx", "D2", words=100)]
    vs = [verdict("D1", "craze", "gx", "x"), verdict("D2", "craze", "gx", "tie")]
    lb = rp.length_bias(vs, runs)
    assert lb["points"] == 2
    top = next(b for b in lb["bins"] if b["log_ratio"][0] == 1.0)
    mid = next(b for b in lb["bins"] if b["log_ratio"][0] == -0.3)
    assert top["n"] == 1 and top["rate"] == 1.0 and mid["n"] == 1 and mid["rate"] == 0.5
    assert lb["longer_side"]["n"] == 1 and lb["longer_side"]["rate"] == 1.0
    assert lb["rows"][0]["log_ratio"] == pytest.approx(math.log(301 / 101), abs=1e-3)


def test_task_table_marks():
    vs = [verdict("D1", "craze", "gx", "x"), verdict("D1", "gx", "craze", "x", conf="low", xb="C"),
          verdict("D2", "craze", "opencode", None)]
    t = rp.task_table(vs, M)
    assert t["D1"]["gx"] == "WLl" and t["D2"]["opencode"] == "·"


def _order(x_is_a, winner, rubric_x, rubric_y, reasons, judge="sol"):
    return {"judge_model": judge, "x_is_a": x_is_a, "verdict": {
        "winner": winner, "confidence": "high", "score_x": 5, "score_y": 8, "reasons": reasons,
        "rubric_x": [{"item": i, "grade": g} for i, g in enumerate(rubric_x, 1)],
        "rubric_y": [{"item": i, "grade": g} for i, g in enumerate(rubric_y, 1)]}}


def test_where_craze_lost_on_a_synthetic_record():
    """The report's --losses section (plan 029 W2): a craze run that lost, on either
    side, with the rubric items it did not meet against the other side's and the judge's
    reasons per order; wins, ties and craze-vs-craze pairs are not losses."""
    lost_as_y = verdict("D1", "opencode", "craze", "x")  # craze is y and lost
    lost_as_y["orders"] = [
        _order(True, "x", ["met", "met", "met"], ["met", "missed", "false"],
               "opencode showed the traceback; craze item 3 false"),
        _order(False, "x", ["met", "met", "missed"], ["met", "missed", "met"], "item 2 decided it"),
    ]
    lost_as_y["result"].update({"score_x": 8.5, "score_y": 5.5})
    lost_as_x = verdict("D2", "craze", "gx", "y", conf="medium")
    lost_as_x["orders"] = [_order(True, "y", ["missed", "met", "met"], ["met", "met", "met"], "gx covered item 1")]
    others = [verdict("D3", "craze", "gx", "x"), verdict("D3", "craze", "opencode", "tie"),
              verdict("D2", "craze", "craze", "y", xb="N", yb="O"), verdict("H1", "craze", "gx", None)]
    losses = rp.craze_losses([lost_as_y, lost_as_x] + others)
    assert [(x["task"], x["other_harness"]) for x in losses] == [("D1", "opencode"), ("D2", "gx")]
    one = losses[0]
    assert one["craze_run_key"] == f"craze/{M}/D1/rep1" and one["other_run_key"] == f"opencode/{M}/D1/rep1"
    assert one["craze_score"] == 5.5 and one["other_score"] == 8.5 and one["split"] == "dev"
    first = one["orders"][0]
    assert first["craze_shown_as"] == "B" and first["winner"] == "other"  # craze is y; x was shown as A
    assert first["craze_not_met"] == [{"item": 2, "grade": "missed"}, {"item": 3, "grade": "false"}]
    assert first["other_not_met"] == [] and first["craze_score"] == 8 and first["other_score"] == 5
    assert "traceback" in first["reasons"] and one["orders"][1]["craze_shown_as"] == "A"
    assert losses[1]["orders"][0]["craze_not_met"] == [{"item": 1, "grade": "missed"}]

    runs = [run(h, t) for h in ("craze", "gx", "opencode") for t in ("D1", "D2", "D3")]
    tasks = _tasks()
    tasks["D1"].rubric = ["uses X", "reads Y at a.go:10", "says Z"]
    rep = rp.build(rp.ReportInput(runs=runs, verdicts=[lost_as_y, lost_as_x] + others[:2], tasks=tasks, unseal=False,
                                  losses=True))
    assert [x["task"] for x in rep["losses"]] == ["D1", "D2"] and rep["rubrics"]["D1"][1] == "reads Y at a.go:10"
    md = rp.render_markdown(rep)
    assert "## Where craze lost" in md and "2 final verdict(s) in which a craze run lost" in md
    assert "craze did not meet: 2 (missed), 3 (false); opencode did not meet: none" in md
    assert "only craze missed item 2: reads Y at a.go:10" in md and "reasons: opencode showed the traceback" in md
    assert "## Where craze lost" not in rp.render_markdown(rp.build(rp.ReportInput(
        runs=runs, verdicts=[lost_as_y], tasks=tasks, unseal=False)))
