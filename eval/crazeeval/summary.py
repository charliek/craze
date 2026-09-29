"""``crazeeval summarize``: a campaign's compact ``summary.json`` from its final reports.

A campaign's final is often several report groups (one ``crazeeval report`` per
provider or baseline). This reads each report's ``report.json`` and writes one small,
deterministic JSON document for the main repository (``eval/results/<campaign>/``)::

    {"schema": 1, "campaign": NAME, "groups": [<one slim group per report>, ...]}

Each group, in ``--report`` order, holds:

- ``report``: the report directory's name; ``models``: the models it covers (those with
  craze runs, or with a best open harness fixed); ``other_models``: the rest of the
  report's models (runs reused from a shared batch, no bar) -- named, their rows left
  out;
- ``bar``: per covered model the best open harness and why (its baseline batch by
  name), the outcome and reason, each condition (``all_tasks``, ``heldout``, ``dev``:
  objective counts, win rate with its 90% CI, W/T/L and n) and coverage gaps if any;
- ``objective``: pass counts per ``model/harness`` on dev, held-out and all tasks, with
  each task's mean pass; ``statuses``: run statuses per ``model/harness``;
- ``win_rates``: per split, per covered model and ``pooled``, ``row vs column`` cells
  (rate, CI, W/T/L, n);
- ``metrics``: metric medians per ``model/harness``; ``task_tables``: craze's W/L/T per
  task and harness;
- ``provenance``: the whole report's -- batch names (never paths), the baseline batch,
  the judge hash(es) and per-task hashes, the plan modes, the verdict counts (over every
  model), and ``sealed``.

**Scope:** everything in a group but ``provenance`` is the covered models' alone. The
per-model rows are filtered to them, and ``pooled`` is recomputed from their cells (the
W/T/L summed per pair, the rate and Wilson interval by the report's ``WinRate``) --
never the report's pooled matrix, which counts every model's verdicts. The report's
other report-wide aggregates cannot be split by model from ``report.json`` and are left
out: the length-bias digest, and a ``--compare`` block (its pooled rate and keep rule).

Left out too: verdict lists, loss lists and rubrics, length-bias rows, verdict source
paths and every absolute path. The output has sorted keys and no timestamp, so
the same reports give the same bytes. A sealed report (held-out not read; its bar only
provisional) is refused unless ``allow_sealed``. Before anything is written the output
is checked -- no string may be an absolute path or hold a local one (``archive``'s
``path_token``), and the text may not hold ``/home/`` or the owner's home -- and a hit
writes nothing.
"""

from __future__ import annotations

import json
import os
import re
from pathlib import Path, PurePosixPath

from crazeeval import archive as arch
from crazeeval import report as rp

SCHEMA = 1
REPORT_FILE = "report.json"
BAR_PARTS = ("all_tasks", "heldout", "dev")
VERDICT_COUNTS = ("loaded", "stale", "bound_to_scored_runs", "unbound", "final")
# An absolute path inside free text (a best open harness's "recorded in <path>"): a
# whitespace-delimited token that starts with "/".
_ABS_TOKEN = re.compile(r"(?<!\S)/\S+")


class SummaryError(RuntimeError):
    """A report cannot be summarized, or the summary cannot be written as asked."""


def load_report(p: Path) -> tuple[str, dict]:
    """A report's name (its directory's) and its parsed ``report.json``. ``p`` is a
    report output directory or its ``report.json``."""
    p = Path(p)
    f = p / REPORT_FILE if p.is_dir() else p
    try:
        rep = json.loads(f.read_text(encoding="utf-8"))
    except (OSError, ValueError) as e:
        raise SummaryError(f"{f}: not a readable {REPORT_FILE} ({type(e).__name__})") from e
    if not isinstance(rep, dict) or not isinstance(rep.get("success_bar"), dict) or "objective" not in rep:
        raise SummaryError(f"{f}: not a crazeeval report (no success_bar or objective)")
    return f.resolve().parent.name, rep


def _split_key(key: str) -> tuple[str, str]:
    """``model/harness`` -> (model, harness): a model key may hold a "/", a harness never."""
    m, _, h = str(key).rpartition("/")
    return m, h


def batch_ref(path, names: set[str]) -> str | None:
    """An absolute path reduced to where it sits in a batch: from the last component
    that is one of the report's batch ``names`` on (``base-x/best-open-harness.json``),
    else its last component."""
    if not isinstance(path, str) or not path:
        return None
    parts = PurePosixPath(path).parts
    for i in range(len(parts) - 1, -1, -1):
        if parts[i] in names:
            return "/".join(parts[i:])
    return parts[-1] if parts else None


def _reduce_text(text, names: set[str]):
    """``text`` with every absolute-path token reduced by ``batch_ref``."""
    if not isinstance(text, str):
        return text
    return _ABS_TOKEN.sub(lambda m: batch_ref(m.group(0), names) or "", text)


