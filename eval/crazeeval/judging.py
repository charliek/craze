"""Judging whole batches: pairing runs, resumable verdict files, calibration (plan 029 §3.1.6).

- ``judge-batch`` pairs every scored run of one harness with every scored run of
  another on the same task and model (one batch, or two batches' builds of the same
  harness for ``--compare``) and appends one record per pair to ``verdicts.jsonl`` --
  held-out pairs to the sealed ``heldout/verdicts.jsonl``, never printed. A pair
  already judged with the same judge hash, judge model and mode is skipped.
- ``calibrate`` judges 30 dev pairs in both orders by sol and again by luna, plus 10
  padding pairs (one side is the other's answer with neutral padding), and writes the
  flip rate, luna's agreement, the padded side's win count and the gate decisions. A
  gate passes only when every requested pair has a verdict in both orders; otherwise it
  is ``incomplete``, which fails it (review r1-c2 §7).
- ``judge-batch`` enforces those decisions (``enforce_calibration``): luna is refused
  unless its agreement gate passed, and single-order judging becomes both-order unless
  the flip gate passed -- with no (or a stale) calibration file, both hold. An explicit
  override is allowed and recorded in every verdict it produced.
"""

from __future__ import annotations

import asyncio
import json
import random
from dataclasses import dataclass
from pathlib import Path

from crazeeval.judge import Judge, Pair, judge_hash, judge_pair, judge_success_bar_pair
from crazeeval.packet import Side, load_side
from crazeeval.report import UNSCORED, iter_results, read_jsonl
from crazeeval.runners import RUNNERS
from crazeeval.tasks import Task

FLIP_GATE = 0.20
LUNA_AGREEMENT_GATE = 0.80
PADDING_MAX_PREFERRED = 2
PASS, FAIL, INCOMPLETE = "pass", "fail", "incomplete"
CALIBRATION_FILE = "calibration.json"


class CalibrationRefusal(RuntimeError):
    pass


# -- finding runs --------------------------------------------------------------------------------------


@dataclass
class RunRef:
    harness: str
    model: str
    task: str
    rep: int
    rep_dir: Path
    batch: str


def find_runs(batch: Path, include_heldout: bool = True) -> list[RunRef]:
    """Every scored run of a batch (``heldout/`` too unless ``include_heldout`` is off)."""
    return [RunRef(r["harness"], r["model"], r["task"], int(r.get("rep") or 1), rep_dir, Path(batch).name)
            for _, rep_dir, r in iter_results(batch, include_heldout)
            if r.get("status") not in UNSCORED and "harness" in r]


def to_pair(x: RunRef, y: RunRef, tasks: dict[str, Task]) -> Pair:
    return Pair(tasks[x.task], x.model, load_side(x.rep_dir), load_side(y.rep_dir))


def make_pairs(runs_x: list[RunRef], runs_y: list[RunRef], hx: str, hy: str, tasks: dict[str, Task],
               models: set[str] | None = None, task_ids: set[str] | None = None) -> list[tuple[RunRef, RunRef]]:
    """Every (x, y) with x from harness ``hx``, y from ``hy``, the same task and model."""
    ys: dict[tuple[str, str], list[RunRef]] = {}
    for r in runs_y:
        if r.harness == hy:
            ys.setdefault((r.task, r.model), []).append(r)
    out = []
    for x in runs_x:
        if x.harness != hx or x.task not in tasks:
            continue
        if models and x.model not in models or task_ids and x.task not in task_ids:
            continue
        for y in ys.get((x.task, x.model), []):
            if x.rep_dir != y.rep_dir:
                out.append((x, y))
    return out


def strip_patterns(*harnesses: str) -> list[str]:
    out = []
    for h in harnesses:
        out += list(getattr(RUNNERS.get(h), "workspace_state", []) or [])
    return out


# -- verdict files -----------------------------------------------------------------------------------------


