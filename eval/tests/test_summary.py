"""``crazeeval summarize``: report.json files become one slim, deterministic summary --
the bar per covered model with its conditions, objective counts, win rates per split,
medians and task tables, a provenance digest of names and hashes -- with no verdict or
loss lists and no path; a sealed report is refused, and a planted path writes nothing."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from crazeeval import paths
from crazeeval import report as rp
from crazeeval import summary as sm
from crazeeval.cli import main
from crazeeval.tasks import Task

M = "m-1"
STRAY = "stray-model"  # gx plan-task runs reused from a shared batch: no craze, no bar
LOSS_REASON = "LOSS-REASON-never-summarized"
HASH = "163ea21f" + "0" * 56


def _wr(w: int, t: int, lo: int, rate: float) -> dict:
    return {"wins": w, "ties": t, "losses": lo, "n": w + t + lo, "rate": rate, "ci90": [0.31, 0.69]}


def _cond(native: float, other: float, wr: dict, ok: bool = True) -> dict:
    return {"objective_native": native, "objective_other": other, "objective_ok": native >= other, "win_rate": wr,
            "win_ok": ok}


def _report(root: Path, tag: str, *, sealed: bool = False, missing_verdict: list[str] | None = None) -> dict:
    """A report.json as ``crazeeval report`` writes it (absolute paths, a stray model's
    rows, a length-bias block with infinite bins, a losses section)."""
    runs = root / "camp" / "eval-runs"
    base, shared, final = (str(runs / f"{k}-{tag}") for k in ("base", "gxplan", "final"))
    best = {"harness": "gx", "reason": "tied counts; gx vs opencode win rate 0.79", "counts": {"gx": 14.0, "opencode": 14.0},
            "baseline_batch": base, "source": f"recorded in {base}/best-open-harness.json"}
    tasks = {"T-1": 1.0, "T-2": 0.5, "smoke-fix": 1.0}
    rep = {
        "sealed": sealed,
        "batches": [base, shared, final],
        "verdict_sources": [f"{final}/judging/verdicts.jsonl", f"{final}/judging/heldout/verdicts.jsonl"],
        "judge_hashes": [HASH],
        "judge_hash_now": HASH,
        "task_judge_hashes": {"T-1": ["aa" * 32]},
        "baseline_batch": base,
        "verdicts_used": {"loaded": 31, "stale": 1, "bound_to_scored_runs": 30, "unbound": 0, "final": 30},
        "models": [M, STRAY],
        "harnesses": ["craze", "gx", "opencode"],
        "objective": {
            f"{M}/craze": {"dev": 7.0, "heldout": 7.5, "all": 14.5, "tasks": tasks},
            f"{M}/gx": {"dev": 7.0, "heldout": 8.0, "all": 15.0, "tasks": {**tasks, "T-2": 1.0}},
            f"{M}/opencode": {"dev": 7.0, "heldout": 7.0, "all": 14.0, "tasks": tasks},
            f"{STRAY}/gx": {"dev": 1.0, "heldout": 1.0, "all": 2.0, "tasks": {"T-P1": 1.0}},
        },
        "statuses": {f"{M}/craze": {"ok": 32}, f"{M}/gx": {"ok": 16, "crashed": 1}, f"{STRAY}/gx": {"ok": 2}},
        # The stray model HAS verdicts (gx vs opencode, a pair the covered model has too):
        # the report's pooled matrix counts them, the summary's must not.
        "win_rates": {
            f"dev|{M}": {"craze vs gx": _wr(6, 3, 5, 0.5357), "gx vs craze": _wr(5, 3, 6, 0.4643),
                         "gx vs opencode": _wr(1, 0, 1, 0.5), "opencode vs gx": _wr(1, 0, 1, 0.5),
                         "craze vs opencode": _wr(0, 0, 0, None)},
            f"dev|{STRAY}": {"gx vs opencode": _wr(2, 0, 0, 1.0), "opencode vs gx": _wr(0, 0, 2, 0.0)},
            "dev|pooled": {"craze vs gx": _wr(6, 3, 5, 0.5357), "gx vs craze": _wr(5, 3, 6, 0.4643),
                           "gx vs opencode": _wr(3, 0, 1, 0.75), "opencode vs gx": _wr(1, 0, 3, 0.25),
                           "craze vs opencode": _wr(0, 0, 0, None)},
            f"heldout|{M}": {"craze vs gx": _wr(7, 2, 7, 0.5), "gx vs craze": _wr(7, 2, 7, 0.5)},
            f"heldout|{STRAY}": {"gx vs opencode": _wr(0, 1, 0, 0.5), "opencode vs gx": _wr(0, 1, 0, 0.5)},
            "heldout|pooled": {"craze vs gx": _wr(7, 2, 7, 0.5), "gx vs craze": _wr(7, 2, 7, 0.5),
                               "gx vs opencode": _wr(0, 1, 0, 0.5), "opencode vs gx": _wr(0, 1, 0, 0.5)},
        },
        "success_bar": {
            M: {"model": M, "best_open_harness": best, "sealed": sealed, "outcome": "misses",
                "reason": "all fifteen: objective 14.5 < 15; held-out: objective 7.5 < 8",
                "all_tasks": _cond(14.5, 15.0, _wr(13, 5, 12, 0.5167)),
                "heldout": _cond(7.5, 8.0, _wr(7, 2, 7, 0.5)),
                "dev": _cond(7.0, 7.0, _wr(6, 3, 5, 0.5357)),
                "coverage": {"complete": not missing_verdict,
                             "missing": {"native_run": [], "other_run": [], "verdict": missing_verdict or []}}},
            STRAY: {"model": STRAY, "best_open_harness": {"harness": None, "reason": "no open-harness runs",
                                                          "baseline_batch": base,
                                                          "source": "not fixed (no open-harness runs in the baseline)"},
                    "sealed": sealed, "outcome": "inconclusive", "reason": "no native or no open-harness runs"},
        },
        "metrics": {f"{M}/craze": {"main requests": 11.0, "cost $": 0.0, "wall s": 123.8},
                    f"{M}/gx": {"main requests": 11.0, "cost $": 0.0, "wall s": 96.6},
                    f"{STRAY}/gx": {"main requests": 19.5}},
        "task_tables": {M: {"T-1": {"gx": "LW"}, "T-2": {"gx": "Wl"}}, STRAY: {}},
        "plan_modes": ["gx: prompt-only", "craze: --plan"],
        "length_bias": {"points": 60, "bins": [{"log_ratio": [float("-inf"), -1.0], "wins": 0, "ties": 0, "losses": 0,
                                                "n": 0, "rate": None, "ci90": [0.0, 1.0]}],
                        "longer_side": _wr(13, 7, 16, 0.4583), "correlation": -0.0306},
        "losses": [{"task": "T-1", "model": M, "orders": [{"reasons": f"{LOSS_REASON} at {final}/runs/x"}]}],
        "rubrics": {"T-1": ["item one"]},
    }
    if sealed:
        for k in ("all_tasks", "heldout"):
            del rep["success_bar"][M][k]
        rep["success_bar"][M].update({"outcome": "provisional-misses", "reason": "a dev condition fails"})
    return rep


def _write(d: Path, rep: dict) -> Path:
    d.mkdir(parents=True)
    (d / "report.json").write_text(json.dumps(rep, indent=2) + "\n")  # -Infinity, as report.write writes it
    return d


@pytest.fixture
def reports(tmp_path) -> tuple[Path, Path]:
    return (_write(tmp_path / "reports" / "report-final-a", _report(tmp_path, "a")),
            _write(tmp_path / "reports" / "report-final-b", _report(tmp_path, "b", missing_verdict=["T-2"])))


def _strings(obj):
    if isinstance(obj, dict):
        for k, v in obj.items():
            yield k
            yield from _strings(v)
    elif isinstance(obj, list):
        for v in obj:
            yield from _strings(v)
    elif isinstance(obj, str):
        yield obj


def test_reports_become_the_slim_structure(reports, tmp_path):
    out = tmp_path / "results" / "camp-x" / "summary.json"
    s = sm.summarize(list(reports), out, "camp-x")
    doc = json.loads(out.read_text())
    assert s["bytes"] == len(out.read_bytes()) and s["groups"][0]["outcomes"] == {M: "misses"}
    assert (doc["schema"], doc["campaign"]) == (1, "camp-x")
    assert [g["report"] for g in doc["groups"]] == ["report-final-a", "report-final-b"]  # --report order
    g = doc["groups"][0]
    assert g["models"] == [M] and g["other_models"] == [STRAY] and g["harnesses"] == ["craze", "gx", "opencode"]
    bar = g["bar"][M]
    assert set(g["bar"]) == {M} and bar["outcome"] == "misses" and bar["reason"].startswith("all fifteen")
    assert bar["best_open_harness"] == {"harness": "gx", "reason": "tied counts; gx vs opencode win rate 0.79",
                                        "counts": {"gx": 14.0, "opencode": 14.0}, "baseline_batch": "base-a",
                                        "source": "recorded in base-a/best-open-harness.json"}
    assert set(bar["conditions"]) == {"all_tasks", "heldout", "dev"}
    assert bar["conditions"]["all_tasks"] == {"objective_native": 14.5, "objective_other": 15.0, "objective_ok": False,
                                              "win_ok": True, "win_rate": {"rate": 0.5167, "ci90": [0.31, 0.69],
                                                                           "wtl": [13, 5, 12], "n": 30}}
    assert bar["coverage_complete"] is True and "coverage_gaps" not in bar
    gaps = doc["groups"][1]["bar"][M]
    assert gaps["coverage_complete"] is False and gaps["coverage_gaps"] == {"verdict": ["T-2"]}
    assert set(g["objective"]) == {f"{M}/craze", f"{M}/gx", f"{M}/opencode"}  # the stray model's rows left out
    assert g["objective"][f"{M}/craze"] == {"dev": 7.0, "heldout": 7.5, "all": 14.5,
                                            "tasks": {"T-1": 1.0, "T-2": 0.5, "smoke-fix": 1.0}}
    assert g["statuses"] == {f"{M}/craze": {"ok": 32}, f"{M}/gx": {"ok": 16, "crashed": 1}}
    assert set(g["win_rates"]) == {"dev", "heldout"} and set(g["win_rates"]["dev"]) == {M, "pooled"}
    assert g["win_rates"]["dev"][M]["craze vs gx"] == {"rate": 0.5357, "ci90": [0.31, 0.69], "wtl": [6, 3, 5], "n": 14}
    assert "craze vs opencode" not in g["win_rates"]["dev"][M]  # n=0 cells dropped
    assert set(g["metrics"]) == {f"{M}/craze", f"{M}/gx"} and g["metrics"][f"{M}/gx"]["wall s"] == 96.6
    assert g["task_tables"] == {M: {"T-1": {"gx": "LW"}, "T-2": {"gx": "Wl"}}}
    assert "length_bias" not in g and "plan_modes" not in g and "compare" not in g  # report-wide: out, or provenance
    assert g["provenance"] == {"batches": ["base-a", "gxplan-a", "final-a"], "baseline_batch": "base-a",
                               "judge_hashes": [HASH], "judge_hash_now": HASH, "task_judge_hashes": {"T-1": ["aa" * 32]},
                               "plan_modes": ["craze: --plan", "gx: prompt-only"],
                               "verdicts": {"loaded": 31, "stale": 1, "bound_to_scored_runs": 30, "unbound": 0,
                                            "final": 30},
                               "sealed": False}
    text = out.read_text()
    for gone in (LOSS_REASON, "item one", "verdict_sources", "verdicts.jsonl", "bins", "Infinity", "losses",
                 "rubrics", "longer_side", "correlation", STRAY + "/gx"):
        assert gone not in text, gone


def _pooled(w: int, t: int, lo: int) -> dict:
    return sm.cell(rp.WinRate(wins=w, ties=t, losses=lo).to_dict())


def test_pooled_counts_only_the_covered_models_verdicts(reports, tmp_path):
    """The stray model's gx-vs-opencode verdicts are in the report's pooled matrix; the
    summary's pooled is recomputed from the covered model's cells alone."""
    out = tmp_path / "s.json"
    sm.summarize([reports[0]], out, "camp-x")
    wr = json.loads(out.read_text())["groups"][0]["win_rates"]
    assert STRAY not in wr["dev"] and STRAY not in wr["heldout"]
    assert wr["dev"]["pooled"] == {"craze vs gx": _pooled(6, 3, 5), "gx vs craze": _pooled(5, 3, 6),
                                   "gx vs opencode": _pooled(1, 0, 1), "opencode vs gx": _pooled(1, 0, 1)}
    assert wr["dev"]["pooled"]["gx vs opencode"]["wtl"] == [1, 0, 1]  # the report's pooled says [3, 0, 1]
    assert wr["heldout"]["pooled"] == {"craze vs gx": _pooled(7, 2, 7), "gx vs craze": _pooled(7, 2, 7)}


def test_an_older_report_without_the_newer_fields_summarizes(tmp_path):
    """Plan 029's final reports predate judge_hash_now, task_judge_hashes and the stale count."""
    rep = _report(tmp_path, "old")
    for k in ("judge_hash_now", "task_judge_hashes", "losses", "rubrics"):
        del rep[k]
    del rep["verdicts_used"]["stale"]
    d = _write(tmp_path / "report-old", rep)
    sm.summarize([d], tmp_path / "s.json", "camp-x")
    prov = json.loads((tmp_path / "s.json").read_text())["groups"][0]["provenance"]
    assert prov["judge_hash_now"] is None and prov["task_judge_hashes"] is None and prov["verdicts"]["stale"] is None