def cell(w) -> dict | None:
    """A win-rate record, compact: rate, 90% CI, W/T/L and n."""
    if not isinstance(w, dict):
        return None
    return {"rate": w.get("rate"), "ci90": w.get("ci90"), "wtl": [w.get("wins"), w.get("ties"), w.get("losses")],
            "n": w.get("n")}


def _condition(c: dict) -> dict:
    return {"objective_native": c.get("objective_native"), "objective_other": c.get("objective_other"),
            "objective_ok": c.get("objective_ok"), "win_rate": cell(c.get("win_rate")), "win_ok": c.get("win_ok")}


def _bar(sb: dict, names: set[str]) -> dict:
    best = sb.get("best_open_harness") if isinstance(sb.get("best_open_harness"), dict) else {}
    out: dict = {
        "outcome": sb.get("outcome"),
        "reason": sb.get("reason"),
        "best_open_harness": {
            "harness": best.get("harness"),
            "reason": best.get("reason"),
            "counts": best.get("counts"),
            "baseline_batch": batch_ref(best.get("baseline_batch"), names),
            "source": _reduce_text(best.get("source"), names),
        },
    }
    conds = {p: _condition(sb[p]) for p in BAR_PARTS if isinstance(sb.get(p), dict)}
    if conds:
        out["conditions"] = conds
    cov = sb.get("coverage")
    if isinstance(cov, dict):
        out["coverage_complete"] = bool(cov.get("complete"))
        gaps = {k: v for k, v in (cov.get("missing") or {}).items() if v}
        if gaps:
            out["coverage_gaps"] = gaps
    return out


def covered_models(rep: dict) -> list[str]:
    """The models a report covers: those with craze runs, or whose best open harness
    was fixed. A model with neither only has runs reused from a batch shared across
    providers (the gx plan-task re-run) and has no bar."""
    native = {m for m, h in map(_split_key, rep.get("objective") or {}) if h == rp.NATIVE}
    out = set(native)
    for m, sb in (rep.get("success_bar") or {}).items():
        best = sb.get("best_open_harness") if isinstance(sb, dict) else None
        if isinstance(best, dict) and best.get("harness"):
            out.add(m)
    return sorted(out)


def _rows(table, covered: set[str]) -> dict:
    return {k: v for k, v in (table or {}).items() if _split_key(k)[0] in covered}


def _objective(rep: dict, covered: set[str]) -> dict:
    sealed = bool(rep.get("sealed"))
    out = {}
    for k, c in _rows(rep.get("objective"), covered).items():
        out[k] = {"dev": c.get("dev"), "heldout": None if sealed else c.get("heldout"),
                  "all": None if sealed else c.get("all"), "tasks": c.get("tasks") or {}}
    return out


def _win_rates(rep: dict, covered: set[str]) -> dict:
    """Per split: each covered model's cells, and ``pooled`` recomputed from those cells
    alone -- never the report's pooled matrix, which counts every model's verdicts. A
    pooled cell is the sum of the models' W/T/L for the pair (as the report pools), its
    rate and Wilson interval recomputed by the report's own ``WinRate``."""
    out: dict = {}
    sums: dict[str, dict[str, list[int]]] = {}
    for key, mat in (rep.get("win_rates") or {}).items():
        split, _, model = str(key).partition("|")
        if model not in covered:  # the report's "pooled" and every model left out
            continue
        cells = {}
        for pair, w in (mat or {}).items():
            if not isinstance(w, dict) or not w.get("n"):
                continue
            cells[pair] = cell(w)
            acc = sums.setdefault(split, {}).setdefault(pair, [0, 0, 0])
            for i, k in enumerate(("wins", "ties", "losses")):
                acc[i] += int(w.get(k) or 0)
        if cells:
            out.setdefault(split, {})[model] = cells
    for split, pairs in sums.items():
        out[split]["pooled"] = {pair: cell(rp.WinRate(wins=w, ties=t, losses=lo).to_dict())
                                for pair, (w, t, lo) in pairs.items()}
    return out


def group(name: str, rep: dict) -> dict:
    """One report's slim group: everything but ``provenance`` is the covered models'
    alone (see the module doc)."""
    batches = [b for b in rep.get("batches") or [] if isinstance(b, str)]
    names = {Path(b).name for b in batches}
    if rep.get("baseline_batch"):
        names.add(Path(rep["baseline_batch"]).name)
    covered = covered_models(rep)
    cov = set(covered)
    out = {
        "report": name,
        "models": covered,
        "other_models": sorted(m for m in rep.get("models") or [] if m not in cov),
        "harnesses": sorted({_split_key(k)[1] for k in _rows(rep.get("objective"), cov)}),
        "bar": {m: _bar(rep["success_bar"][m], names) for m in covered if isinstance(rep["success_bar"].get(m), dict)},
        "objective": _objective(rep, cov),
        "statuses": _rows(rep.get("statuses"), cov),
        "win_rates": _win_rates(rep, cov),
        "metrics": _rows(rep.get("metrics"), cov),
        "task_tables": {m: t for m, t in (rep.get("task_tables") or {}).items() if m in cov and t},
        # The whole report's, every model's: where the group's rows came from.
        "provenance": {
            "batches": [Path(b).name for b in batches],
            "baseline_batch": batch_ref(rep.get("baseline_batch"), names),
            "judge_hashes": sorted(rep.get("judge_hashes") or []),
            "judge_hash_now": rep.get("judge_hash_now"),
            "task_judge_hashes": rep.get("task_judge_hashes"),
            "plan_modes": sorted(rep.get("plan_modes") or []),
            "verdicts": {k: (rep.get("verdicts_used") or {}).get(k) for k in VERDICT_COUNTS},
            "sealed": bool(rep.get("sealed")),
        },
    }
    return out


