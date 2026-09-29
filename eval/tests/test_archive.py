"""``crazeeval archive`` (plan 029 W2): rows reconcile with the report's selection, the
held-out part stays sealed, no answer, capture, command line or path gets out, a planted
key or home path fails the scan, the size budget holds, and a fresh campaign archives."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from fakes import ZAI_KEY, keyring

from crazeeval import archive as arch
from crazeeval import paths
from crazeeval import report as rp
from crazeeval.judge import judge_hash, task_judge_hash
from crazeeval.tasks import Task

M = "m-1"
SPLITS = {"D1": "dev", "D2": "dev", "H1": "heldout"}
ANSWER = "THE-ANSWER-TEXT-never-archived"
RAW_CAL = "RAW-CALIBRATION-RECORD"
TASK_ROOT: Path | None = None  # each task's directory (its fingerprint), set per module


@pytest.fixture(autouse=True, scope="module")
def _task_dirs(tmp_path_factory):
    global TASK_ROOT
    TASK_ROOT = tmp_path_factory.mktemp("tasks")
    for t in SPLITS:
        (TASK_ROOT / t).mkdir()
        (TASK_ROOT / t / "task.toml").write_text(f'id = "{t}"\n')


def _tasks() -> dict[str, Task]:
    return {t: Task(id=t, dir=TASK_ROOT / t, category="explain", split=s, mode="build", prompt=f"Explain {t}.",
                    repo_kind="fixture", fixture="x", setup_patch=None, timeout_s=1, rubric=["r1", "r2", "r3"],
                    checks=[], validate={}, false_claims=["f"])
            for t, s in SPLITS.items()}


def _fingerprints() -> dict[str, str]:
    from crazeeval.batch import task_fingerprint

    return {t: task_fingerprint(task) for t, task in _tasks().items()}


def _run(batch: Path, h: str, task: str, rep: int = 1, status: str = "ok", passed: bool = True) -> dict:
    key = f"{h}/{M}/{task}/rep{rep}"
    return {"harness": h, "model": M, "wire_model": "wire-1", "task": task, "rep": rep, "status": status,
            "objective_pass": passed, "split": SPLITS[task], "category": "explain", "mode": "build",
            "plan_mode": None, "run_key": key, "run_id": f"{batch.name}:{key}:a1", "answer": ANSWER,
            "answer_words": 4, "command": [h, "--prompt", "<prompt>"], "notes": [f"{batch}/runs/x.log"],
            "checks": [{"name": "facts", "type": "facts", "passed": passed}, {"name": "no_writes", "passed": True}],
            "metrics": {"main_requests": 3, "tool_calls": 5, "cost": 0.01, "list_cost": 0.02,
                        "tokens": {"input": 100, "output": 10}, "served_models": ["wire-1"]},
            "wall_s": 12.5}


def _verdict(x: dict, y: dict, winner: str, judge: str = "sol", both: bool = True, reasons: str = "item 2 decided it",
             task_hash: str | None = "auto", global_hash: str | None = "auto", escalated: bool = False) -> dict:
    tasks = _tasks()
    orders = []
    for x_is_a in ((True, False) if both else (True,)):
        orders.append({"judge_model": judge, "x_is_a": x_is_a, "raw": {"winner": "A"}, "error": None,
                       "verdict": {"winner": winner, "confidence": "high", "score_x": 6, "score_y": 8,
                                   "rubric_x": [{"item": 1, "grade": "met"}, {"item": 2, "grade": "missed"},
                                                {"item": 3, "grade": "false"}],
                                   "rubric_y": [{"item": 1, "grade": "met"}, {"item": 2, "grade": "met"},
                                                {"item": 3, "grade": "missed"}],
                                   "reasons": reasons}})
    v = {"pair": f"{x['task']}|{M}|{x['run_id']}|{y['run_id']}", "task": x["task"], "model": M,
         "x": x["run_key"], "y": y["run_key"], "x_run_id": x["run_id"], "y_run_id": y["run_id"],
         "hx": x["harness"], "hy": y["harness"], "split": SPLITS[x["task"]], "judge_model": judge,
         "both_orders": both, "judge_mode": "success-bar", "seed": 29, "orders": orders,
         "result": {"winner": winner, "agree": True, "confidence": "high", "score_x": 6, "score_y": 8},
         "calibration": {"file": "/home/someone/plan/calibration.json", "status": "complete"},
         "judge_hash": judge_hash(tasks) if global_hash == "auto" else global_hash}
    if task_hash == "auto":
        v["task_judge_hash"] = task_judge_hash(tasks[x["task"]])
    elif task_hash is not None:
        v["task_judge_hash"] = task_hash
    if escalated:
        v["escalated_from"] = {"judge_model": "sol", "result": {"winner": "tie", "agree": False, "confidence": "low",
                                                                  "score_x": 7, "score_y": 7}}
    return v


def _write(batch: Path, runs: list[dict], verdicts: list[dict], ident: dict) -> Path:
    for r in runs:
        d = batch / ("heldout" if r["split"] == "heldout" else "runs") / r["harness"] / M / r["task"] / f"rep{r['rep']}"
        d.mkdir(parents=True)
        (d / "result.json").write_text(json.dumps(r))
    for v in verdicts:
        p = batch / "judging" / ("heldout" if v["split"] == "heldout" else "") / "verdicts.jsonl"
        p.parent.mkdir(parents=True, exist_ok=True)
        with open(p, "a") as f:
            f.write(json.dumps(v) + "\n")
    (batch / "batch.json").write_text(json.dumps(ident))
    (batch / "manifest.json").write_text(json.dumps({"executables": {"craze": {
        "path": str(batch.parent / "bin" / "craze"), "sha256": ident["executables"]["craze"]["sha256"],
        "repo_head": "73ed5e0" + "0" * 33, "repo_dirty": False}}}))
    return batch


def _ident(label: str, harnesses: list[str], campaign: str | None, craze_sha: str) -> dict:
    out = {"label": label, "harnesses": harnesses, "reps": 1, "models": {M: {"key": M}},
           "prices": {"wire-1": {"input": 1.0, "output": 2.0}}, "tasks": _fingerprints(),
           "fixtures": {"x": "fx-hash"}, "evaluator": "eval-hash", "config_snapshot": "snap-hash",
           "executables": {"craze": {"sha256": craze_sha, "version": "0.0.1"},
                           "gx": {"sha256": "ab" * 32, "version": "grok 1.0.16+gx.12"},
                           "opencode": {"sha256": "cd" * 32, "version": "1.2.3"}},
           "fingerprint": f"fp-{label}"}
    if campaign:
        out["campaign"] = campaign
    return out


@pytest.fixture
def world(tmp_path):
    """A baseline batch (all three harnesses) and a final batch (craze again, rep2 too),
    laid out as a campaign's eval-runs/ -- the baseline's batch.json names its campaign,
    the final's (an older batch) does not."""
    runs_dir = tmp_path / "camp-a" / "eval-runs"
    base, final = runs_dir / "base-x", runs_dir / "final-x"
    b = {(h, t): _run(base, h, t) for h in ("craze", "gx", "opencode") for t in SPLITS}
    b[("gx", "D2")] = _run(base, "gx", "D2", status="infra")  # unscored: no row, binds nothing
    f = {t: _run(final, "craze", t) for t in SPLITS}
    f2 = _run(final, "craze", "D1", rep=2, passed=False)
    base_verdicts = [
        _verdict(b[("craze", "D1")], b[("gx", "D1")], "x", both=False),  # on a replaced run: unbound
        _verdict(b[("gx", "D1")], b[("opencode", "D1")], "y"),
    ]
    final_verdicts = [
        _verdict(f["D1"], b[("gx", "D1")], "y", reasons="craze missed item 2"),
        _verdict(f["D1"], b[("gx", "D1")], "x", judge="astra", escalated=True),  # the final one of that pair
        _verdict(f["D2"], b[("opencode", "D2")], "y", task_hash=None),  # older record: stamped
        _verdict(f2, b[("opencode", "D1")], "x", task_hash=None, global_hash="OLD"),  # not current: null
        _verdict(f["H1"], b[("gx", "H1")], "y"),
        _verdict(f["D2"], b[("gx", "D2")], "x"),  # gx D2 is infra: unbound
    ]
    _write(base, list(b.values()), base_verdicts,
           _ident("base-x", ["codex", "craze", "gx", "opencode"], str(tmp_path / "camp-a"), "11" * 32))
    _write(final, list(f.values()) + [f2], final_verdicts, _ident("final-x", ["craze"], None, "22" * 32))
    (base / rp.BEST_OPEN_FILE).write_text(json.dumps({"baseline_batch": str(base), "models": {
        M: {"harness": "opencode", "reason": "tied counts", "counts": {"gx": 2, "opencode": 2},
            "baseline_batch": str(base)}}}))
    (base / "calibration").mkdir()
    (base / "calibration" / "calibration.json").write_text(json.dumps({
        "seed": 29, "judge_hash": judge_hash(_tasks()), "requested": {"pairs": 30},
        "flip": {"rate": 0.1, "complete": True}, "luna_agreement": {"rate": 0.9}, "padding": {"padded_preferred": 0},
        "gates": {"flip_gate": "pass", "luna_gate": "pass", "padding_gate": "pass", "complete": True}}))
    (base / "calibration" / "sol.jsonl").write_text(json.dumps({"raw": RAW_CAL}) + "\n")
    return base, final


