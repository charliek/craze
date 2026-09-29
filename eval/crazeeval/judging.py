"""Judging whole batches: pairing runs, resumable verdict files, calibration (plan 029 §3.1.6).

- ``judge-batch`` pairs every scored run of one harness with every scored run of
  another on the same task and model (one batch, or two batches' builds of the same
  harness for ``--compare``) and appends one record per pair to ``verdicts.jsonl`` --
  held-out pairs to the sealed ``heldout/verdicts.jsonl``, never printed. A pair
  already judged in the same mode, by a verdict that still stands
  (``judge.verdict_current``: its task's judge hash, else for an older record the
  global one), is skipped. ``judge-hash --stamp`` (``stamp_task_hashes``) gives older
  records their task hash while the global one still matches and the task is what its
  batch ran, so they keep standing. Every writer of a verdict or calibration file holds
  its ``jsonl_lock``.
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
import fcntl
import json
import os
import random
import uuid
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

from crazeeval.judge import (
    Judge,
    Pair,
    judge_hash,
    judge_pair,
    judge_success_bar_pair,
    task_judge_hash,
    task_judge_hashes,
    verdict_current,
)
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


def done_pairs(out: Path, tasks: dict[str, Task]) -> set[tuple]:
    """The (pair, mode) already judged with a verdict that still stands
    (``judge.verdict_current``: its task's judge hash is unchanged -- or, for a record
    from before per-task hashes, the global hash is), so adding or changing one task
    never re-judges another's pairs."""
    th, jhash = task_judge_hashes(tasks), judge_hash(tasks)
    return {(v["pair"], v.get("judge_mode"))
            for p in (verdict_path(out, "dev"), verdict_path(out, "heldout"))
            for v in read_jsonl(p)
            if v.get("result") is not None and verdict_current(v, th, jhash)}


LOCK_SUFFIX = ".lock"


def lock_path(p: Path) -> Path:
    """``verdicts.jsonl`` -> ``verdicts.jsonl.lock`` beside it."""
    p = Path(p)
    return p.with_name(p.name + LOCK_SUFFIX)


@contextmanager
def jsonl_lock(p: Path):
    """An exclusive advisory ``flock`` on ``<file>.lock`` beside a verdict or calibration
    JSONL file, taken by every writer of the file -- each append (``_append``), each
    calibration write -- and held by the stamper across its read, compute and replace.
    A sibling file, not the data file: the stamper's replace gives the data file a new
    inode, which a lock on the file itself would not follow."""
    p = Path(p)
    p.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(lock_path(p), os.O_WRONLY | os.O_CREAT, 0o644)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)  # releases the lock


PRE_STAMP_SUFFIX = ".pre-stamp.jsonl"
TASK_CHANGED = "task_changed_or_unknown"


class StampError(RuntimeError):
    pass


def pre_stamp_path(p: Path) -> Path:
    """``verdicts.jsonl`` -> ``verdicts.pre-stamp.jsonl`` beside it."""
    p = Path(p)
    stem = p.name[: -len(".jsonl")] if p.name.endswith(".jsonl") else p.name
    return p.with_name(stem + PRE_STAMP_SUFFIX)


def owning_batch_tasks(path: Path) -> dict | None:
    """The task fingerprints (``batch.json``'s ``tasks``) of the batch a verdict or
    calibration file belongs to: the nearest of its three closest ancestor directories
    holding a ``batch.json`` (``<batch>/judging/``, ``<batch>/judging/heldout/``,
    ``<batch>/calibration/``). None when there is none, or it cannot be read."""
    for d in list(Path(path).resolve().parents)[:3]:
        bj = d / "batch.json"
        if bj.is_file():
            try:
                tasks = json.loads(bj.read_text()).get("tasks")
            except (OSError, ValueError, AttributeError):
                return None
            return tasks if isinstance(tasks, dict) else None
    return None