def verdict_path(out: Path, split: str) -> Path:
    return out / ("heldout" if split == "heldout" else "") / "verdicts.jsonl"


def done_pairs(out: Path, jhash: str) -> set[tuple]:
    return {(v["pair"], v.get("judge_mode"))
            for p in (verdict_path(out, "dev"), verdict_path(out, "heldout"))
            for v in read_jsonl(p)
            if v.get("judge_hash") == jhash and v.get("result") is not None}


def _append(p: Path, rec: dict) -> None:
    p.parent.mkdir(parents=True, exist_ok=True)
    with open(p, "a") as f:
        f.write(json.dumps(rec, default=str) + "\n")


async def judge_batch(pairs: list[tuple[RunRef, RunRef]], tasks: dict[str, Task], out: Path, judge: Judge,
                      judge_model: str, mode: str, seed, log=print, calibration: dict | None = None) -> dict:
    """``mode``: "single" (one order, the seed's), "both" (both orders, disagreement =
    tie) or "success-bar" (both orders by sol, astra on disagreement or low confidence).
    ``calibration``: enforce_calibration's record, written into every verdict."""
    jhash = judge_hash(tasks)
    done = done_pairs(out, jhash)
    counts = {"judged": 0, "skipped": 0, "no_verdict": 0}

    async def one(x: RunRef, y: RunRef):
        task = tasks[x.task]
        pair = to_pair(x, y, tasks)
        if x.batch != y.batch:
            pair.label = f"{x.batch}~{y.batch}"
        if (pair.id, mode) in done:
            counts["skipped"] += 1
            return
        strip = strip_patterns(x.harness, y.harness)
        if mode == "success-bar":
            rec = await judge_success_bar_pair(judge, pair, seed, strip)
        else:
            rec = await judge_pair(judge, pair, judge_model, seed, mode == "both", strip)
        rec.update({"hx": x.harness, "hy": y.harness, "split": task.split, "x_batch": x.batch, "y_batch": y.batch,
                    "judge_hash": jhash, "judge_mode": mode, "seed": seed, "calibration": calibration})
        _append(verdict_path(out, task.split), rec)
        if rec["result"] is None:
            counts["no_verdict"] += 1
        else:
            counts["judged"] += 1
        if task.split == "heldout":
            log(f"[sealed] {x.task} {x.model}: judged")
        else:
            log(f"{x.task} {x.model} {x.harness} vs {y.harness}: {(rec['result'] or {}).get('winner', 'no verdict')}")

    await asyncio.gather(*(one(x, y) for x, y in pairs))
    return counts


# -- calibration ----------------------------------------------------------------------------------------------


def pad_answer(task: Task, answer: str) -> str:
    """Deterministic neutral padding (§3.1.6): the task restated and the answer's first
    paragraph recapped -- no new facts."""
    body = (answer or "").strip()
    first = body.split("\n\n", 1)[0].strip()
    return (f"To restate what was asked: {task.prompt.strip()}\n\n{body}\n\n"
            f"To recap the main point from above: {first}")


def pick_calibration_pairs(pairs: list[tuple[RunRef, RunRef]], n: int, seed) -> list[tuple[RunRef, RunRef]]:
    """``n`` pairs spread across tasks and models (round-robin over (task, model)
    groups, each shuffled by the seed)."""
    rng = random.Random(f"calibration|{seed}")
    groups: dict[tuple[str, str], list] = {}
    for x, y in pairs:
        groups.setdefault((x.task, x.model), []).append((x, y))
    keys = sorted(groups)
    rng.shuffle(keys)
    for k in keys:
        rng.shuffle(groups[k])
    out = []
    while len(out) < n and any(groups[k] for k in keys):
        for k in keys:
            if groups[k] and len(out) < n:
                out.append(groups[k].pop())
    return out