def _archive(world, out: Path, **kw) -> dict:
    return arch.archive(list(world), out, keyring(), tasks=_tasks(), **kw)


def _files_text(out: Path) -> str:
    return "\n".join(p.read_text() for p in sorted(out.iterdir()) if p.is_file())


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


def _load(out: Path, name: str):
    p = out / name
    return [json.loads(line) for line in p.read_text().splitlines()] if name.endswith(".jsonl") else json.loads(
        p.read_text())


def test_rows_reconcile_and_carry_the_record(world, tmp_path, capsys):
    out = tmp_path / "arch"
    s = _archive(world, out, unseal=True)
    assert s["ok"], s["errors"]
    rec = s["reconciliation"]
    runs = rp.load_runs(list(world), unseal=True)
    assert rec["scored_runs_selected"] == len([r for r in runs if rp.scored(r)]) == rec["run_rows"] == 9
    assert rec["final_verdicts"] == rec["verdict_rows"] == 5 and rec["final_verdicts_current"] == 4 and rec["ok"]
    assert "reconciled" in rec["line"] and _load(out, arch.ARCHIVE_FILE)["reconciliation"] == rec
    rows = _load(out, arch.RUNS_FILE)
    craze = next(r for r in rows if r["run_key"] == f"craze/{M}/D1/rep1")
    assert craze["batch"] == "final-x" and craze["campaign"] == "camp-a"  # inferred from the layout
    assert craze["craze_build"] == {"binary": "craze", "sha256": "22" * 6, "version": "0.0.1",
                                    "source_commit": "73ed5e0" + "0" * 33, "source_dirty": False}
    assert craze["checks"] == {"facts": True, "no_writes": True} and craze["tokens"] == {"input": 100, "output": 10}
    assert craze["served_models"] == ["wire-1"] and craze["list_cost"] == 0.02 and craze["wall_s"] == 12.5
    gx = next(r for r in rows if r["run_key"] == f"gx/{M}/D1/rep1")
    assert gx["campaign"] == "camp-a" and gx["harness_version"] == "grok 1.0.16+gx.12" and "craze_build" not in gx
    assert gx["harness_sha256"] == "ab" * 6
    assert not any(r["run_key"] == f"gx/{M}/D2/rep1" for r in rows)  # infra: not scored
    verdicts = _load(out, arch.VERDICTS_FILE)
    d1 = next(v for v in verdicts if v["task"] == "D1" and v["hy"] == "gx")
    assert d1["judge_model"] == "astra" and d1["winner"] == "x" and d1["escalated_from"]["winner"] == "tie"
    assert d1["orders"][0]["rubric_x"][1] == {"item": 2, "grade": "missed"} and d1["orders"][0]["reasons"]
    assert d1["task_judge_hash_source"] == "recorded" and d1["current"]
    stamped = next(v for v in verdicts if v["task"] == "D2")
    assert stamped["task_judge_hash"] == task_judge_hash(_tasks()["D2"]) and stamped["task_judge_hash_source"] == "stamped"
    old = next(v for v in verdicts if v["x_run_id"].endswith("D1/rep2:a1"))
    assert old["task_judge_hash"] is None and old["current"] is False and "cannot be recomputed" in old["task_judge_hash_note"]
    assert s["verdicts"]["task_judge_hash"] == {"recorded": 3, "stamped": 1, "null": 1}
    batches = _load(out, arch.BATCHES_FILE)["batches"]
    assert [b["name"] for b in batches] == ["base-x", "final-x"] and batches[0]["campaign"] == "camp-a"
    assert batches[0]["best_open_harness"]["baseline_batch"] == "base-x"
    assert batches[0]["best_open_harness"]["models"][M]["baseline_batch"] == "base-x"
    assert batches[0]["tasks"] == _fingerprints() and batches[0]["evaluator"] == "eval-hash"
    assert batches[1]["craze_build"]["sha256"] == "22" * 6 and batches[0]["prices"] == {"wire-1": {"input": 1.0,
                                                                                               "output": 2.0}}
    cal = _load(out, arch.CALIBRATION_FILE)["batches"]
    assert [(c["batch"], c["campaign"]) for c in cal] == [("base-x", "camp-a")]
    assert cal[0]["gates"]["flip_gate"] == "pass" and cal[0]["judge_hash_current"] is True
    text = _files_text(out)
    for gone in (ANSWER, RAW_CAL, "--prompt", "x.log", "/home/someone", str(tmp_path), "/home/"):
        assert gone not in text, gone
    assert s["scan"]["clean"] and s["total_bytes"] <= arch.DEFAULT_MAX_BYTES


