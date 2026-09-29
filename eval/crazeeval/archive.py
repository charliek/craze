"""``crazeeval archive``: a compact, diffable record of chosen batches (plan 029 W2).

It selects runs and verdicts exactly as ``crazeeval report`` does -- a later ``--batch``
replaces an earlier one's run with the same key, held-out runs and verdicts only with
``--unseal``, a verdict counts only for the two scored runs it judged, one final verdict
per pair by the report's ranking -- and writes:

- ``runs.jsonl``: one line per scored run -- its batch and campaign (names, never
  paths), run key and id, harness with its version and executable hash, the craze build
  for a craze run, the model, wire model and served models, the task, split, category,
  mode, plan mode, rep, status, objective pass and each check's result, answer words,
  tool calls, main requests, wall time, cost, list cost and tokens;
- ``verdicts.jsonl``: one line per final verdict: the task, model, split, the two run
  ids and harnesses, the judge model and mode, the winner, confidence and scores, each
  judged order's rubric grades and reasons, and the judge hashes. The report's own set
  comes first (the verdicts that still stand, ``report.current_verdicts``); a pair with
  none keeps its best older verdict, marked ``"current": false``. A record from before
  per-task judge hashes is stamped with its task's hash by ``judge-hash --stamp``'s rule:
  its global hash is today's and its task's fingerprint equals the one its batch
  recorded (the global hash leaves out the task's mode); otherwise its
  ``task_judge_hash`` stays null and ``task_judge_hash_note`` says why;
- ``batches.json``: per batch its name, campaign, label, harnesses, models, reps, prices,
  task fingerprints, evaluator and config-snapshot hashes, executables, the craze build,
  and its ``best-open-harness.json`` (paths reduced to batch names);
- ``calibration.json`` (only when a batch has calibration): per such batch the
  calibration summary -- rates, gates, judge hash -- never the raw calibration records;
- ``archive.json``: the reconciliation (scored runs selected against run rows written,
  final verdicts against verdict rows written; either mismatch fails the command), the
  verdict counts, how many paths were redacted, the file sizes, and the scan of the data
  files. No timestamp: the same batches archive to the same bytes.

Provenance is looked up by each batch's resolved directory, so two campaigns' batches
that share a name keep their own campaign and build; only names are written.

Left out on purpose: answers, captures, logs, diffs, command lines, judge prompts, raw
calibration records and every path. Free text that is kept -- the judge's reasons above
all -- has every local filesystem path in it (under LOCAL_PATH_ROOTS or the owner's
home) replaced by ``<path>``, a ``:line[:col]`` kept after it (``redact_paths``); routes,
fractions and other slash-rooted text stay. Before it
finishes, the command greps every file in ``--out`` for every loaded key (the batch
runner's key ring and scan) and for the owner's home path and ``/home/``: the backstop; a
hit fails it, naming the file (nothing is deleted), as does a total over ``--max-bytes``
counted over every file in ``--out``. ``--out`` is new, empty or an earlier archive (it
holds ``archive.json``) -- never another non-empty directory, never inside a batch.
"""

from __future__ import annotations

import functools
import json
import os
import re
from pathlib import Path

from crazeeval import judging
from crazeeval import keys as keymod
from crazeeval import paths
from crazeeval import report as rp
from crazeeval.judge import judge_hash, task_judge_hashes
from crazeeval.tasks import Task

RUNS_FILE = "runs.jsonl"
VERDICTS_FILE = "verdicts.jsonl"
BATCHES_FILE = "batches.json"
CALIBRATION_FILE = "calibration.json"
ARCHIVE_FILE = "archive.json"
OUTPUTS = (RUNS_FILE, VERDICTS_FILE, BATCHES_FILE, CALIBRATION_FILE, ARCHIVE_FILE)
DEFAULT_MAX_BYTES = 2_000_000
SHA_PREFIX = 12
LEFT_OUT = ["answers", "captures", "logs", "diffs", "command lines", "judge prompts", "raw calibration records",
            "paths (reduced to batch names)"]
NOTE_OTHER_HASH = "judged under another global judge hash: its task hash cannot be recomputed"
NOTE_TASK_CHANGED = ("its task is not what its batch ran, or its batch's task fingerprint is not found: "
                     "its task hash cannot be confirmed")
NOTE_UNSTAMPED = "no task hash: another global judge hash, or a task that changed since its batch"


