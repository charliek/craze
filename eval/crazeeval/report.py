"""``crazeeval report``: objective counts, win rates, the success bar (plan 029 §3.1.8).

Reads one or more batch directories (their runs' ``result.json``) and the judge's
verdict files, and writes a Markdown report and its JSON:

- objective pass counts per model x harness (dev, held-out, all fifteen);
- the pairwise win-rate matrix with Wilson 90% intervals (ties count half);
- the success bar per model: craze native against the best open harness (gx or
  opencode), fixed from the baseline batch and recorded there -- meets, misses (with
  the gap) or inconclusive (incomplete coverage);
- metric medians (requests, tool calls, tokens, cost, wall time, answer words);
- verdicts against the log length ratio of the two answers (length bias);
- task-level verdict tables;
- ``--compare``: a craze build against an earlier one (a lever's before/after,
  pooled and per model);
- ``--losses``: "Where craze lost" -- every final verdict a craze run lost, with the
  rubric items it did not meet against the other side's and the judge's reasons.

**Only verdicts that still stand count** (``current_verdicts``, plan 029 W2): a record's
``task_judge_hash`` must equal its task's current one -- or, for a record from before
per-task hashes, its global ``judge_hash`` today's. The report prints the hashes it used.

**Held-out tasks stay sealed** unless ``--unseal``: nothing under a batch's
``heldout/`` (runs or verdicts) is read, and the success bar is evaluated on the dev
tasks alone and labelled provisional. The orchestrator unseals only for the final.

**Verdicts are bound to runs** (review r1-c2 §8): a verdict counts only for the exact
pair of runs it judged (their ``run_id``s: batch, run key and attempt), and only when
both are among the scored runs the report selected -- a verdict on a replaced or
unscored run counts for nothing. A pair judged more than once counts once, by its final
verdict: astra's both-order re-judgement over sol's both-order verdict over a single
order (then the later record).
"""

from __future__ import annotations

import csv
import io
import json
import math
import statistics
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterator

from crazeeval.judge import judge_hash, task_judge_hashes, verdict_current
from crazeeval.tasks import Task

Z90 = 1.6448536269514722
OPEN_HARNESSES = ("gx", "opencode")
NATIVE = "craze"
# Runs that are not scored: infrastructure failures, and runs invalidated by a
# contamination hit or a key exposure (§3.1.5). Timeouts, crashes and budget-capped runs
# are scored failures, shown to the judge as such.
UNSCORED = {"infra", "launch-error", "budget-stop", "contaminated", "key-exposure", None}
VERDICTS_FILE = "verdicts.jsonl"
BEST_OPEN_FILE = "best-open-harness.json"
KEEP, DROP, INCONCLUSIVE = "keep", "drop", "inconclusive"


# -- statistics ------------------------------------------------------------------------------------


def wilson(successes: float, n: int, z: float = Z90) -> tuple[float, float]:
    """The Wilson score interval for a proportion (``successes`` may be fractional:
    ties count half). (0, 1) for n = 0."""
    if n <= 0:
        return 0.0, 1.0
    p = successes / n
    denom = 1 + z * z / n
    centre = (p + z * z / (2 * n)) / denom
    half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / denom
    return max(0.0, centre - half), min(1.0, centre + half)


@dataclass
class WinRate:
    wins: int = 0
    ties: int = 0
    losses: int = 0

    @property
    def n(self) -> int:
        return self.wins + self.ties + self.losses

    @property
    def score(self) -> float:
        return self.wins + 0.5 * self.ties

    @property
    def rate(self) -> float | None:
        return self.score / self.n if self.n else None

    @property
    def ci(self) -> tuple[float, float]:
        return wilson(self.score, self.n)

    def add(self, outcome: float) -> None:
        if outcome == 1:
            self.wins += 1
        elif outcome == 0:
            self.losses += 1
        else:
            self.ties += 1

    def to_dict(self) -> dict:
        lo, hi = self.ci
        return {"wins": self.wins, "ties": self.ties, "losses": self.losses, "n": self.n,
                "rate": None if self.rate is None else round(self.rate, 4), "ci90": [round(lo, 4), round(hi, 4)]}

    def fmt(self) -> str:
        if not self.n:
            return "–"
        lo, hi = self.ci
        return f"{self.rate:.0%} [{lo:.0%}–{hi:.0%}] n={self.n}"


# -- loading ------------------------------------------------------------------------------------------


def iter_results(batch: Path, heldout: bool) -> Iterator[tuple[Path, Path, dict]]:
    """(split root, rep directory, parsed ``result.json``) for every rep under a batch's
    ``runs/`` -- and ``heldout/`` only when ``heldout`` is set. An unreadable result is
    skipped. The rep directory sits at ``<root>/<harness>/<model slug>/<task>/rep<n>``."""
    for part in ("runs", "heldout") if heldout else ("runs",):
        root = Path(batch) / part
        if not root.is_dir():
            continue
        for p in sorted(root.glob("*/*/*/rep*/result.json")):
            try:
                r = json.loads(p.read_text())
            except (OSError, ValueError):
                continue
            yield root, p.parent, r


def read_jsonl(p: Path) -> Iterator[dict]:
    """The records of a JSONL file, skipping a line that does not parse (none for a
    missing file)."""
    try:
        lines = Path(p).read_text().splitlines()
    except OSError:
        return
    for line in lines:
        try:
            yield json.loads(line)
        except ValueError:
            continue