def test_an_old_record_is_stamped_only_while_its_task_is_what_its_batch_ran(world, tmp_path):
    """The archive stamps an old record (global hash only) by judge-hash --stamp's rule:
    the global hash is today's and the task's fingerprint equals its batch's."""
    _, final = world
    ident = json.loads((final / "batch.json").read_text())
    ident["tasks"]["D2"] = "fingerprint-before-a-mode-change"
    (final / "batch.json").write_text(json.dumps(ident))
    out = tmp_path / "arch"
    s = _archive(world, out, unseal=True)
    assert s["ok"] and s["verdicts"]["task_judge_hash"] == {"recorded": 3, "stamped": 0, "null": 2}
    d2 = next(v for v in _load(out, arch.VERDICTS_FILE) if v["task"] == "D2")
    assert d2["task_judge_hash"] is None and d2["task_judge_hash_note"] == arch.NOTE_TASK_CHANGED
    assert d2["current"]  # the report's legacy fallback still counts it (its documented blind spot)


def test_no_absolute_path_in_any_output(world, tmp_path):
    out = tmp_path / "arch"
    assert _archive(world, out, unseal=True)["ok"]
    for name in (arch.RUNS_FILE, arch.VERDICTS_FILE, arch.BATCHES_FILE, arch.CALIBRATION_FILE, arch.ARCHIVE_FILE):
        docs = _load(out, name)
        for s in _strings(docs):
            assert not s.startswith("/") and str(tmp_path) not in s, (name, s)