def test_the_same_reports_give_the_same_bytes(reports, tmp_path):
    a, b = tmp_path / "one.json", tmp_path / "two.json"
    sm.summarize(list(reports), a, "camp-x")
    # The same report written with its keys in reverse order, and given twice.
    d = reports[0]
    rep = json.loads((d / "report.json").read_text())
    (d / "report.json").write_text(json.dumps(dict(reversed(list(rep.items()))), indent=4))
    sm.summarize([reports[0], reports[0], reports[1]], b, "camp-x")
    assert a.read_bytes() == b.read_bytes()
    assert a.read_text().endswith("}\n") and "  " in a.read_text()  # indented, one final newline


def test_a_sealed_report_is_refused_unless_allowed(tmp_path):
    d = _write(tmp_path / "report-dev", _report(tmp_path, "dev", sealed=True))
    out = tmp_path / "s.json"
    with pytest.raises(sm.SummaryError, match="sealed report"):
        sm.summarize([d], out, "camp-x")
    assert not out.exists()
    sm.summarize([d], out, "camp-x", allow_sealed=True)
    g = json.loads(out.read_text())["groups"][0]
    assert g["provenance"]["sealed"] is True and set(g["bar"][M]["conditions"]) == {"dev"}
    assert g["objective"][f"{M}/craze"]["heldout"] is None and g["objective"][f"{M}/craze"]["all"] is None