def load_runs(batches: list[Path], unseal: bool = False) -> list[dict]:
    """Every run's final result (rep-level ``result.json``) under each batch's
    ``runs/`` -- and ``heldout/`` only when unsealed. A later batch's run with the same
    key replaces an earlier one's (references reused from a baseline, §3.1.8)."""
    by_key: dict[tuple, dict] = {}
    for b in batches:
        for root, rep_dir, r in iter_results(b, unseal):
            r.setdefault("rep_dir", str(rep_dir))
            r["batch"] = str(b)
            if "harness" not in r:  # a result written before the attempt ran (budget stop)
                h, _, t, rep = rep_dir.relative_to(root).parts
                r.update({"harness": h, "task": t, "rep": int(rep[3:] or 0)})
            by_key[(r.get("harness"), r.get("model") or rep_dir.parent.parent.name, r.get("task"), r.get("rep"))] = r
    return list(by_key.values())


def load_all_runs(batches: list[Path], unseal: bool = False) -> list[dict]:
    """Every scored-or-not run's final result under each batch's ``runs/`` -- and
    ``heldout/`` only when unsealed -- keyed by its ``run_id`` (which already carries the
    batch's name), so no batch replaces another's run that happens to share a run key
    (``crazeeval archive --all-runs``, a whole campaign kept whole). A batch passed more
    than once is read once: batches are deduplicated by their resolved directory."""
    seen_batches: set[str] = set()
    by_id: dict[str, dict] = {}
    order: list[str] = []
    for b in batches:
        rb = str(Path(b).resolve())
        if rb in seen_batches:
            continue
        seen_batches.add(rb)
        for root, rep_dir, r in iter_results(b, unseal):
            r.setdefault("rep_dir", str(rep_dir))
            r["batch"] = str(b)
            if "harness" not in r:  # a result written before the attempt ran (budget stop)
                h, _, t, rep = rep_dir.relative_to(root).parts
                r.update({"harness": h, "task": t, "rep": int(rep[3:] or 0)})
            rid = r.get("run_id")
            if rid not in by_id:
                order.append(rid)
                by_id[rid] = r
    return [by_id[rid] for rid in order]


def verdict_files(paths_: list[Path], unseal: bool = False) -> list[Path]:
    out = []
    for p in paths_:
        p = Path(p)
        if p.is_file():
            if "heldout" in p.parts and not unseal:
                continue
            out.append(p)
            continue
        for cand in [p / VERDICTS_FILE] + ([p / "heldout" / VERDICTS_FILE] if unseal else []):
            if cand.is_file():
                out.append(cand)
    return out


def read_verdicts(paths_: list[Path], unseal: bool = False) -> list[dict]:
    """Every pair record of the verdict files, in file order (held-out ones only when
    unsealed)."""
    return [v for f in verdict_files(paths_, unseal) for v in read_jsonl(f)
            if unseal or v.get("split") != "heldout"]


def dedupe_verdicts(records: list[dict]) -> list[dict]:
    """A later record for the same pair, judge and mode replaces an earlier one."""
    by: dict[tuple, dict] = {}
    for v in records:
        by[(v.get("pair"), v.get("judge_model"), v.get("both_orders"))] = v
    return list(by.values())


def load_verdicts(paths_: list[Path], unseal: bool = False) -> list[dict]:
    """Pair records (one line per judged pair). A later line for the same pair, judge
    and mode replaces an earlier one."""
    return dedupe_verdicts(read_verdicts(paths_, unseal))


def load_current_verdicts(paths_: list[Path], tasks: dict[str, Task], unseal: bool = False
                          ) -> tuple[list[dict], list[dict]]:
    """The records that still stand (current_verdicts), deduplicated as load_verdicts
    does -- the judge-hash test first, so a newer record made under a since-reverted
    rubric never hides an older one that stands -- and the stale records set aside."""
    current, stale = current_verdicts(read_verdicts(paths_, unseal), tasks)
    return dedupe_verdicts(current), stale


def current_verdicts(verdicts: list[dict], tasks: dict[str, Task]) -> tuple[list[dict], list[dict]]:
    """The verdict records that still stand under today's judge, and the stale rest
    (plan 029 W2): a record stands when its ``task_judge_hash`` equals its task's current
    one -- or, for an older record without one, when its global ``judge_hash`` equals
    today's. So adding or changing one task never invalidates another task's verdicts;
    a record from before per-task hashes stands only while nothing changed."""
    th, jh = task_judge_hashes(tasks), judge_hash(tasks)
    current, stale = [], []
    for v in verdicts:
        (current if verdict_current(v, th, jh) else stale).append(v)
    return current, stale


def run_id(r: dict) -> str | None:
    return r.get("run_id")


def bind_verdicts(verdicts: list[dict], runs: list[dict], runs_y: list[dict] | None = None) -> tuple[list[dict], int]:
    """The verdicts whose two judged runs are exactly scored runs of the report: x among
    ``runs`` and y among ``runs_y`` (default: ``runs`` too). Returns them and how many
    were dropped (stale, unscored, or from an older record without run ids)."""
    xs = {run_id(r) for r in runs if scored(r) and run_id(r)}
    ys = xs if runs_y is None else {run_id(r) for r in runs_y if scored(r) and run_id(r)}
    bound = [v for v in verdicts if v.get("x_run_id") in xs and v.get("y_run_id") in ys]
    return bound, len(verdicts) - len(bound)


def verdict_rank(v: dict) -> int:
    """Which of several verdicts on one pair is final: astra both-order (the success-bar
    escalation) > sol both-order > another both-order judge > a single order."""
    both, jm = bool(v.get("both_orders")), v.get("judge_model")
    if both:
        return {"astra": 4, "sol": 3}.get(jm, 2)
    return 1 if jm == "sol" else 0