def test_held_out_stays_out_without_unseal(world, tmp_path):
    sealed = _archive(world, tmp_path / "sealed")
    assert sealed["ok"], sealed["errors"]
    rows = _load(tmp_path / "sealed", arch.RUNS_FILE)
    assert {r["task"] for r in rows} == {"D1", "D2"}
    for name in (arch.RUNS_FILE, arch.VERDICTS_FILE):  # batches.json lists every task's fingerprint
        assert "H1" not in (tmp_path / "sealed" / name).read_text()
    assert all(v["split"] == "dev" for v in _load(tmp_path / "sealed", arch.VERDICTS_FILE))
    opened = _archive(world, tmp_path / "open", unseal=True)
    assert {r["task"] for r in _load(tmp_path / "open", arch.RUNS_FILE)} == {"D1", "D2", "H1"}
    assert opened["reconciliation"]["run_rows"] == sealed["reconciliation"]["run_rows"] + 3


def test_verdict_binding_matches_the_reports(world, tmp_path):
    out = tmp_path / "arch"
    assert _archive(world, out, unseal=True)["ok"]
    tasks = _tasks()
    runs = rp.load_runs(list(world), unseal=True)
    verdicts, stale = rp.load_current_verdicts([b / "judging" for b in world], tasks, unseal=True)
    rep = rp.build(rp.ReportInput(runs=runs, verdicts=verdicts, tasks=tasks, unseal=True, stale_verdicts=len(stale)))
    bound, _ = rp.bind_verdicts(verdicts, runs)
    report_final = {(v["x_run_id"], v["y_run_id"], v["judge_model"]) for v in rp.final_verdicts(bound)}
    rows = _load(out, arch.VERDICTS_FILE)
    assert {(v["x_run_id"], v["y_run_id"], v["judge_model"]) for v in rows if v["current"]} == report_final
    assert rep["verdicts_used"]["final"] == len(report_final) == 4 and rep["verdicts_used"]["stale"] == 1


def test_a_planted_key_or_home_path_fails_the_scan(world, tmp_path):
    base, final = world
    with open(final / "judging" / "verdicts.jsonl", "a") as f:
        runs = {r["run_key"]: r for r in rp.load_runs([base, final], unseal=True)}
        f.write(json.dumps(_verdict(runs[f"craze/{M}/D2/rep1"], runs[f"craze/{M}/D1/rep2"] | {"task": "D2"}, "x",
                                    reasons=f"the answer printed {ZAI_KEY}")) + "\n")
    out = tmp_path / "arch"
    s = _archive(world, out, unseal=True)
    assert not s["ok"] and any("key scan" in e and arch.VERDICTS_FILE in e for e in s["errors"]), s["errors"]
    assert (out / arch.VERDICTS_FILE).exists() and (out / arch.ARCHIVE_FILE).exists()  # nothing deleted
    assert ZAI_KEY not in (out / arch.ARCHIVE_FILE).read_text()