def test_no_absolute_path_reaches_the_summary(reports, tmp_path):
    out = tmp_path / "s.json"
    sm.summarize(list(reports), out, "camp-x")
    doc = json.loads(out.read_text())
    for s in _strings(doc):
        assert not s.startswith("/") and str(tmp_path) not in s, s
    assert sm.path_hits(doc) == [] and "/home/" not in out.read_text()


@pytest.mark.parametrize("plant", [
    lambda r: r["success_bar"][M].update(reason="it read /home/someone/.craze/native/providers.toml"),
    lambda r: r["plan_modes"].append("craze: --plan under /tmp/x/run"),  # provenance.plan_modes
    lambda r: r["task_tables"][M].update({"/sandbox/work/T-9": {"gx": "W"}}),
])
def test_a_path_the_summary_would_carry_fails_and_writes_nothing(tmp_path, plant):
    rep = _report(tmp_path, "p")
    plant(rep)
    d = _write(tmp_path / "report-p", rep)
    out = tmp_path / "s.json"
    with pytest.raises(sm.SummaryError, match="nothing written"):
        sm.summarize([d], out, "camp-x")
    assert not out.exists() and not list(tmp_path.glob(".s.json.tmp-*"))


def test_the_owners_home_is_scanned_for_wherever_it_is(tmp_path):
    rep = _report(tmp_path, "h")
    rep["success_bar"][M]["reason"] = "see /data/users/bob/notes"
    d = _write(tmp_path / "report-h", rep)
    with pytest.raises(sm.SummaryError, match="absolute path"):
        sm.summarize([d], tmp_path / "s.json", "camp-x", home="/data/users/bob")