def _complete(records: list[dict], requested: int) -> bool:
    """Every requested pair is there, each with a verdict in both orders (a both-order
    result exists only when both orders produced one)."""
    return (requested > 0 and len(records) == requested
            and all(r.get("result") is not None and len(r.get("orders") or []) == 2
                    and all(o.get("verdict") is not None for o in r["orders"]) for r in records))


def flip_rate(records: list[dict], requested: int) -> dict:
    """A both-order record flips when its two orders' winners differ."""
    usable = [r for r in records if r.get("result") is not None]
    flips = sum(1 for r in usable if not r["result"]["agree"])
    return {"requested": requested, "pairs": len(usable), "flips": flips, "complete": _complete(records, requested),
            "rate": flips / len(usable) if usable else None}


def agreement(sol: list[dict], luna: list[dict], requested: int) -> dict:
    """The share of pairs whose combined (both-order) verdicts agree; complete only when
    both judges covered every requested pair in both orders."""
    s = {r["pair"]: r["result"]["winner"] for r in sol if r.get("result")}
    l_ = {r["pair"]: r["result"]["winner"] for r in luna if r.get("result")}
    common = sorted(set(s) & set(l_))
    agree = sum(1 for p in common if s[p] == l_[p])
    complete = _complete(sol, requested) and _complete(luna, requested) and len(common) == requested
    return {"requested": requested, "pairs": len(common), "agree": agree, "complete": complete,
            "rate": agree / len(common) if common else None}


def padding_preference(records: list[dict], requested: int) -> dict:
    """Records whose y side is the padded copy of x: how often the padded side won
    (combined both-order verdicts), plus per-order counts."""
    usable = [r for r in records if r.get("result") is not None]
    padded_wins = sum(1 for r in usable if r["result"]["winner"] == "y")
    per_order = sum(1 for r in usable for o in r.get("orders") or [] if (o.get("verdict") or {}).get("winner") == "y")
    return {"requested": requested, "pairs": len(usable), "complete": _complete(records, requested),
            "padded_preferred": padded_wins, "padded_preferred_single_orders": per_order}


def gate_decisions(flip: dict, agree: dict, pad: dict) -> dict:
    """Each gate is pass, fail or incomplete; only pass lets its relaxation through
    (review r1-c2 §7). Incomplete calibration therefore forces both orders, keeps luna
    off bulk pairs and does not clear the length control."""
    def gate(stat: dict, ok: bool) -> str:
        return INCOMPLETE if not stat["complete"] else (PASS if ok else FAIL)

    flip_status = gate(flip, flip["rate"] is not None and flip["rate"] <= FLIP_GATE)
    luna_status = gate(agree, agree["rate"] is not None and agree["rate"] >= LUNA_AGREEMENT_GATE)
    pad_status = gate(pad, pad["padded_preferred"] <= PADDING_MAX_PREFERRED)
    return {
        "flip_gate": flip_status,
        "luna_gate": luna_status,
        "padding_gate": pad_status,
        "complete": all(g != INCOMPLETE for g in (flip_status, luna_status, pad_status)),
        "both_orders_for_all_pairs": flip_status != PASS,
        "luna_judges_bulk_pairs": luna_status == PASS,
        "padding_ok": pad_status == PASS,
        "thresholds": {"flip_gate": FLIP_GATE, "luna_agreement": LUNA_AGREEMENT_GATE,
                       "padding_max_preferred": PADDING_MAX_PREFERRED},
    }


def load_calibration(path: Path | None, jhash: str) -> dict:
    """The calibration decisions for batch judging. A missing file, an unreadable one or
    one made under another judge hash counts as no calibration: every gate fails."""
    info = {"file": str(path) if path else None, "status": "missing", "both_orders_required": True,
            "luna_allowed": False}
    if path is None or not Path(path).is_file():
        return info
    try:
        cal = json.loads(Path(path).read_text())
    except (OSError, ValueError):
        return {**info, "status": "unreadable"}
    if cal.get("judge_hash") != jhash:
        return {**info, "status": "stale (another judge hash)"}
    g = cal.get("gates") or {}
    return {**info, "status": "complete" if g.get("complete") else "incomplete",
            "flip_gate": g.get("flip_gate"), "luna_gate": g.get("luna_gate"),
            "both_orders_required": g.get("flip_gate") != PASS, "luna_allowed": g.get("luna_gate") == PASS}