@pytest.mark.parametrize("text,want", [
    ("ran /tmp/pytest-of-x/test0/ws/a.py and saw it.", "ran <path> and saw it."),
    ("see /home/someone/.craze/native/providers.toml", "see <path>"),
    ("/tmp/foo.py:12 and /tmp/foo.py:12:5 failed", "<path>:12 and <path>:12:5 failed"),
    ("/tmp/foo.py:12", "<path>:12"),
    ("/tmp/foo.py:12:5", "<path>:12:5"),
    ("/tmp/a:b/x", "<path>"),  # a colon with no trailing line[:col] is part of the path
    ("(/Users/bob/src/x.go:12) and `/private/var/folders/q/T/y`", "(<path>:12) and `<path>`"),
    ("/Users/x/y.go", "<path>"),
    ("under /sandbox/work/craze/internal/a.go: fine", "under <path>: fine"),
    ("the dir /tmp, then /home", "the dir <path>, then <path>"),
    ("/var/folders/ab/T/x and /run/user/1000/bus and /root/.ssh", "<path> and <path> and <path>"),
    ("/api/v1/users", None),
    ("2 /3/4", None),
    ("/usr/local/bin/craze, /var/log/syslog and /etc/hosts", None),
    ("/tmpfile/x and /homes/y", None),
    ("<workspace>/internal/harness/tools.go:290 and internal/a.go", None),
    ("items 2/3, a 7/10, and/or https://github.com/a/b and ~/.craze/native", None),
    ("http://localhost:8080/tmp/x and the /help command", None),
])
def test_local_paths_in_free_text_are_redacted(text, want):
    got, n = arch.redact_paths(text, home="/home/owner")
    assert got == (text if want is None else want) and (n > 0) == (want is not None)


def test_the_owners_own_home_is_redacted_wherever_it_is():
    assert arch.redact_paths("under /data/users/bob/src/a.go:3", home="/data/users/bob") == ("under <path>:3", 1)
    assert arch.redact_paths("under /data/users/bob/src/a.go:3", home="/home/bob")[1] == 0
    assert arch.redact_paths("/data/users/bobby/x", home="/data/users/bob")[1] == 0


def test_the_judges_reasons_lose_their_paths(world, tmp_path):
    """Reasons are kept verbatim but for local paths: a /tmp/... path is no home path,
    so only the redaction keeps it out; its line number stays."""
    base, _ = world
    p = base / "judging" / "verdicts.jsonl"
    lines = p.read_text().splitlines()
    rec = json.loads(lines[1])
    rec["orders"][0]["verdict"]["reasons"] = (
        f"A ran pytest in {tmp_path}/ws/test_a.py:12; B read /home/someone/.craze/native/providers.toml; "
        "item 2; GET /api/v1/users")
    p.write_text("\n".join([lines[0], json.dumps(rec)]) + "\n")
    out = tmp_path / "arch"
    s = _archive(world, out, unseal=True)
    assert s["ok"], s["errors"]
    reasons = next(v for v in _load(out, arch.VERDICTS_FILE) if v["hx"] == "gx")["orders"][0]["reasons"]
    assert reasons == "A ran pytest in <path>:12; B read <path>; item 2; GET /api/v1/users"
    assert s["paths_redacted"] == 2 and _load(out, arch.ARCHIVE_FILE)["paths_redacted"] == 2
    text = _files_text(out)
    assert str(tmp_path) not in text and "/tmp/" not in text and "/home/" not in text


def test_the_scan_is_the_backstop_over_everything_in_out(world, tmp_path, monkeypatch):
    # With the redaction off, a home path in the reasons reaches verdicts.jsonl: the scan fails.
    base, _ = world
    p = base / "judging" / "verdicts.jsonl"
    lines = p.read_text().splitlines()
    rec = json.loads(lines[1])
    rec["orders"][0]["verdict"]["reasons"] = "it read /home/someone/.craze/native/providers.toml"
    p.write_text("\n".join([lines[0], json.dumps(rec)]) + "\n")
    with monkeypatch.context() as mp:
        mp.setattr(arch, "redact_paths", lambda text, home=None: (text, 0))
        s = _archive(world, tmp_path / "arch", unseal=True)
    assert not s["ok"] and s["scan"]["home_path_hits"] == [arch.VERDICTS_FILE]
    # A file kept beside an earlier archive is scanned too.
    out = tmp_path / "arch2"
    assert _archive(world, out, unseal=True)["ok"]
    (out / "notes").mkdir()
    (out / "notes" / "README.md").write_text("raw runs: /home/someone/runs\n")
    s = _archive(world, out, unseal=True)
    assert not s["ok"] and s["scan"]["home_path_hits"] == ["notes/README.md"]