def pair_key(v: dict) -> tuple[str, ...]:
    """The judged pair of runs a verdict is about, in either orientation."""
    return tuple(sorted((str(v.get("x_run_id") or v.get("x")), str(v.get("y_run_id") or v.get("y")))))


def final_verdicts(verdicts: list[dict]) -> list[dict]:
    """One verdict per judged pair of runs: the highest-ranked, the later on a tie. A
    final verdict that failed stays a missing verdict (it is re-judged, never replaced by
    a lesser one)."""
    best: dict[tuple, tuple[int, int, dict]] = {}
    for i, v in enumerate(verdicts):
        key = pair_key(v)
        cand = (verdict_rank(v), i, v)
        if key not in best or cand[:2] > best[key][:2]:
            best[key] = cand
    return [v for _, _, v in sorted(best.values(), key=lambda t: t[1])]


# -- objective counts -----------------------------------------------------------------------------------


def scored(r: dict) -> bool:
    return r.get("status") not in UNSCORED


def pass_by_task(runs: list[dict]) -> dict[tuple[str, str], dict[str, float]]:
    """(model, harness) -> task -> the mean objective pass over its scored reps."""
    acc: dict[tuple[str, str], dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    for r in runs:
        if not scored(r):
            continue
        acc[(r.get("model"), r.get("harness"))][r.get("task")].append(1.0 if r.get("objective_pass") else 0.0)
    return {k: {t: sum(v) / len(v) for t, v in tasks.items()} for k, tasks in acc.items()}


def objective_counts(runs: list[dict], splits: dict[str, str]) -> dict[tuple[str, str], dict]:
    """(model, harness) -> counts on dev, held-out and all tasks (a task's value is its
    reps' mean pass, so two reps give halves)."""
    out = {}
    for key, tasks in pass_by_task(runs).items():
        c = {"dev": 0.0, "heldout": 0.0, "all": 0.0, "tasks": dict(sorted(tasks.items()))}
        for t, v in tasks.items():
            sp = splits.get(t)
            if sp in ("dev", "heldout"):
                c[sp] += v
                c["all"] += v
        out[key] = c
    return out


# -- verdicts -----------------------------------------------------------------------------------------------


def _outcome(winner: str | None, side: str) -> float:
    """1 / 0.5 / 0 for ``side`` ("x" or "y") given a pair's winner (a tie counts half)."""
    return 0.5 if winner == "tie" else (1.0 if winner == side else 0.0)


def outcome_for(v: dict, harness: str) -> float | None:
    """1 / 0.5 / 0 from ``harness``'s side of a pair record (None: no verdict)."""
    res = v.get("result")
    if not res:
        return None
    side = "x" if v.get("hx") == harness else ("y" if v.get("hy") == harness else None)
    if side is None:
        return None
    return _outcome(res.get("winner"), side)


def win_matrix(verdicts: list[dict], splits: dict[str, str], split: str | None = None,
               model: str | None = None, both_orders_only: bool = False) -> dict[tuple[str, str], WinRate]:
    """(harness, other) -> WinRate of harness against other."""
    m: dict[tuple[str, str], WinRate] = defaultdict(WinRate)
    for v in verdicts:
        if model is not None and v.get("model") != model:
            continue
        if split is not None and splits.get(v.get("task")) != split:
            continue
        if both_orders_only and not v.get("both_orders"):
            continue
        hx, hy = v.get("hx"), v.get("hy")
        if not hx or not hy or hx == hy:
            continue
        o = outcome_for(v, hx)
        if o is None:
            continue
        m[(hx, hy)].add(o)
        m[(hy, hx)].add(1 - o)
    return dict(m)


# -- the success bar ------------------------------------------------------------------------------------------


def best_open_harness(counts: dict, verdicts: list[dict], model: str, key: str = "all") -> dict:
    """The open harness with the higher objective count on ``key`` (§3.1.8); tiebreak:
    its win rate in gx-vs-opencode pairs (then its name)."""
    have = {h: counts[(model, h)][key] for h in OPEN_HARNESSES if (model, h) in counts}
    if not have:
        return {"harness": None, "reason": "no open-harness runs"}
    if len(have) == 1:
        h = next(iter(have))
        return {"harness": h, "reason": "the only open harness with runs", "counts": have}
    a, b = OPEN_HARNESSES
    if have[a] != have[b]:
        h = a if have[a] > have[b] else b
        return {"harness": h, "reason": "higher objective pass count", "counts": have}
    wr = win_matrix(verdicts, {}, model=model).get((a, b))
    if wr is not None and wr.rate is not None and wr.rate != 0.5:
        h = a if wr.rate > 0.5 else b
        return {"harness": h, "reason": f"tied counts; {a} vs {b} win rate {wr.rate:.2f}", "counts": have}
    return {"harness": a, "reason": "tied counts and no deciding gx-vs-opencode verdicts; by name", "counts": have}


def fixed_best_open_harness(baseline_batch: Path | None, baseline_runs: list[dict], verdicts: list[dict],
                            splits: dict[str, str], models: list[str], unsealed: bool) -> dict[str, dict]:
    """The best open harness per model, from the **baseline** batch alone (§3.1.8) --
    never from a later rerun. Unsealed, it is chosen on all fifteen tasks and recorded in
    ``<baseline>/best-open-harness.json`` the first time; after that the record stands.
    Sealed, it is a provisional choice on the dev tasks, not recorded."""
    base_verdicts, _ = bind_verdicts(verdicts, baseline_runs)
    base_verdicts = final_verdicts(base_verdicts)
    counts = objective_counts(baseline_runs, splits)
    record_path = Path(baseline_batch) / BEST_OPEN_FILE if baseline_batch else None
    recorded: dict = {}
    if unsealed and record_path is not None and record_path.is_file():
        try:
            recorded = json.loads(record_path.read_text()).get("models") or {}
        except (OSError, ValueError):
            recorded = {}
    out, new = {}, {}
    for m in models:
        if m in recorded:
            out[m] = {**recorded[m], "source": f"recorded in {record_path}"}
            continue
        best = best_open_harness(counts, base_verdicts, m, "all" if unsealed else "dev")
        best["baseline_batch"] = str(baseline_batch) if baseline_batch else None
        if unsealed and best.get("harness"):
            new[m] = best
            out[m] = {**best, "source": f"fixed now and recorded in {record_path}"}
        else:
            out[m] = {**best, "source": "provisional (dev tasks of the baseline; held-out sealed)" if not unsealed
                      else "not fixed (no open-harness runs in the baseline)"}
    if new and record_path is not None:
        doc = {"baseline_batch": str(baseline_batch), "models": {**recorded, **new}}
        tmp = record_path.with_name(record_path.name + ".tmp")
        tmp.write_text(json.dumps(doc, indent=2) + "\n")
        tmp.replace(record_path)
    return out


def coverage(runs: list[dict], verdicts: list[dict], model: str, native: str, other: str, tasks: list[str]) -> dict:
    """Every task needs a scored run on both sides and a verdict between them (the
    verdicts are already bound to the scored runs)."""
    scored_by = defaultdict(set)
    for r in runs:
        if r.get("model") == model and scored(r):
            scored_by[r.get("harness")].add(r.get("task"))
    judged = set()
    for v in verdicts:
        if v.get("model") == model and {v.get("hx"), v.get("hy")} == {native, other} and v.get("result"):
            judged.add(v.get("task"))
    missing = {
        "native_run": sorted(t for t in tasks if t not in scored_by[native]),
        "other_run": sorted(t for t in tasks if t not in scored_by[other]),
        "verdict": sorted(t for t in tasks if t not in judged),
    }
    return {"complete": not any(missing.values()), "missing": missing}


def _bar_conditions(counts, verdicts, splits, model, other, key, split):
    n_obj = counts.get((model, NATIVE), {}).get(key, 0.0)
    o_obj = counts.get((model, other), {}).get(key, 0.0)
    wr = win_matrix(verdicts, splits, split=split, model=model).get((NATIVE, other), WinRate())
    return {
        "objective_native": n_obj,
        "objective_other": o_obj,
        "objective_ok": n_obj >= o_obj,
        "win_rate": wr.to_dict(),
        "win_ok": wr.rate is not None and wr.rate >= 0.5,
    }


def success_bar(runs: list[dict], verdicts: list[dict], splits: dict[str, str], model: str,
                unsealed: bool, counts: dict | None = None, best: dict | None = None) -> dict:
    """§3.1.8 for one model. With the held-out tasks sealed, only the dev conditions
    can be evaluated and the result is provisional. ``counts``: objective_counts(runs,
    splits), when already computed. ``best``: the best open harness fixed from the
    baseline (fixed_best_open_harness); without it, it is chosen from ``runs``."""
    counts = objective_counts(runs, splits) if counts is None else counts
    key = "all" if unsealed else "dev"
    if best is None:
        best = best_open_harness(counts, verdicts, model, key)
    other = best.get("harness")
    out: dict = {"model": model, "best_open_harness": best, "sealed": not unsealed}
    if other is None or (model, NATIVE) not in counts:
        out.update({"outcome": "inconclusive", "reason": "no native or no open-harness runs"})
        return out
    tasks_all = sorted(t for t, s in splits.items() if s in ("dev", "heldout"))
    tasks_dev = sorted(t for t, s in splits.items() if s == "dev")
    if unsealed:
        cond_all = _bar_conditions(counts, verdicts, splits, model, other, "all", None)
        cond_ho = _bar_conditions(counts, verdicts, splits, model, other, "heldout", "heldout")
        cov = coverage(runs, verdicts, model, NATIVE, other, tasks_all)
        out.update({"all_tasks": cond_all, "heldout": cond_ho,
                    "dev": _bar_conditions(counts, verdicts, splits, model, other, "dev", "dev"), "coverage": cov})
        if not cov["complete"]:
            out.update({"outcome": "inconclusive", "reason": "coverage incomplete"})
        elif all(c["objective_ok"] and c["win_ok"] for c in (cond_all, cond_ho)):
            out.update({"outcome": "meets", "reason": "(a)-(d) hold"})
        else:
            gaps = []
            for name, c in (("all fifteen", cond_all), ("held-out", cond_ho)):
                if not c["objective_ok"]:
                    gaps.append(f"{name}: objective {c['objective_native']:g} < {c['objective_other']:g}")
                if not c["win_ok"]:
                    gaps.append(f"{name}: win rate {c['win_rate']['rate']} < 0.5")
            out.update({"outcome": "misses", "reason": "; ".join(gaps)})
        return out
    cond_dev = _bar_conditions(counts, verdicts, splits, model, other, "dev", "dev")
    cov = coverage(runs, verdicts, model, NATIVE, other, tasks_dev)
    out.update({"dev": cond_dev, "coverage": cov})
    if not cov["complete"]:
        out.update({"outcome": "inconclusive", "reason": "coverage incomplete (dev)"})
    elif cond_dev["objective_ok"] and cond_dev["win_ok"]:
        out.update({"outcome": "provisional-meets", "reason": "dev conditions hold; held-out sealed"})
    else:
        out.update({"outcome": "provisional-misses", "reason": "a dev condition fails; held-out sealed"})
    return out


# -- metrics, length bias, task tables ---------------------------------------------------------------------


METRICS = {
    "main requests": lambda r: (r.get("metrics") or {}).get("main_requests"),
    "tool calls": lambda r: (r.get("metrics") or {}).get("tool_calls"),
    "input tokens": lambda r: ((r.get("metrics") or {}).get("tokens") or {}).get("input"),
    "output tokens": lambda r: ((r.get("metrics") or {}).get("tokens") or {}).get("output"),
    "cost $": lambda r: (r.get("metrics") or {}).get("cost"),
    "wall s": lambda r: r.get("wall_s"),
    "answer words": lambda r: r.get("answer_words"),
}


def metric_medians(runs: list[dict]) -> dict[tuple[str, str], dict]:
    acc: dict[tuple[str, str], dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    for r in runs:
        if not scored(r):
            continue
        for name, f in METRICS.items():
            v = f(r)
            if isinstance(v, (int, float)):
                acc[(r.get("model"), r.get("harness"))][name].append(float(v))
    return {k: {n: round(statistics.median(vs), 4) for n, vs in d.items()} for k, d in acc.items()}


def _words_by_run(runs: list[dict]) -> dict[str, int]:
    return {run_id(r): int(r.get("answer_words") or 0) for r in runs if run_id(r)}


LENGTH_BINS = [(-math.inf, -1.0), (-1.0, -0.3), (-0.3, 0.3), (0.3, 1.0), (1.0, math.inf)]


def length_bias(verdicts: list[dict], runs: list[dict]) -> dict:
    """Each verdict against log(words_x / words_y): the x side's win rate per bin of the
    log length ratio, the longer side's overall win rate, and the correlation."""
    words = _words_by_run(runs)
    points = []
    for v in verdicts:
        res = v.get("result")
        if not res:
            continue
        wx, wy = words.get(v.get("x_run_id")), words.get(v.get("y_run_id"))
        if wx is None or wy is None:
            continue
        points.append((math.log((wx + 1) / (wy + 1)), _outcome(res["winner"], "x"), v))
    bins = []
    for lo, hi in LENGTH_BINS:
        sel = [o for lr, o, _ in points if lo <= lr < hi]
        wr = WinRate()
        for o in sel:
            wr.add(o)
        bins.append({"log_ratio": [lo, hi], **wr.to_dict()})
    longer = WinRate()
    for lr, o, _ in points:
        if abs(lr) >= 0.1:
            longer.add(o if lr > 0 else 1 - o)
    corr = None
    if len(points) >= 3:
        xs, ys = [p[0] for p in points], [p[1] for p in points]
        try:
            corr = round(statistics.correlation(xs, ys), 4)
        except statistics.StatisticsError:
            corr = None
    return {"points": len(points), "bins": bins, "longer_side": longer.to_dict(), "correlation": corr,
            "rows": [{"pair": v.get("pair"), "log_ratio": round(lr, 4), "x_outcome": o} for lr, o, v in points]}


def _not_met(grades: list | None) -> list[dict]:
    """The rubric items a side did not meet (``missed`` or ``false``), in item order."""
    out = [{"item": g.get("item"), "grade": g.get("grade")} for g in grades or []
           if isinstance(g, dict) and g.get("grade") != "met"]
    return sorted(out, key=lambda g: (g["item"] is None, g["item"] or 0))


def craze_losses(verdicts: list[dict], native: str = NATIVE) -> list[dict]:
    """Every final verdict in which a craze run lost to another harness's run, craze on
    either side (plan 029 W2, the tuning signal behind L7): the task, model, split, the
    craze run, the scores, and per judged order the rubric items each side did not meet
    and the judge's reasons (``orders[i].verdict``, already mapped to x/y)."""
    out = []
    for v in verdicts:
        res = v.get("result")
        hx, hy = v.get("hx"), v.get("hy")
        if not res or (hx == native) == (hy == native):
            continue
        me, them = ("x", "y") if hx == native else ("y", "x")
        if res.get("winner") != them:
            continue
        orders = []
        for o in v.get("orders") or []:
            ov = o.get("verdict")
            if not isinstance(ov, dict):
                continue
            w = ov.get("winner")
            orders.append({
                "judge_model": o.get("judge_model"),
                "craze_shown_as": None if o.get("x_is_a") is None else ("A" if (o["x_is_a"] == (me == "x")) else "B"),
                "winner": "tie" if w == "tie" else ("craze" if w == me else "other"),
                "confidence": ov.get("confidence"),
                "craze_score": ov.get(f"score_{me}"),
                "other_score": ov.get(f"score_{them}"),
                "craze_not_met": _not_met(ov.get(f"rubric_{me}")),
                "other_not_met": _not_met(ov.get(f"rubric_{them}")),
                "reasons": ov.get("reasons"),
            })
        out.append({
            "task": v.get("task"), "model": v.get("model"), "split": v.get("split"),
            "craze_run_key": v.get(me), "craze_run_id": v.get(f"{me}_run_id"),
            "other_harness": hx if me == "y" else hy, "other_run_key": v.get(them),
            "judge_model": v.get("judge_model"), "both_orders": v.get("both_orders"),
            "escalated": bool(v.get("escalated_from")), "confidence": res.get("confidence"),
            "craze_score": res.get(f"score_{me}"), "other_score": res.get(f"score_{them}"),
            "orders": orders,
        })
    return sorted(out, key=lambda r: (str(r["split"]), str(r["task"]), str(r["model"]), str(r["craze_run_key"]),
                                      str(r["other_harness"])))


def task_table(verdicts: list[dict], model: str, native: str = NATIVE) -> dict[str, dict[str, str]]:
    """task -> "<native> vs <other>" -> W/L/T (from native's side; several reps joined)."""
    table: dict[str, dict[str, list[str]]] = defaultdict(lambda: defaultdict(list))
    for v in verdicts:
        if v.get("model") != model or native not in (v.get("hx"), v.get("hy")):
            continue
        other = v.get("hy") if v.get("hx") == native else v.get("hx")
        if other == native:
            continue
        o = outcome_for(v, native)
        mark = "·" if o is None else {1.0: "W", 0.0: "L"}.get(o, "T")
        conf = ((v.get("result") or {}).get("confidence") or "")[:1]
        table[v.get("task")][other].append(mark + (conf if mark != "·" and conf == "l" else ""))
    return {t: {o: "".join(ms) for o, ms in row.items()} for t, row in sorted(table.items())}


# -- compare two craze builds -------------------------------------------------------------------------------------


def compare(new_runs: list[dict], old_runs: list[dict], verdicts: list[dict], splits: dict[str, str],
            native: str = NATIVE) -> dict:
    """A lever's before/after (§3.2): objective counts per model for the two builds and
    the new build's win rate against the old, pooled and per model (dev only unless the
    verdicts include held-out pairs). ``verdicts`` are bound here to the new runs (x)
    and the old runs (y). The keep rule reads ``inconclusive`` until every (task, model)
    that both builds ran has a verdict -- with no verdicts it can never read keep (review
    r1-c2 §8)."""
    new_native = [r for r in new_runs if r.get("harness") == native]
    old_native = [r for r in old_runs if r.get("harness") == native]
    verdicts, unbound = bind_verdicts(verdicts, new_native, old_native)
    verdicts = [v for v in final_verdicts(verdicts) if v.get("hx") == native and v.get("hy") == native]
    new_c = objective_counts(new_native, splits)
    old_c = objective_counts(old_native, splits)
    models = sorted({k[0] for k in new_c} | {k[0] for k in old_c})
    per_model = {}
    pooled = WinRate()
    missing: list[str] = []
    for m in models:
        wr = WinRate()
        judged = set()
        for v in verdicts:
            if v.get("model") != m or not v.get("result"):
                continue
            o = _outcome(v["result"]["winner"], "x")
            wr.add(o)
            pooled.add(o)
            judged.add(v.get("task"))
        n = new_c.get((m, native), {})
        o = old_c.get((m, native), {})
        both_ran = sorted(set(n.get("tasks") or {}) & set(o.get("tasks") or {}))
        missing += [f"{m}/{t}" for t in both_ran if t not in judged]
        per_model[m] = {
            "objective_new": {"dev": n.get("dev"), "heldout": n.get("heldout")},
            "objective_old": {"dev": o.get("dev"), "heldout": o.get("heldout")},
            "objective_drop": (n.get("dev") or 0) < (o.get("dev") or 0),
            "win_rate_new_vs_old": wr.to_dict(),
        }
    drop = any(p["objective_drop"] for p in per_model.values())
    complete = pooled.n > 0 and not missing

    def decide(threshold: float) -> str:
        if drop:
            return DROP
        if not complete:
            return INCONCLUSIVE
        return KEEP if pooled.rate >= threshold else DROP

    return {"pooled": pooled.to_dict(), "per_model": per_model, "unbound_verdicts": unbound,
            "coverage": {"complete": complete, "missing": missing[:50]},
            "keep_rule": {"conditional_keep": decide(0.55), "requested_keep": decide(0.45)}}


# -- the report ---------------------------------------------------------------------------------------------------


@dataclass
class ReportInput:
    runs: list[dict]
    verdicts: list[dict]
    tasks: dict[str, Task]
    unseal: bool
    batches: list[str] = field(default_factory=list)
    verdict_sources: list[str] = field(default_factory=list)
    compare_runs: list[dict] | None = None
    compare_verdicts: list[dict] | None = None
    judge_hashes: list[str] = field(default_factory=list)
    baseline_batch: Path | None = None
    baseline_runs: list[dict] | None = None  # default: ``runs``
    stale_verdicts: int = 0  # records current_verdicts set aside before ``verdicts``
    judge_hash_now: str | None = None
    losses: bool = False  # add "Where craze lost"


def build(inp: ReportInput) -> dict:
    splits = {t.id: t.split for t in inp.tasks.values() if t.split in ("dev", "heldout")}
    counts = objective_counts(inp.runs, splits)
    models = sorted({r.get("model") for r in inp.runs if r.get("model")})
    loaded = len(inp.verdicts) + inp.stale_verdicts
    bound, unbound = bind_verdicts(inp.verdicts, inp.runs)
    verdicts = final_verdicts(bound)
    best = fixed_best_open_harness(inp.baseline_batch, inp.baseline_runs if inp.baseline_runs is not None else inp.runs,
                                   inp.verdicts, splits, models, inp.unseal)
    harnesses = sorted({r.get("harness") for r in inp.runs if r.get("harness")})
    statuses: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))
    for r in inp.runs:
        statuses[f"{r.get('model')}/{r.get('harness')}"][str(r.get("status"))] += 1
    matrices = {}
    for split in ("dev", "heldout") if inp.unseal else ("dev",):
        for m in models + [None]:
            mat = win_matrix(verdicts, splits, split=split, model=m)
            matrices[f"{split}|{m or 'pooled'}"] = {f"{a} vs {b}": w.to_dict() for (a, b), w in sorted(mat.items())}
    out = {
        "sealed": not inp.unseal,
        "batches": inp.batches,
        "verdict_sources": inp.verdict_sources,
        "judge_hashes": sorted(set(inp.judge_hashes)),
        "judge_hash_now": inp.judge_hash_now,
        # The per-task judge hashes the counted verdicts carry (records from before them
        # carry none: they stood on the global hash).
        "task_judge_hashes": {t: sorted(hs) for t, hs in sorted(_task_hashes(verdicts).items())},
        "baseline_batch": str(inp.baseline_batch) if inp.baseline_batch else None,
        "verdicts_used": {"loaded": loaded, "stale": inp.stale_verdicts, "bound_to_scored_runs": len(bound),
                          "unbound": unbound, "final": len(verdicts)},
        "models": models,
        "harnesses": harnesses,
        "objective": {f"{m}/{h}": c for (m, h), c in sorted(counts.items())},
        "statuses": {k: dict(v) for k, v in sorted(statuses.items())},
        "win_rates": matrices,
        "success_bar": {m: success_bar(inp.runs, verdicts, splits, m, inp.unseal, counts, best[m]) for m in models},
        "metrics": {f"{m}/{h}": v for (m, h), v in sorted(metric_medians(inp.runs).items())},
        "length_bias": length_bias(verdicts, inp.runs),
        "task_tables": {m: task_table(verdicts, m) for m in models},
        "plan_modes": sorted({f"{r.get('harness')}: {r.get('plan_mode')}" for r in inp.runs if r.get("plan_mode")}),
    }
    if inp.compare_runs is not None:
        out["compare"] = compare(inp.runs, inp.compare_runs, inp.compare_verdicts or [], splits)
    if inp.losses:
        out["losses"] = craze_losses(verdicts)
        # The rubric text of the tasks with a loss, to name the items craze missed.
        out["rubrics"] = {t: list(inp.tasks[t].rubric) for t in sorted({x["task"] for x in out["losses"]})
                          if t in inp.tasks}
    return out