def build(reports: list[tuple[str, dict]], campaign: str | None) -> dict:
    """The summary document: one group per ``(name, report)``, in the order given."""
    return {"schema": SCHEMA, "campaign": campaign, "groups": [group(n, r) for n, r in reports]}


def render(doc: dict) -> str:
    """The document's bytes: sorted keys, no NaN or infinity, a final newline."""
    try:
        return json.dumps(doc, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False) + "\n"
    except ValueError as e:
        raise SummaryError(f"the summary is not plain JSON: {e}") from e


def path_hits(obj, home: str | None = None, where: str = "") -> list[str]:
    """Where in ``obj`` a string (a key or a value) is an absolute path or holds a local
    one (``archive.path_token``: /home, /tmp, the owner's home...). Locations only."""
    tok = arch.path_token(home)
    hits: list[str] = []
    if isinstance(obj, dict):
        for k, v in obj.items():
            here = f"{where}.{k}" if where else str(k)
            if isinstance(k, str) and (k.startswith("/") or tok.search(k)):
                hits.append(f"{here} (key)")
            hits += path_hits(v, home, here)
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            hits += path_hits(v, home, f"{where}[{i}]")
    elif isinstance(obj, str) and (obj.startswith("/") or tok.search(obj)):
        hits.append(where or "<root>")
    return hits


def scan(doc: dict, text: str, home: str | None = None) -> list[str]:
    """The archive's path checks over the summary: absolute or local paths anywhere in
    the document, and ``/home/`` or the owner's home anywhere in its text."""
    errors = []
    hits = path_hits(doc, home)
    if hits:
        errors.append(f"absolute path(s) at {hits[:10]}" + (f" and {len(hits) - 10} more" if len(hits) > 10 else ""))
    data = text.encode("utf-8")
    if any(n in data for n in arch.home_needles(home)):
        errors.append("home path scan: the owner's home path or /home/ in the summary")
    return errors


def summarize(report_dirs: list[Path], out: Path, campaign: str | None, *, allow_sealed: bool = False,
              home: str | None = None) -> dict:
    """Build the summary of ``report_dirs`` and write it to ``out`` (atomically; parent
    directories made). SummaryError -- and nothing written -- when a report is missing,
    sealed without ``allow_sealed``, two reports share a name, ``out`` is a directory or
    an input, or the path scan hits. Returns what was written: path, bytes, groups."""
    seen: set[Path] = set()
    loaded: list[tuple[str, dict]] = []
    inputs: set[Path] = set()
    for d in report_dirs:
        f = (Path(d) / REPORT_FILE if Path(d).is_dir() else Path(d)).resolve()
        if f in seen:
            continue  # the same report given twice is read once
        seen.add(f)
        inputs.add(f)
        name, rep = load_report(f)
        if rep.get("sealed", True) and not allow_sealed:
            raise SummaryError(f"{name}: a sealed report (held-out not read, its bar provisional): a summary is for "
                               "an unsealed final -- pass --allow-sealed to summarize it anyway")
        if name in {n for n, _ in loaded}:
            raise SummaryError(f"two reports named {name!r}: each group is named by its report directory")
        loaded.append((name, rep))
    if not loaded:
        raise SummaryError("no report given")
    out = Path(out)
    if out.is_dir() or out.is_symlink():
        raise SummaryError(f"--out {out} is a directory or a symlink: give the summary file's path")
    if out.resolve() in inputs:
        raise SummaryError(f"--out {out} is one of the input reports")
    doc = build(loaded, campaign)
    text = render(doc)
    errors = scan(doc, text, home)
    if errors:
        raise SummaryError("nothing written: " + "; ".join(errors))
    out.parent.mkdir(parents=True, exist_ok=True)
    tmp = out.with_name(f".{out.name}.tmp-{os.getpid()}")
    try:
        tmp.write_text(text, encoding="utf-8")
        tmp.replace(out)
    finally:
        if tmp.exists():
            tmp.unlink()
    return {"out": str(out), "bytes": len(text.encode("utf-8")), "campaign": campaign,
            "groups": [{"report": g["report"], "models": g["models"],
                        "outcomes": {m: b.get("outcome") for m, b in g["bar"].items()}} for g in doc["groups"]]}