def test_the_size_budget(world, tmp_path):
    s = _archive(world, tmp_path / "arch", unseal=True, max_bytes=500)
    assert not s["ok"] and any("over the 500-byte budget" in e for e in s["errors"])
    assert (tmp_path / "arch" / arch.RUNS_FILE).exists()
    # Every file in --out counts, an earlier archive's extras included.
    out = tmp_path / "arch2"
    first = _archive(world, out, unseal=True)
    assert first["ok"] and first["total_bytes"] == sum(p.stat().st_size for p in out.iterdir())
    (out / "README.md").write_text("x" * 5000)
    budget = first["total_bytes"] + 1000
    s = _archive(world, out, unseal=True, max_bytes=budget)
    assert s["total_bytes"] == sum(p.stat().st_size for p in out.iterdir()) > s["data_bytes"] + 5000
    assert not s["ok"] and any(f"over the {budget}-byte budget" in e for e in s["errors"])


def test_out_is_a_new_directory_or_an_earlier_archive(world, tmp_path):
    base, final = world
    before = (base / "judging" / "verdicts.jsonl").read_bytes()
    for inside in (final / "archive", base / "judging"):
        with pytest.raises(arch.ArchiveError, match="read-only inputs"):
            _archive(world, inside)
    assert not (final / "archive").exists() and (base / "judging" / "verdicts.jsonl").read_bytes() == before
    # Any other non-empty directory -- a verdict directory kept elsewhere, or one holding
    # none of our names -- is refused, and left as it was.
    for i, name in enumerate(("verdicts.jsonl", "README.md", "sub/x.txt")):
        other = tmp_path / f"other{i}"
        (other / name).parent.mkdir(parents=True)
        (other / name).write_text("{}\n")
        with pytest.raises(arch.ArchiveError, match="not an earlier archive"):
            _archive(world, other)
        assert sorted(p.name for p in other.rglob("*") if p.is_file()) == [Path(name).name]
    empty = tmp_path / "empty"
    empty.mkdir()
    assert _archive(world, empty)["ok"]
    link = tmp_path / "link"
    link.symlink_to(empty, target_is_directory=True)
    with pytest.raises(arch.ArchiveError, match="not a directory"):
        _archive(world, link)
    # Again into an earlier archive: the same bytes (a diffable record), a README kept.
    out = tmp_path / "arch"
    assert _archive(world, out, unseal=True)["ok"]
    (out / "README.md").write_text("# results\n")
    first = {p.name: p.read_bytes() for p in out.iterdir()}
    assert _archive(world, out, unseal=True)["ok"]
    assert {p.name: p.read_bytes() for p in out.iterdir()} == first


def test_two_campaigns_batches_that_share_a_name_keep_their_own_provenance(tmp_path):
    """Provenance is looked up by the resolved batch directory, not its name."""
    b1 = tmp_path / "camp-a" / "eval-runs" / "smoke-1"
    b2 = tmp_path / "camp-b" / "eval-runs" / "smoke-1"
    _write(b1, [_run(b1, "craze", "D1")], [], _ident("smoke-1", ["craze"], None, "aa" * 32))
    _write(b2, [_run(b2, "craze", "D2")], [], _ident("smoke-1", ["craze"], str(tmp_path / "camp-b"), "bb" * 32))
    out = tmp_path / "arch"
    s = arch.archive([b1, b2], out, keyring(), tasks=_tasks())
    assert s["ok"], s["errors"]
    rows = {r["task"]: r for r in _load(out, arch.RUNS_FILE)}
    assert (rows["D1"]["campaign"], rows["D1"]["craze_build"]["sha256"]) == ("camp-a", "aa" * 6)
    assert (rows["D2"]["campaign"], rows["D2"]["craze_build"]["sha256"]) == ("camp-b", "bb" * 6)
    assert rows["D1"]["batch"] == rows["D2"]["batch"] == "smoke-1"
    batches = _load(out, arch.BATCHES_FILE)["batches"]
    assert [(b["name"], b["campaign"], b["craze_build"]["sha256"]) for b in batches] == [
        ("smoke-1", "camp-a", "aa" * 6), ("smoke-1", "camp-b", "bb" * 6)]
    assert s["campaigns"] == ["camp-a", "camp-b"]