def _read_json(p: Path) -> dict:
    try:
        v = json.loads(Path(p).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return v if isinstance(v, dict) else {}


def _prefix(sha) -> str | None:
    return sha[:SHA_PREFIX] if isinstance(sha, str) and sha else None


def _base(p) -> str | None:
    return Path(p).name if isinstance(p, str) and p else None


def batch_campaign(batch: Path, ident: dict) -> str | None:
    """The campaign's name: the one batch.json records, else -- for a batch from before
    campaigns were recorded -- the directory holding its ``eval-runs/``."""
    if ident.get("campaign"):
        return Path(ident["campaign"]).name
    batch = Path(batch).resolve()
    if batch.parent.name == paths.RUNS_DIRNAME:
        return batch.parent.parent.name
    return None


def _strip_paths(obj):
    """A copied record's values that are whole absolute paths (a baseline batch's
    directory) reduced to their last component: the batch's name."""
    if isinstance(obj, dict):
        return {k: _strip_paths(v) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_strip_paths(v) for v in obj]
    if isinstance(obj, str) and obj.startswith("/"):
        return Path(obj).name
    return obj


# Local-machine filesystem paths inside free text (a judge's reasons, a recorded reason):
# a path under one of these roots -- or under the owner's own home -- is replaced by
# "<path>". Only these: a route (/api/v1/users), a fraction (2 /3/4) and any other
# slash-rooted text stay. The root must start where a path can (not after a name
# character, "~", ">" -- ``<workspace>/x`` stays -- or "/", so a URL's path stays) and
# end at a "/" or where a path cannot go on (``/tmpfile`` is no /tmp). A colon is an
# ordinary path character (``/tmp/a:b/x`` redacts whole), so the match never ends on a
# bare trailing ":" (backtracks instead, keeping "...a.go: fine" outside); a numeric
# ``:line[:col]`` right at the match's end is trimmed back off it after the fact, so it
# stays outside the redaction (``redact_paths``). A trailing "." (the end of a sentence)
# is excluded the same way.
LOCAL_PATH_ROOTS = ("/home", "/Users", "/tmp", "/private", "/root", "/sandbox", "/var/folders", "/run/user")
_COMP = r"[^\s/'\"`<>()\[\]{},;|*?]"
_NOT_AFTER = r"(?<![\w.~$/>\-])"
PATH_PLACEHOLDER = "<path>"
_TRAILING_LINE_COL = re.compile(r":\d+(?::\d+)?$")


@functools.lru_cache(maxsize=8)
def path_token(home: str | None = None) -> re.Pattern:
    """The local-path pattern: LOCAL_PATH_ROOTS and ``home`` (default: the owner's)."""
    roots = set(LOCAL_PATH_ROOTS)
    h = (str(Path.home()) if home is None else home).rstrip("/")
    if h:
        roots.add(h)
    alt = "|".join(re.escape(r) for r in sorted(roots, key=len, reverse=True))
    return re.compile(rf"{_NOT_AFTER}(?:{alt})(?!{_COMP})(?:/{_COMP}*)*(?<!\.)(?<!:)")


def redact_paths(text: str, home: str | None = None) -> tuple[str, int]:
    """``text`` with every local filesystem path replaced by ``<path>``, a trailing
    numeric ``:line`` or ``:line:col`` kept outside the placeholder (any other colon is
    part of the redacted path), and how many paths were redacted."""
    def repl(m: re.Match) -> str:
        tail = _TRAILING_LINE_COL.search(m.group(0))
        return PATH_PLACEHOLDER + (m.group(0)[tail.start():] if tail else "")

    return path_token(home).subn(repl, text)


def _scrub(obj, counter: list[int], home: str | None = None):
    """Every string of a record (dict keys too) with its local paths redacted: the
    judge's reasons and any other free text are kept, never a path (plan 029 W4)."""
    if isinstance(obj, dict):
        return {_scrub(k, counter, home): _scrub(v, counter, home) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_scrub(v, counter, home) for v in obj]
    if isinstance(obj, str):
        text, n = redact_paths(obj, home)
        counter[0] += n
        return text
    return obj


def batch_key(batch) -> str:
    """A batch's identity inside one archive run: its resolved directory, so two
    campaigns' batches that share a name never share provenance (only names are
    written out)."""
    return str(Path(batch).resolve())


def craze_build(ident: dict, manifest: dict) -> dict | None:
    """The craze binary a batch ran: its basename, sha256 prefix, version, and the
    source commit it was built from (manifest.json records the repository's HEAD)."""
    ex = (ident.get("executables") or {}).get("craze")
    if not ex:
        return None
    man = (manifest.get("executables") or {}).get("craze") or {}
    return {"binary": _base(man.get("path")), "sha256": _prefix(ex.get("sha256")), "version": ex.get("version"),
            "source_commit": man.get("repo_head") or None, "source_dirty": man.get("repo_dirty")}


def batch_info(batch: Path) -> dict:
    """One batch's provenance, from its batch.json and manifest.json (nothing else)."""
    batch = Path(batch)
    ident = _read_json(batch / "batch.json")
    manifest = _read_json(batch / "manifest.json") or _read_json(batch / "manifest.resume.json")
    best = _read_json(batch / rp.BEST_OPEN_FILE)
    return {
        "name": batch.name,
        "campaign": batch_campaign(batch, ident),
        "label": ident.get("label"),
        "harnesses": ident.get("harnesses"),
        "models": sorted((ident.get("models") or {}).keys()),
        "reps": ident.get("reps"),
        "first_rep": ident.get("first_rep", 1),
        "prices": ident.get("prices"),
        "tasks": ident.get("tasks"),
        **({"craze_commits": ident["craze_commits"]} if ident.get("craze_commits") else {}),
        "fixtures": ident.get("fixtures"),
        "evaluator": ident.get("evaluator"),
        "config_snapshot": ident.get("config_snapshot"),
        "identity_fingerprint": ident.get("fingerprint"),
        "executables": {k: {"version": v.get("version"), "sha256": _prefix(v.get("sha256"))}
                        for k, v in sorted((ident.get("executables") or {}).items()) if isinstance(v, dict)},
        "craze_build": craze_build(ident, manifest),
        "best_open_harness": _strip_paths(best) if best else None,
        "has_calibration": (batch / "calibration" / CALIBRATION_FILE).is_file(),
    }


def calibration_summary(batch: Path, global_now: str) -> dict | None:
    """A batch's calibration decisions without its records (plan 029 W4 amendment)."""
    cal = _read_json(Path(batch) / "calibration" / CALIBRATION_FILE)
    if not cal:
        return None
    out = {k: cal.get(k) for k in ("seed", "judge_hash", "requested", "flip", "luna_agreement", "padding", "gates")}
    out["judge_hash_current"] = cal.get("judge_hash") == global_now
    return out


def run_row(r: dict, info: dict) -> dict:
    m = r.get("metrics") or {}
    h = r.get("harness")
    ex = (info.get("executables") or {}).get(h) or {}
    row = {
        "campaign": info.get("campaign"),
        "batch": info.get("name"),
        "run_key": r.get("run_key"),
        "run_id": r.get("run_id"),
        "harness": h,
        "harness_version": ex.get("version"),
        "harness_sha256": ex.get("sha256"),
        "model": r.get("model"),
        "wire_model": r.get("wire_model"),
        "served_models": m.get("served_models"),
        "task": r.get("task"),
        "split": r.get("split"),
        "category": r.get("category"),
        "mode": r.get("mode"),
        "plan_mode": r.get("plan_mode"),
        "rep": r.get("rep"),
        "status": r.get("status"),
        "objective_pass": r.get("objective_pass"),
        "checks": {str(c.get("name")): bool(c.get("passed")) for c in r.get("checks") or [] if isinstance(c, dict)},
        "answer_words": r.get("answer_words"),
        "tool_calls": m.get("tool_calls"),
        "main_requests": m.get("main_requests"),
        "wall_s": r.get("wall_s"),
        "cost": m.get("cost"),
        "list_cost": m.get("list_cost"),
        "tokens": m.get("tokens"),
    }
    if h == rp.NATIVE:
        row["craze_build"] = info.get("craze_build")
    return row


def _order(o: dict) -> dict:
    ov = o.get("verdict") if isinstance(o.get("verdict"), dict) else {}
    return {
        "judge_model": o.get("judge_model"),
        "x_is_a": o.get("x_is_a"),
        "winner": ov.get("winner"),
        "confidence": ov.get("confidence"),
        "score_x": ov.get("score_x"),
        "score_y": ov.get("score_y"),
        "rubric_x": ov.get("rubric_x"),
        "rubric_y": ov.get("rubric_y"),
        "reasons": ov.get("reasons"),
        "failed": not ov,
    }


def verdict_row(v: dict, current: bool, task_hashes: dict[str, str], global_now: str,
                task_unchanged: bool = False) -> tuple[dict, str | None]:
    """A final verdict's row and how its task judge hash was obtained: recorded;
    stamped -- a record from before per-task hashes, whose global hash is today's and
    whose task is still what its batch ran (``task_unchanged``: the rule of
    ``judge-hash --stamp``, since the global hash leaves out the task's mode); or None
    when it cannot be, with a note saying why."""
    res = v.get("result") or {}
    th, how, note = v.get("task_judge_hash"), "recorded", None
    if not th:
        th, how = None, None
        if not (v.get("judge_hash") and v.get("judge_hash") == global_now):
            note = NOTE_OTHER_HASH
        elif not task_unchanged or v.get("task") not in task_hashes:
            note = NOTE_TASK_CHANGED
        else:
            th, how = task_hashes[v["task"]], "stamped"
    esc = v.get("escalated_from") if isinstance(v.get("escalated_from"), dict) else None
    row = {
        "task": v.get("task"),
        "model": v.get("model"),
        "split": v.get("split"),
        "x_run_id": v.get("x_run_id"),
        "y_run_id": v.get("y_run_id"),
        "hx": v.get("hx"),
        "hy": v.get("hy"),
        "judge_model": v.get("judge_model"),
        "mode": v.get("judge_mode"),
        "both_orders": v.get("both_orders"),
        "seed": v.get("seed"),
        "winner": res.get("winner"),
        "agree": res.get("agree"),
        "confidence": res.get("confidence"),
        "score_x": res.get("score_x"),
        "score_y": res.get("score_y"),
        "orders": [_order(o) for o in v.get("orders") or [] if isinstance(o, dict)],
        "escalated_from": None if esc is None else {
            "judge_model": esc.get("judge_model"), **{k: (esc.get("result") or {}).get(k) for k in (
                "winner", "agree", "confidence", "score_x", "score_y")}},
        "judge_hash": v.get("judge_hash"),
        "task_judge_hash": th,
        "task_judge_hash_source": how,
        "current": current,
    }
    if note:
        row["task_judge_hash_note"] = note
    return row, how


def select_verdicts(records: list[dict], runs: list[dict], tasks: dict[str, Task]) -> dict:
    """The report's acceptance, binding and ranking over the raw verdict ``records``:
    those that still stand, deduplicated, bound to the scored ``runs``, one final
    verdict per pair -- then, for a pair with none, the best of its older verdicts
    (written with ``current`` false)."""
    cur, stale = rp.current_verdicts(records, tasks)
    bound_cur, unbound_cur = rp.bind_verdicts(rp.dedupe_verdicts(cur), runs)
    bound_old, unbound_old = rp.bind_verdicts(rp.dedupe_verdicts(stale), runs)
    final_cur = rp.final_verdicts(bound_cur)
    have = {rp.pair_key(v) for v in final_cur}
    final_old = [v for v in rp.final_verdicts(bound_old) if rp.pair_key(v) not in have]
    return {"bound": len(bound_cur) + len(bound_old), "unbound": unbound_cur + unbound_old,
            "final_current": final_cur, "final_old": final_old}


def _sort_key_run(r: dict) -> tuple:
    return tuple(str(r.get(k)) for k in ("split", "task", "model", "harness", "rep", "batch"))


def _sort_key_verdict(v: dict) -> tuple:
    return tuple(str(v.get(k)) for k in ("split", "task", "model", "hx", "hy", "x_run_id", "y_run_id"))


def _jsonl(rows: list[dict]) -> str:
    return "".join(json.dumps(r, sort_keys=True, ensure_ascii=False) + "\n" for r in rows)


def _json(obj) -> str:
    return json.dumps(obj, indent=2, sort_keys=True, ensure_ascii=False) + "\n"


def _count_lines(p: Path) -> int:
    return sum(1 for line in p.read_text(encoding="utf-8").splitlines() if line.strip())


def _home_needles(home: str | None) -> list[bytes]:
    needles = [b"/home/"]
    home = str(Path.home()) if home is None else home
    if home and home.rstrip("/") not in ("", "/"):
        needles.append(home.rstrip("/").encode())
    return needles


def safety_scan(out: Path, keyring: keymod.KeyRing, home: str | None = None) -> dict:
    """Every file under ``out``: the key scan the batch runner uses
    (``keys.scan_tree``), and a grep for the owner's home path and ``/home/``. Counts and
    relative paths only."""
    ks = keymod.scan_tree(out, keyring)
    needles = _home_needles(home)
    home_hits = [str(p.relative_to(out)) for p in sorted(Path(out).rglob("*"))
                 if p.is_file() and not p.is_symlink() and any(n in p.read_bytes() for n in needles)]
    return {"keys_checked": ks["keys_checked"], "files_scanned": ks["files_scanned"],
            "files_with_key": ks["files_with_key"], "skipped": ks["skipped"], "home_path_hits": home_hits,
            "clean": not ks["files_with_key"] and not home_hits and not ks["skipped"]}


def data_scan(out: Path, names: list[str], keyring: keymod.KeyRing, home: str | None = None) -> dict:
    """The same checks over the archive's own data files only -- what archive.json
    records, so a rerun into the same directory writes the same bytes."""
    needles = _home_needles(home)
    data = {n: (Path(out) / n).read_bytes() for n in names}
    return {"keys_checked": len(keyring.secrets()), "files": sorted(names),
            "files_with_key": sorted(n for n, b in data.items() if keyring.contains_key(b)),
            "home_path_hits": sorted(n for n, b in data.items() if any(x in b for x in needles))}


class ArchiveError(RuntimeError):
    """The archive cannot be written where asked."""


def check_out(out: Path, batches: list[Path]) -> None:
    """``out`` must be a new or empty directory, or an earlier archive (it holds
    archive.json) -- never any other non-empty directory, whatever it holds -- and never
    inside a batch: batches are read-only inputs."""
    out = Path(out)
    o = out.resolve()
    for b in batches:
        rb = Path(b).resolve()
        if o == rb or rb in o.parents:
            raise ArchiveError(f"--out {out} is inside the batch {b}: batches are read-only inputs")
    if not os.path.lexists(out):
        return
    if out.is_symlink() or not out.is_dir():
        raise ArchiveError(f"--out {out} is not a directory")
    if any(out.iterdir()) and not ((out / ARCHIVE_FILE).is_file() and not (out / ARCHIVE_FILE).is_symlink()):
        raise ArchiveError(f"--out {out} is not empty and holds no {ARCHIVE_FILE}: not an earlier archive")
    links = [n for n in OUTPUTS if (out / n).is_symlink()]
    if links:
        raise ArchiveError(f"--out {out}: {links} are symlinks")


def out_bytes(out: Path) -> int:
    """Every file under ``out`` (``lstat`` sizes, no link followed): the archive's
    outputs and anything else kept beside them."""
    return sum(p.lstat().st_size for p in Path(out).rglob("*") if p.is_symlink() or p.is_file())


def archive(batches: list[Path], out: Path, keyring: keymod.KeyRing, *, unseal: bool = False,
            max_bytes: int = DEFAULT_MAX_BYTES, tasks: dict[str, Task] | None = None,
            verdict_paths: list[Path] | None = None, home: str | None = None) -> dict:
    """Write the archive of ``batches`` into ``out`` and check it; the summary (its
    ``ok`` is false, with ``errors``, when a reconciliation, the scan or the size fails).
    ArchiveError when ``out`` is not a place an archive may be written (check_out)."""
    from crazeeval.tasks import load_tasks

    tasks = load_tasks() if tasks is None else tasks
    batches = [Path(b) for b in batches]
    out = Path(out)
    check_out(out, batches)
    out.mkdir(parents=True, exist_ok=True)
    global_now = judge_hash(tasks)
    th_now = task_judge_hashes(tasks)

    runs = rp.load_runs(batches, unseal)
    # Provenance by resolved directory: batches of two campaigns may share a name.
    keys = list(dict.fromkeys(batch_key(b) for b in batches))
    info = {k: batch_info(Path(k)) for k in keys}
    scored_runs = [r for r in runs if rp.scored(r)]
    redacted = [0]
    run_rows = sorted((_scrub(run_row(r, info[batch_key(r["batch"])]), redacted, home) for r in scored_runs),
                      key=_sort_key_run)

    vfiles = rp.verdict_files([Path(v) for v in verdict_paths] if verdict_paths else [b / "judging" for b in batches],
                              unseal)
    # Each record with the task fingerprints of the batch owning its file, for the
    # stamping rule (judging.stamp_task_hashes's).
    loaded, owner = [], {}
    for f in vfiles:
        batch_tasks = judging.owning_batch_tasks(f)
        for v in rp.read_verdicts([f], unseal):
            loaded.append(v)
            owner[id(v)] = batch_tasks
    sel = select_verdicts(loaded, runs, tasks)
    check = judging.TaskCheck(tasks)
    how_counts = {"recorded": 0, "stamped": 0, "null": 0}
    verdict_rows = []
    for current, group in ((True, sel["final_current"]), (False, sel["final_old"])):
        for v in group:
            unchanged = not v.get("task_judge_hash") and check.unchanged(v.get("task"), owner.get(id(v)))
            row, how = verdict_row(v, current, th_now, global_now, unchanged)
            how_counts[how or "null"] += 1
            verdict_rows.append(_scrub(row, redacted, home))
    verdict_rows.sort(key=_sort_key_verdict)

    calibration = [{"batch": info[k]["name"], "campaign": info[k]["campaign"], **s}
                   for k in keys if (s := calibration_summary(Path(k), global_now))]
    calibration = _scrub(calibration, redacted, home)
    batch_rows = _scrub([info[k] for k in keys], redacted, home)
    (out / RUNS_FILE).write_text(_jsonl(run_rows), encoding="utf-8")
    (out / VERDICTS_FILE).write_text(_jsonl(verdict_rows), encoding="utf-8")
    (out / BATCHES_FILE).write_text(_json({"batches": batch_rows}), encoding="utf-8")
    cal_path = out / CALIBRATION_FILE
    if calibration:
        cal_path.write_text(_json({"batches": calibration}), encoding="utf-8")
    elif cal_path.is_file():
        cal_path.unlink()  # an earlier archive's (check_out): no batch has calibration now

    n_final = len(sel["final_current"]) + len(sel["final_old"])
    rec = {
        "scored_runs_selected": len(scored_runs),
        "run_rows": _count_lines(out / RUNS_FILE),
        "final_verdicts": n_final,
        "final_verdicts_current": len(sel["final_current"]),
        "verdict_rows": _count_lines(out / VERDICTS_FILE),
    }
    rec["ok"] = rec["scored_runs_selected"] == rec["run_rows"] and rec["final_verdicts"] == rec["verdict_rows"]
    rec["line"] = (f"archive: {rec['scored_runs_selected']} scored runs selected, {rec['run_rows']} rows written; "
                   f"{rec['final_verdicts']} final verdicts ({rec['final_verdicts_current']} current), "
                   f"{rec['verdict_rows']} rows written -- {'reconciled' if rec['ok'] else 'MISMATCH'}")
    errors = [] if rec["ok"] else [rec["line"]]
    summary = _scrub({
        "batches": [info[k]["name"] for k in keys],
        "campaigns": sorted({i["campaign"] for i in info.values() if i.get("campaign")}),
        "unsealed": unseal,
        "judge_hash_now": global_now,
        "reconciliation": rec,
        "verdicts": {"records_read": len(loaded), "bound_to_scored_runs": sel["bound"], "unbound": sel["unbound"],
                     "final": n_final, "final_current": len(sel["final_current"]),
                     "final_not_current": len(sel["final_old"]), "task_judge_hash": how_counts},
        "calibration_batches": [c["batch"] for c in calibration],
        "paths_redacted": redacted[0],
        "left_out": LEFT_OUT,
        "max_bytes": max_bytes,
    }, [0], home)
    data_files = [n for n in OUTPUTS if n != ARCHIVE_FILE and (out / n).is_file()]
    summary["files"] = {n: (out / n).stat().st_size for n in data_files}
    summary["data_bytes"] = sum(summary["files"].values())
    # archive.json records the scan of the data files; the command then scans every
    # file in --out, archive.json and anything kept beside it included, before it finishes.
    summary["data_scan"] = data_scan(out, data_files, keyring, home)
    (out / ARCHIVE_FILE).write_text(_json(summary), encoding="utf-8")
    final_scan = safety_scan(out, keyring, home)
    total = out_bytes(out)  # the whole of --out: an earlier archive's extra files count too
    summary["total_bytes"] = total
    summary["scan"] = final_scan
    if final_scan["files_with_key"]:
        errors.append(f"key scan: a loaded key in {final_scan['files_with_key']}")
    if final_scan["home_path_hits"]:
        errors.append(f"home path scan: the owner's home path or /home/ in {final_scan['home_path_hits']}")
    if final_scan["skipped"]:
        errors.append(f"scan skipped {final_scan['skipped']}")
    if total > max_bytes:
        errors.append(f"size: {total} bytes over the {max_bytes}-byte budget")
    summary["errors"] = errors
    summary["ok"] = not errors
    return summary