class TaskCheck:
    """Whether a task is today what it was when a batch ran: its fingerprint now
    (``batch.task_fingerprint``: task.toml, testdata and, off 3eabb31, its commit) equals
    the one the batch recorded. Fingerprints are computed once per task."""

    def __init__(self, tasks: dict[str, Task]):
        self.tasks = tasks
        self._now: dict[str, str] = {}

    def unchanged(self, tid: object, batch_tasks: dict | None) -> bool:
        """False for anything but a string task id (a malformed record's ``"task"``,
        e.g. a list, is left alone -- counted as changed or unknown, same as an id this
        run does not have -- never looked up, which would raise on an unhashable id)."""
        from crazeeval.batch import task_fingerprint

        if not isinstance(tid, str) or not batch_tasks or tid not in self.tasks or tid not in batch_tasks:
            return False
        if tid not in self._now:
            self._now[tid] = task_fingerprint(self.tasks[tid])
        return batch_tasks[tid] == self._now[tid]


def _stamp_lines(text: str, tasks: dict[str, Task], batch_tasks: dict | None) -> tuple[dict, str]:
    th, gh = task_judge_hashes(tasks), judge_hash(tasks)
    check = TaskCheck(tasks)
    counts = {"records": 0, "stamped": 0, "already": 0, "other_hash": 0, "no_hash": 0, TASK_CHANGED: 0,
              "unparsed": 0, "written": False, "original_kept": None}
    lines = text.split("\n")  # JSONL: "\n" only, so every other line is kept as it is
    for i, line in enumerate(lines):
        if not line.strip():
            continue
        try:
            rec = json.loads(line)
        except ValueError:
            rec = None
        if not isinstance(rec, dict):
            counts["unparsed"] += 1
            continue
        counts["records"] += 1
        if rec.get("task_judge_hash"):
            counts["already"] += 1
        elif not rec.get("judge_hash"):
            counts["no_hash"] += 1
        elif rec["judge_hash"] != gh:
            counts["other_hash"] += 1
        elif not check.unchanged(rec.get("task"), batch_tasks):
            counts[TASK_CHANGED] += 1
        else:
            rec["task_judge_hash"] = th[rec["task"]]
            counts["stamped"] += 1
            lines[i] = json.dumps(rec)
    return counts, "\n".join(lines)