def test_all_runs_keeps_both_batches_run_key_and_binds_the_craze_vs_craze_verdict(tmp_path):
    """Two batches with the same run key (a lever's craze-vs-craze pair): the default
    keeps only the later batch's run and drops the verdict between them (its x side is
    no longer a scored run); --all-runs keeps both runs -- by their distinct run_id --
    and the verdict is bound and written. Reconciliation holds in both modes."""
    runs_dir = tmp_path / "camp-x" / "eval-runs"
    a, b = runs_dir / "batch-a", runs_dir / "batch-b"
    ra, rb = _run(a, "craze", "D1"), _run(b, "craze", "D1")
    assert ra["run_key"] == rb["run_key"] and ra["run_id"] != rb["run_id"]
    v = _verdict(ra, rb, "x")  # craze (batch-a) vs craze (batch-b), same task and run key
    _write(a, [ra], [], _ident("batch-a", ["craze"], None, "aa" * 32))
    _write(b, [rb], [v], _ident("batch-b", ["craze"], None, "bb" * 32))

    default = arch.archive([a, b], tmp_path / "default", keyring(), tasks=_tasks())
    assert default["ok"] and "reconciled" in default["reconciliation"]["line"]
    assert default["selection"] == arch.CURRENT_VIEW
    rows = _load(tmp_path / "default", arch.RUNS_FILE)
    assert [r["batch"] for r in rows] == ["batch-b"]  # the later batch wins the run key
    assert _load(tmp_path / "default", arch.VERDICTS_FILE) == []  # unbound: batch-a's run is gone

    allr = arch.archive([a, b], tmp_path / "all", keyring(), tasks=_tasks(), all_runs=True)
    assert allr["ok"], allr["errors"]
    assert "reconciled" in allr["reconciliation"]["line"]
    assert allr["selection"] == arch.ALL_RUNS
    rows = _load(tmp_path / "all", arch.RUNS_FILE)
    assert {r["batch"] for r in rows} == {"batch-a", "batch-b"} and len(rows) == 2
    verdicts = _load(tmp_path / "all", arch.VERDICTS_FILE)
    assert len(verdicts) == 1 and verdicts[0]["winner"] == "x" and verdicts[0]["current"]
    assert verdicts[0]["x_run_id"] == ra["run_id"] and verdicts[0]["y_run_id"] == rb["run_id"]


def test_all_runs_verdict_judged_in_two_files_still_yields_one_final_verdict(tmp_path):
    """The same pair judged in two verdict files (e.g. base and final batches' judging/)
    still yields one final verdict per pair, ranked as the report ranks them."""
    runs_dir = tmp_path / "camp-z" / "eval-runs"
    a, b = runs_dir / "batch-a", runs_dir / "batch-b"
    ra, rb = _run(a, "craze", "D1"), _run(b, "craze", "D1")
    v1 = _verdict(ra, rb, "y", judge="sol")
    v2 = _verdict(ra, rb, "x", judge="astra")  # a re-judgement: ranks above sol's
    _write(a, [ra], [v1], _ident("batch-a", ["craze"], None, "aa" * 32))
    _write(b, [rb], [v2], _ident("batch-b", ["craze"], None, "bb" * 32))
    out = tmp_path / "arch"
    s = arch.archive([a, b], out, keyring(), tasks=_tasks(), all_runs=True)
    assert s["ok"], s["errors"]
    verdicts = _load(out, arch.VERDICTS_FILE)
    assert len(verdicts) == 1 and verdicts[0]["judge_model"] == "astra" and verdicts[0]["winner"] == "x"


def test_duplicate_batch_arguments_dont_duplicate_rows(tmp_path):
    """Passing the same batch directory more than once reads it once, in either mode."""
    runs_dir = tmp_path / "camp-w" / "eval-runs"
    a = runs_dir / "batch-a"
    ra = _run(a, "craze", "D1")
    _write(a, [ra], [], _ident("batch-a", ["craze"], None, "aa" * 32))

    s = arch.archive([a, a, a], tmp_path / "dup-all", keyring(), tasks=_tasks(), all_runs=True)
    assert s["ok"], s["errors"]
    assert len(_load(tmp_path / "dup-all", arch.RUNS_FILE)) == 1
    assert len(_load(tmp_path / "dup-all", arch.BATCHES_FILE)["batches"]) == 1

    s2 = arch.archive([a, a], tmp_path / "dup-default", keyring(), tasks=_tasks())
    assert s2["ok"], s2["errors"]
    assert len(_load(tmp_path / "dup-default", arch.RUNS_FILE)) == 1
    assert len(_load(tmp_path / "dup-default", arch.BATCHES_FILE)["batches"]) == 1


def test_all_runs_same_batch_name_two_campaigns_run_id_collision_raises(tmp_path):
    """Two different campaigns whose batch directories share a name and a run key
    produce the same run_id (``<batch.name>:<run_key>:a<n>``) -- not a duplicate (they
    are two different directories, two different runs). Keeping one silently would drop
    the other's row with no trace; --all-runs must raise instead, naming both
    directories, rather than pick a winner."""
    a = tmp_path / "camp-x" / "eval-runs" / "batch-a"
    b = tmp_path / "camp-y" / "eval-runs" / "batch-a"
    ra, rb = _run(a, "craze", "D1"), _run(b, "craze", "D1")
    assert ra["run_id"] == rb["run_id"]  # same batch name + same run key -> same run_id
    _write(a, [ra], [], _ident("batch-a", ["craze"], "camp-x", "aa" * 32))
    _write(b, [rb], [], _ident("batch-a", ["craze"], "camp-y", "bb" * 32))

    with pytest.raises(arch.ArchiveError) as ei:
        arch.archive([a, b], tmp_path / "out", keyring(), tasks=_tasks(), all_runs=True)
    msg = str(ei.value)
    assert str(a.resolve()) in msg and str(b.resolve()) in msg and ra["run_id"] in msg