def _task_hashes(verdicts: list[dict]) -> dict[str, set[str]]:
    out: dict[str, set[str]] = defaultdict(set)
    for v in verdicts:
        if v.get("task_judge_hash"):
            out[v.get("task")].add(v["task_judge_hash"])
    return out


def _fmt(v) -> str:
    if v is None:
        return "–"
    if isinstance(v, float):
        return f"{v:g}"
    return str(v)


def render_markdown(rep: dict) -> str:
    L: list[str] = ["# crazeeval report", ""]
    L.append("Held-out tasks: **sealed** (dev only; run with `--unseal` for the final)." if rep["sealed"]
             else "Held-out tasks: **unsealed**.")
    vu = rep.get("verdicts_used") or {}
    L += [f"Batches: {', '.join(rep['batches'])}", f"Baseline (fixes the best open harness): {rep.get('baseline_batch')}",
          f"Verdicts: {', '.join(rep['verdict_sources']) or 'none'} -- {vu.get('loaded', 0)} loaded, "
          f"{vu.get('stale', 0)} set aside (judged under another judge hash), "
          f"{vu.get('bound_to_scored_runs', 0)} bound to the scored runs, {vu.get('final', 0)} final pairs "
          f"({vu.get('unbound', 0)} unbound: on replaced or unscored runs)",
          f"Judge hash(es): {', '.join(rep['judge_hashes']) or '–'}"
          + (f" (today's: {rep['judge_hash_now']})" if rep.get("judge_hash_now") else ""),
          f"Per-task judge hashes: {_task_hash_line(rep.get('task_judge_hashes') or {})}", ""]
    L += ["## Objective pass counts", "", "| model | harness | dev | held-out | all |", "|---|---|---|---|---|"]
    for k, c in rep["objective"].items():
        m, h = k.rsplit("/", 1)  # model keys may hold a "/" (fireworks/kimi-k3); harness names never do
        L.append(f"| {m} | {h} | {c['dev']:g} | {'sealed' if rep['sealed'] else format(c['heldout'], 'g')} | "
                 f"{'–' if rep['sealed'] else format(c['all'], 'g')} |")
    L += ["", "## Success bar (§3.1.8)", ""]
    for m, sb in rep["success_bar"].items():
        best = sb.get("best_open_harness") or {}
        L.append(f"- **{m}**: {sb['outcome']} — {sb.get('reason')} (best open harness: {best.get('harness')}, "
                 f"{best.get('reason')}; {best.get('source')})")
        for part in ("all_tasks", "heldout", "dev"):
            c = sb.get(part)
            if c:
                wr = c["win_rate"]
                L.append(f"  - {part}: objective {c['objective_native']:g} vs {c['objective_other']:g}; "
                         f"win rate {_fmt(wr['rate'])} (90% CI {wr['ci90'][0]:.2f}–{wr['ci90'][1]:.2f}, n={wr['n']})")
        cov = sb.get("coverage")
        if cov and not cov["complete"]:
            L.append(f"  - coverage gaps: {cov['missing']}")
    L += ["", "## Win rates (row vs column; ties half; Wilson 90%)", ""]
    for key, mat in rep["win_rates"].items():
        if not mat:
            continue
        L.append(f"**{key}**")
        L.append("")
        for pair, w in mat.items():
            if w["n"]:
                L.append(f"- {pair}: {w['rate']:.0%} [{w['ci90'][0]:.0%}–{w['ci90'][1]:.0%}] n={w['n']} "
                         f"(W{w['wins']} T{w['ties']} L{w['losses']})")
        L.append("")
    L += ["## Metric medians", "", "| model/harness | " + " | ".join(METRICS) + " |", "|---" * (len(METRICS) + 1) + "|"]
    for k, v in rep["metrics"].items():
        L.append(f"| {k} | " + " | ".join(_fmt(v.get(n)) for n in METRICS) + " |")
    lb = rep["length_bias"]
    L += ["", "## Verdicts vs log length ratio (x's words / y's words)", "",
          f"{lb['points']} verdicts; the longer side's win rate: {_fmt(lb['longer_side']['rate'])} "
          f"(n={lb['longer_side']['n']}); correlation {_fmt(lb['correlation'])}", "",
          "| log ratio | x win rate | n |", "|---|---|---|"]
    for b in lb["bins"]:
        lo, hi = b["log_ratio"]
        L.append(f"| [{lo:g}, {hi:g}) | {_fmt(b['rate'])} | {b['n']} |")
    L += ["", "## Task-level verdicts (craze's side: W/L/T; `l` = low confidence)", ""]
    for m, table in rep["task_tables"].items():
        others = sorted({o for row in table.values() for o in row})
        if not others:
            continue
        L += [f"**{m}**", "", "| task | " + " | ".join(others) + " |", "|---" * (len(others) + 1) + "|"]
        for t, row in table.items():
            L.append(f"| {t} | " + " | ".join(row.get(o, "") for o in others) + " |")
        L.append("")
    if rep.get("plan_modes"):
        L += ["## Plan-mode handling", ""] + [f"- {p}" for p in rep["plan_modes"]] + [""]
    if "compare" in rep:
        c = rep["compare"]
        L += ["## Compare (new craze build vs old)", "",
              f"Pooled win rate: {_fmt(c['pooled']['rate'])} (90% CI {c['pooled']['ci90']}, n={c['pooled']['n']}); "
              f"keep (conditional, ≥55%): {c['keep_rule']['conditional_keep']}; keep (requested, ≥45%): "
              f"{c['keep_rule']['requested_keep']}"
              + ("" if c["coverage"]["complete"] else f" (coverage incomplete: {c['coverage']['missing'][:10]})"), ""]
        for m, p in c["per_model"].items():
            L.append(f"- {m}: win rate {_fmt(p['win_rate_new_vs_old']['rate'])} (n={p['win_rate_new_vs_old']['n']}); "
                     f"dev objective {_fmt(p['objective_new']['dev'])} vs {_fmt(p['objective_old']['dev'])}"
                     + (" — **objective drop**" if p["objective_drop"] else ""))
    if "losses" in rep:
        L += [""] + render_losses(rep["losses"], rep.get("rubrics") or {})
    return "\n".join(L) + "\n"