def _keep_original(p: Path, raw: bytes) -> str | None:
    """``<name>.pre-stamp.jsonl``, with the file's bytes before its first stamp,
    written and fsynced to a unique temporary file first and only then published as
    the final name by an exclusive hard link (``os.link``) -- so a crash mid-write
    never leaves a partial backup under the final name: it is either absent or
    complete. An existing final is kept, never replaced; the temp is removed on every
    path, success or failure."""
    pre = pre_stamp_path(p)
    tmp = pre.with_name(f"{pre.name}.{os.getpid()}.{uuid.uuid4().hex}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    try:
        try:
            view = memoryview(raw)
            while view:
                view = view[os.write(fd, view):]
            os.fsync(fd)
        finally:
            os.close(fd)
        try:
            os.link(tmp, pre)
        except FileExistsError:
            return None
        return pre.name
    finally:
        os.unlink(tmp)


def _replace_with(p: Path, data: bytes) -> None:
    """The stamped file's bytes in place of the old, atomically."""
    tmp = p.with_name(p.name + ".stamp.tmp")
    tmp.write_bytes(data)
    os.replace(tmp, p)


def stamp_task_hashes(path: Path, tasks: dict[str, Task], dry_run: bool = False) -> dict:
    """Give every record of a verdict or calibration JSONL file that has no
    ``task_judge_hash`` its task's current ``task_judge_hash``, so the record keeps
    standing (``judge.verdict_current``) after another task is added or changed -- but
    only when both describe the judge the record saw: its global ``judge_hash`` is
    today's, **and** its task's fingerprint now equals the one recorded in the
    ``batch.json`` of the batch that owns the file (``owning_batch_tasks``). The global
    hash alone would not do: it leaves out the task's mode (which changes the packet's
    plan note) and everything else of the packet outside the prompt, rubric and false
    claims. Records with another global hash, with none, or on a task that changed, is
    unknown, or has no batch fingerprint to compare with are left alone and counted; a
    line that does not parse is kept as it is.

    Only stamped lines change: the rest keep their bytes (read and written as UTF-8). The
    file as it first was is kept once, as ``<name>.pre-stamp.jsonl`` beside it, created
    exclusively and never replaced. The whole read-compute-replace holds the file's
    ``jsonl_lock``, which every appender takes, so no append is lost. Nothing is written
    with ``dry_run`` (which takes no lock) or when nothing needs a stamp. Returns the
    counts."""
    p = Path(path)
    if p.name.endswith(PRE_STAMP_SUFFIX):
        raise StampError("a pre-stamp original is never stamped")
    if p.is_symlink() or not p.is_file():
        raise StampError("not a regular file")
    batch_tasks = owning_batch_tasks(p)

    def compute(raw: bytes) -> tuple[dict, str]:
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError as e:
            raise StampError("not UTF-8") from e
        return _stamp_lines(text, tasks, batch_tasks)

    if dry_run:
        return compute(p.read_bytes())[0]
    with jsonl_lock(p):
        raw = p.read_bytes()
        counts, text = compute(raw)
        if not counts["stamped"]:
            return counts
        counts["original_kept"] = _keep_original(p, raw)
        if p.read_bytes() != raw:  # only a writer that ignores the lock gets here
            raise StampError("the file changed while it was read; nothing stamped")
        _replace_with(p, text.encode("utf-8"))
        counts["written"] = True
    return counts


def _append(p: Path, rec: dict) -> None:
    """One record appended to a verdict file, under the file's lock (``jsonl_lock``)."""
    with jsonl_lock(p), open(p, "a", encoding="utf-8") as f:
        f.write(json.dumps(rec, default=str) + "\n")


async def judge_batch(pairs: list[tuple[RunRef, RunRef]], tasks: dict[str, Task], out: Path, judge: Judge,
                      judge_model: str, mode: str, seed, log=print, calibration: dict | None = None) -> dict:
    """``mode``: "single" (one order, the seed's), "both" (both orders, disagreement =
    tie) or "success-bar" (both orders by sol, astra on disagreement or low confidence).
    ``calibration``: enforce_calibration's record, written into every verdict."""
    jhash = judge_hash(tasks)
    done = done_pairs(out, tasks)
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
                    "judge_hash": jhash, "task_judge_hash": task_judge_hash(task), "judge_mode": mode, "seed": seed,
                    "calibration": calibration})
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
    jhash = judge_hash(tasks)
    chosen = [to_pair(x, y, tasks) for x, y in pick_calibration_pairs(pairs, n_pairs, seed)]

    async def run(pairs_, model, label):
        recs = await asyncio.gather(*(judge_pair(judge, p, model, seed, True, strip_patterns(p.x.result.get("harness"),
                                                                                              p.y.result.get("harness")))
                                      for p in pairs_))
        path = out / f"{label}.jsonl"
        with jsonl_lock(path), open(path, "w", encoding="utf-8") as f:
            for r in recs:
                r["judge_hash"] = jhash
                r["task_judge_hash"] = task_judge_hash(tasks[r["task"]])
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
        "judge_hash": jhash,
        "requested": {"pairs": n_pairs, "padding": n_padding, "available_pairs": len(pairs)},
        "flip": flip_rate(sol, n_pairs),
        "luna_agreement": agreement(sol, luna, n_pairs),
        "padding": padding_preference(pad, n_padding),
    }
    summary["gates"] = gate_decisions(summary["flip"], summary["luna_agreement"], summary["padding"])
    (out / CALIBRATION_FILE).write_text(json.dumps(summary, indent=2) + "\n")
    return summary