def test_all_runs_same_dir_given_twice_still_deduplicated(tmp_path):
    """A run_id collision is only raised across *different* resolved directories --
    the same batch directory passed twice is still deduplicated as before (already
    covered above by ``test_duplicate_batch_arguments_dont_duplicate_rows``), not
    treated as a collision -- restated here directly against the new collision check."""
    runs_dir = tmp_path / "camp-w" / "eval-runs"
    a = runs_dir / "batch-a"
    ra = _run(a, "craze", "D1")
    _write(a, [ra], [], _ident("batch-a", ["craze"], None, "aa" * 32))

    s = arch.archive([a, a], tmp_path / "same-dir-twice", keyring(), tasks=_tasks(), all_runs=True)
    assert s["ok"], s["errors"]
    assert len(_load(tmp_path / "same-dir-twice", arch.RUNS_FILE)) == 1


def test_a_reconciliation_mismatch_fails(world, tmp_path, monkeypatch):
    monkeypatch.setattr(arch, "_count_lines", lambda p: 0)
    s = _archive(world, tmp_path / "arch", unseal=True)
    assert not s["ok"] and "MISMATCH" in s["reconciliation"]["line"] and s["errors"][0] == s["reconciliation"]["line"]


def test_a_fresh_campaign_plans_runs_then_archives(tmp_path, monkeypatch, capsys):
    """HOME of the test's own, a new campaign: its batch goes under ~/.craze-eval/<name>,
    records the campaign, and archives -- nothing lands in a plan folder."""
    from crazeeval import keys as keymod
    from crazeeval.batch import BatchConfig, batch_identity, open_batch_dir, plan_runs, run_dir
    from crazeeval.cli import main
    from crazeeval.config import Snapshot, load_models
    from crazeeval.pricing import load_prices
    from crazeeval.tasks import load_tasks, select_tasks

    home = tmp_path / "home"
    home.mkdir()
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.delenv(paths.CAMPAIGN_ENV, raising=False)
    monkeypatch.delenv(paths.LEGACY_CAMPAIGN_ENV, raising=False)
    monkeypatch.setattr(keymod, "load_providers", lambda *a, **k: ({}, keyring()))
    try:
        paths.set_campaign("fresh")
        ms = load_models()
        cfg = BatchConfig(harnesses=["craze", "gx"], models=[ms["glm-5.3-flash"]],
                          tasks=select_tasks(load_tasks(), "smoke-explain,smoke-fix"), reps=1,
                          out=paths.runs_dir() / "smoke-1", label="smoke", snap=Snapshot(dir=tmp_path, config={},
                                                                                        hash="x"))
        assert cfg.ledger_path == home / ".craze-eval" / "fresh" / "ledger.jsonl"
        open_batch_dir(cfg, batch_identity(cfg, {"craze": {"sha256": "ee" * 32, "version": "dev"}},
                                           load_prices())).close()
        planned = plan_runs(cfg)
        for spec in planned:
            d = run_dir(cfg, spec)
            d.mkdir(parents=True)
            (d / "result.json").write_text(json.dumps({
                "harness": spec.harness, "model": spec.em.key, "wire_model": spec.em.wire_model, "task": spec.task.id,
                "split": spec.task.split, "rep": spec.rep, "run_key": spec.key, "run_id": f"smoke-1:{spec.key}:a1",
                "status": "ok", "objective_pass": True, "answer": ANSWER, "checks": [{"name": "facts", "passed": True}],
                "metrics": {"tool_calls": 1}}))
        paths.set_campaign(None)
        out = tmp_path / "arch"
        assert main(["--campaign", "fresh", "archive", "--batch", str(cfg.out), "--out", str(out)]) == 0
        printed = capsys.readouterr().out
        assert f"{len(planned)} scored runs selected, {len(planned)} rows written" in printed and "reconciled" in printed
        rows = _load(out, arch.RUNS_FILE)
        assert len(rows) == len(planned) and {r["campaign"] for r in rows} == {"fresh"}
        assert {r["batch"] for r in rows} == {"smoke-1"} and ANSWER not in _files_text(out)
        assert _load(out, arch.BATCHES_FILE)["batches"][0]["campaign"] == "fresh"
        assert json.loads((cfg.out / "batch.json").read_text())["campaign"] == str(home / ".craze-eval" / "fresh")
        assert not (home / ".claude").exists()
    finally:
        paths.set_campaign(None)