def _task_hash_line(th: dict[str, list[str]]) -> str:
    if not th:
        return "– (no counted verdict carries one)"
    return "; ".join(f"{t} {','.join(h[:12] for h in hs)}" for t, hs in th.items())


def _grades(items: list[dict]) -> str:
    return ", ".join(f"{g['item']} ({g['grade']})" for g in items) or "none"


def render_losses(losses: list[dict], rubrics: dict[str, list[str]]) -> list[str]:
    """The "Where craze lost" section: each lost pair, the rubric items craze did not
    meet against the other side's, and the judge's reasons per order."""
    L = ["## Where craze lost", "",
         f"{len(losses)} final verdict(s) in which a craze run lost (scores are craze's vs the other side's)." if losses
         else "No final verdict in which a craze run lost.", ""]
    for x in losses:
        L.append(f"### {x['task']} · {x['model']} · {x['split']} — {x['craze_run_key']} vs {x['other_harness']}")
        L.append("")
        L.append(f"Scores {_fmt(x['craze_score'])} vs {_fmt(x['other_score'])}; judge {x['judge_model']}"
                 f"{' (both orders)' if x.get('both_orders') else ''}{' after escalation' if x.get('escalated') else ''}, "
                 f"confidence {x.get('confidence')}; other run {x['other_run_key']}")
        rubric = rubrics.get(x["task"]) or []
        for i, o in enumerate(x["orders"], 1):
            L.append(f"- order {i} (craze shown as {o.get('craze_shown_as') or '?'}, {o.get('judge_model')}): "
                     f"winner {o.get('winner')}, scores {_fmt(o.get('craze_score'))} vs {_fmt(o.get('other_score'))}")
            L.append(f"  - craze did not meet: {_grades(o['craze_not_met'])}; "
                     f"{x['other_harness']} did not meet: {_grades(o['other_not_met'])}")
            theirs = {g["item"] for g in o["other_not_met"]}
            for g in o["craze_not_met"]:
                if g["item"] not in theirs and isinstance(g["item"], int) and 0 < g["item"] <= len(rubric):
                    text = " ".join(rubric[g["item"] - 1].split())
                    L.append(f"  - only craze {g['grade']} item {g['item']}: {text[:240]}{'…' if len(text) > 240 else ''}")
            reasons = " ".join(str(o.get("reasons") or "").split())
            L.append(f"  - reasons: {reasons or '–'}")
        L.append("")
    return L


def length_csv(rep: dict) -> str:
    buf = io.StringIO()
    w = csv.writer(buf)
    w.writerow(["pair", "log_ratio", "x_outcome"])
    for row in rep["length_bias"]["rows"]:
        w.writerow([row["pair"], row["log_ratio"], row["x_outcome"]])
    return buf.getvalue()


def write(rep: dict, out: Path) -> None:
    out.mkdir(parents=True, exist_ok=True)
    slim = {k: v for k, v in rep.items() if k != "length_bias"}
    slim["length_bias"] = {k: v for k, v in rep["length_bias"].items() if k != "rows"}
    (out / "report.json").write_text(json.dumps(slim, indent=2, default=str) + "\n")
    (out / "report.md").write_text(render_markdown(rep))
    (out / "length_vs_verdict.csv").write_text(length_csv(rep))