def test_refusals(reports, tmp_path):
    with pytest.raises(sm.SummaryError, match="not a readable"):
        sm.summarize([tmp_path / "nowhere"], tmp_path / "s.json", "camp-x")
    other = tmp_path / "elsewhere" / "report-final-a"
    _write(other, _report(tmp_path, "c"))
    with pytest.raises(sm.SummaryError, match="two reports named"):
        sm.summarize([reports[0], other], tmp_path / "s.json", "camp-x")
    with pytest.raises(sm.SummaryError, match="a directory"):
        sm.summarize(list(reports), tmp_path, "camp-x")
    with pytest.raises(sm.SummaryError, match="one of the input reports"):
        sm.summarize(list(reports), reports[0] / "report.json", "camp-x")
    (tmp_path / "not-a-report").mkdir()
    (tmp_path / "not-a-report" / "report.json").write_text("{}")
    with pytest.raises(sm.SummaryError, match="not a crazeeval report"):
        sm.summarize([tmp_path / "not-a-report"], tmp_path / "s.json", "camp-x")


def test_a_report_the_report_builder_writes_summarizes(tmp_path):
    """The live ``report.build``/``report.write`` output, not a hand-made one -- with a
    stray model (runs outside the baseline, no craze) whose gx-vs-opencode verdicts the
    report pools: the summary's pooled leaves them out, and where the stray has none it
    equals the report's own pooled cell exactly."""
    tdir = tmp_path / "tasks"
    splits = {"D1": "dev", "H1": "heldout"}
    tasks = {}
    for t, s in splits.items():
        (tdir / t).mkdir(parents=True)
        tasks[t] = Task(id=t, dir=tdir / t, category="explain", split=s, mode="build", prompt="Explain.",
                        repo_kind="fixture", fixture="x", setup_patch=None, timeout_s=1, rubric=["r"], checks=[],
                        validate={}, false_claims=["f"])
    base = tmp_path / "camp" / "eval-runs" / "base-y"
    base.mkdir(parents=True)

    def run(h, t, m=M):
        return {"harness": h, "model": m, "task": t, "rep": 1, "status": "ok", "objective_pass": True,
                "split": splits[t], "run_id": f"base-y:{h}/{m}/{t}/rep1:a1", "run_key": f"{h}/{m}/{t}/rep1",
                "answer_words": 10, "metrics": {"main_requests": 3, "tool_calls": 4, "cost": 0.01}, "wall_s": 5.0,
                "batch": str(base)}

    def verdict(t, hx, hy, winner, m=M):
        return {"task": t, "model": m, "split": splits[t], "hx": hx, "hy": hy, "both_orders": True,
                "judge_model": "sol", "x_run_id": f"base-y:{hx}/{m}/{t}/rep1:a1",
                "y_run_id": f"base-y:{hy}/{m}/{t}/rep1:a1", "result": {"winner": winner, "confidence": "high"}}

    covered_runs = [run(h, t) for h in ("craze", "gx", "opencode") for t in splits]
    stray_runs = [run(h, t, STRAY) for h in ("gx", "opencode") for t in splits]
    verdicts = [verdict(t, "craze", "gx", "x") for t in splits] + [
        verdict("D1", "gx", "opencode", "x"),  # the covered model's
        verdict("D1", "gx", "opencode", "y", STRAY), verdict("H1", "gx", "opencode", "tie", STRAY)]
    rep = rp.build(rp.ReportInput(runs=covered_runs + stray_runs, verdicts=verdicts, tasks=tasks, unseal=True,
                                  batches=[str(base)], verdict_sources=[str(base / "judging" / "verdicts.jsonl")],
                                  baseline_batch=base, baseline_runs=covered_runs, judge_hashes=[HASH],
                                  judge_hash_now=HASH))
    rp.write(rep, tmp_path / "report-live")
    assert rep["win_rates"]["dev|pooled"]["gx vs opencode"]["n"] == 2  # the report pools the stray's verdict
    out = tmp_path / "s.json"
    sm.summarize([tmp_path / "report-live"], out, "camp-y")
    g = json.loads(out.read_text())["groups"][0]
    assert g["models"] == [M] and g["other_models"] == [STRAY]
    assert g["bar"][M]["best_open_harness"]["harness"] == "gx"
    assert g["bar"][M]["best_open_harness"]["source"] == "fixed now and recorded in base-y/best-open-harness.json"
    assert g["bar"][M]["conditions"]["heldout"]["win_rate"]["wtl"] == [1, 0, 0]
    pooled = g["win_rates"]["dev"]["pooled"]
    assert pooled["gx vs opencode"] == _pooled(1, 0, 0) and pooled["opencode vs gx"] == _pooled(0, 0, 1)
    for split in ("dev", "heldout"):  # the stray judged no craze pair: those cells are the report's own
        for pair in ("craze vs gx", "gx vs craze"):
            assert g["win_rates"][split]["pooled"][pair] == sm.cell(rep["win_rates"][f"{split}|pooled"][pair])
    assert "gx vs opencode" not in g["win_rates"]["heldout"]["pooled"]  # only the stray's there
    assert g["provenance"]["batches"] == ["base-y"] and sm.path_hits(g) == []