def enforce_calibration(judge_model: str, mode: str, cal: dict, override: str | None = None) -> tuple[str, dict]:
    """The mode batch judging may use under the calibration decisions: luna only when
    its agreement gate passed (else CalibrationRefusal), and both orders unless the
    flip gate passed. ``override`` (a reason) lifts both and is recorded."""
    record = {**cal, "override": override, "forced_both_orders": False}
    if override:
        return mode, record
    if judge_model == "luna" and mode != "success-bar" and not cal["luna_allowed"]:
        raise CalibrationRefusal(
            f"luna may judge bulk pairs only after calibration shows >= {LUNA_AGREEMENT_GATE:.0%} agreement with sol "
            f"(calibration: {cal['status']}, luna gate: {cal.get('luna_gate')}); pass --override-calibration REASON "
            "to judge anyway")
    if mode == "single" and cal["both_orders_required"]:
        record["forced_both_orders"] = True
        return "both", record
    return mode, record


async def calibrate(pairs: list[tuple[RunRef, RunRef]], tasks: dict[str, Task], out: Path, judge: Judge, seed,
                    n_pairs: int = 30, n_padding: int = 10, log=print) -> dict:
    """§3.1.6 calibration over dev pairs; writes ``calibration.json`` and the records."""
    out.mkdir(parents=True, exist_ok=True)
    chosen = [to_pair(x, y, tasks) for x, y in pick_calibration_pairs(pairs, n_pairs, seed)]

    async def run(pairs_, model, label):
        recs = await asyncio.gather(*(judge_pair(judge, p, model, seed, True, strip_patterns(p.x.result.get("harness"),
                                                                                              p.y.result.get("harness")))
                                      for p in pairs_))
        with open(out / f"{label}.jsonl", "w") as f:
            for r in recs:
                f.write(json.dumps(r, default=str) + "\n")
        log(f"calibration {label}: {sum(1 for r in recs if r['result'])}/{len(recs)} verdicts")
        return recs

    sol = await run(chosen, "sol", "sol")
    luna = await run(chosen, "luna", "luna")
    # Padding: one run's own answer against itself padded (same evidence, diff and
    # objective results -- only the answer differs).
    rng = random.Random(f"padding|{seed}")
    singles = sorted({x.rep_dir: x for x, _ in pairs}.values(), key=lambda r: str(r.rep_dir))
    rng.shuffle(singles)
    padded_pairs = []
    for r in singles[:n_padding]:
        side = load_side(r.rep_dir)
        task = tasks[r.task]
        answer = side.result.get("answer") or ""
        twin = Side(rep_dir=side.rep_dir, result={**side.result, "run_key": side.key + "+padded",
                                                  "run_id": (side.result.get("run_id") or side.key) + "+padded"},
                    attempt_dir=side.attempt_dir)
        padded_pairs.append(Pair(task, r.model, side, twin, answers=(answer, pad_answer(task, answer)), label="padding"))
    pad = await run(padded_pairs, "sol", "padding")
    summary = {
        "seed": seed,
        "judge_hash": judge_hash(tasks),
        "requested": {"pairs": n_pairs, "padding": n_padding, "available_pairs": len(pairs)},
        "flip": flip_rate(sol, n_pairs),
        "luna_agreement": agreement(sol, luna, n_pairs),
        "padding": padding_preference(pad, n_padding),
    }
    summary["gates"] = gate_decisions(summary["flip"], summary["luna_agreement"], summary["padding"])
    (out / CALIBRATION_FILE).write_text(json.dumps(summary, indent=2) + "\n")
    return summary