def test_the_cli_prints_the_size_and_defaults_the_campaign_name(reports, tmp_path, monkeypatch, capsys):
    monkeypatch.setenv(paths.CAMPAIGN_ENV, str(tmp_path / "camp-z"))
    out = tmp_path / "s.json"
    argv = ["summarize", "--report", str(reports[0]), "--report", str(reports[1]), "--out", str(out)]
    assert main(argv) == 0
    printed = capsys.readouterr().out
    assert f"({len(out.read_bytes())} bytes; campaign camp-z, 2 group(s))" in printed
    assert f"report-final-a: {M} misses" in printed
    assert json.loads(out.read_text())["campaign"] == "camp-z"
    assert main(argv + ["--campaign-name", "2026-09-plan029"]) == 0
    assert json.loads(out.read_text())["campaign"] == "2026-09-plan029"
    assert main(argv + ["--campaign-name", "a/b"]) == 2
    sealed = _write(tmp_path / "report-dev", _report(tmp_path, "dev", sealed=True))
    assert main(["summarize", "--report", str(sealed), "--out", str(tmp_path / "t.json")]) == 2
    assert "--allow-sealed" in capsys.readouterr().err and not (tmp_path / "t.json").exists()
    assert main(["summarize", "--report", str(sealed), "--out", str(tmp_path / "t.json"), "--allow-sealed"]) == 0
