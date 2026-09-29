"""``crazeeval judge-hash --stamp`` (plan 029 W2 reviews r1, r2): a record from before
per-task judge hashes gets its task's hash while its global hash is still today's and its
task is still what its batch ran, so it keeps standing once another task is added.
Idempotent, the original kept once (created exclusively), every other record's bytes
untouched, and no append lost to a stamp."""

from __future__ import annotations

import json
import threading
from pathlib import Path

import pytest

from crazeeval import judging as jg
from crazeeval import report as rp
from crazeeval.batch import task_fingerprint
from crazeeval.judge import judge_hash, task_judge_hash, task_judge_hashes, verdict_current
from crazeeval.judging import StampError, pre_stamp_path, stamp_task_hashes
from crazeeval.tasks import Task, load_task

TOML = """id = "{tid}"
category = "explain"
split = "dev"
mode = "{mode}"
prompt = "Explain it."
rubric = {rubric}
false_claims = ["a false claim"]

[repo]
kind = "fixture"
name = "smoke-py"

[[checks]]
type = "facts"
[[checks.items]]
regex = "x"
"""


def _write_task(root: Path, tid: str, rubric: list[str], mode: str = "build") -> Task:
    d = root / tid
    d.mkdir(parents=True, exist_ok=True)
    (d / "task.toml").write_text(TOML.format(tid=tid, mode=mode, rubric=json.dumps(rubric)))
    return load_task(d)


def _world(tmp_path: Path) -> tuple[dict[str, Task], Path]:
    """Tasks A and B, and a batch whose batch.json records their fingerprints."""
    root = tmp_path / "tasks"
    tasks = {"A": _write_task(root, "A", ["a1", "a2"]), "B": _write_task(root, "B", ["b1"])}
    batch = tmp_path / "batch"
    batch.mkdir()
    (batch / "batch.json").write_text(json.dumps({"tasks": {t: task_fingerprint(x) for t, x in tasks.items()}}))
    return tasks, batch


def _stamp_file(batch: Path, tasks: dict[str, Task], extra: list[str] = ()) -> tuple[Path, list[str]]:
    gh = judge_hash(tasks)
    lines = [
        json.dumps({"pair": "p1", "task": "A", "judge_hash": gh, "result": {"winner": "x"}}),  # stamped
        json.dumps({"pair": "p2", "task": "B", "judge_hash": gh, "task_judge_hash": "kept-as-is"}),  # already
        json.dumps({"pair": "p3", "task": "A", "judge_hash": "OLD", "result": {"winner": "y"}}),  # another hash
        json.dumps({"pair": "p4", "task": "A", "result": {"winner": "y"}}),  # no global hash
        json.dumps({"pair": "p5", "task": "gone", "judge_hash": gh}),  # a task that is gone
        "not json {",
        json.dumps({"pair": "p6", "task": "B", "judge_hash": gh}),  # stamped
        *extra,
    ]
    p = batch / "judging" / "verdicts.jsonl"
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(("\n".join(lines) + "\n").encode("utf-8"))
    return p, lines


WANT = {"records": 6, "stamped": 2, "already": 1, "other_hash": 1, "no_hash": 1, "task_changed_or_unknown": 1,
        "unparsed": 1}


def test_a_dry_run_counts_and_writes_nothing(tmp_path):
    tasks, batch = _world(tmp_path)
    p, _ = _stamp_file(batch, tasks)
    original = p.read_bytes()
    c = stamp_task_hashes(p, tasks, dry_run=True)
    assert {k: c[k] for k in WANT} == WANT and not c["written"] and c["original_kept"] is None
    assert p.read_bytes() == original and not pre_stamp_path(p).exists()
    assert not jg.lock_path(p).exists()  # a dry run takes no lock either


def test_stamp_once_idempotent_and_the_original_kept_once(tmp_path):
    tasks, batch = _world(tmp_path)
    p, lines = _stamp_file(batch, tasks)
    original = p.read_bytes()
    c = stamp_task_hashes(p, tasks)
    assert {k: c[k] for k in WANT} == WANT and c["written"] and c["original_kept"] == "verdicts.pre-stamp.jsonl"
    assert pre_stamp_path(p) == p.parent / "verdicts.pre-stamp.jsonl" and pre_stamp_path(p).read_bytes() == original
    got = p.read_text().split("\n")
    assert got[-1] == "" and len(got) == len(lines) + 1
    for i in (1, 2, 3, 4, 5):  # every record left alone keeps its bytes, the unparsed line too
        assert got[i] == lines[i]
    th = task_judge_hashes(tasks)
    assert json.loads(got[0]) == {**json.loads(lines[0]), "task_judge_hash": th["A"]}
    assert json.loads(got[6]) == {**json.loads(lines[6]), "task_judge_hash": th["B"]}

    # Idempotent: nothing more to stamp, nothing written, the original untouched.
    stamped = p.read_bytes()
    again = stamp_task_hashes(p, tasks)
    assert again["stamped"] == 0 and again["already"] == 3 and not again["written"] and p.read_bytes() == stamped
    assert pre_stamp_path(p).read_bytes() == original
    # A record appended later is stamped by a later run; the kept original is never replaced.
    jg._append(p, {"pair": "p7", "task": "A", "judge_hash": judge_hash(tasks)})
    later = stamp_task_hashes(p, tasks)
    assert later["stamped"] == 1 and later["original_kept"] is None and pre_stamp_path(p).read_bytes() == original

    # The point: after a task is added, the stamped records still stand; the others do not.
    more = {**tasks, "C": _write_task(tmp_path / "tasks", "C", ["c1"])}
    th2, gh2 = task_judge_hashes(more), judge_hash(more)
    standing = {r["pair"] for r in rp.read_jsonl(p) if verdict_current(r, th2, gh2)}
    assert standing == {"p1", "p6", "p7"}  # p2's recorded hash is not B's real one


def test_an_existing_original_is_kept_exclusively(tmp_path):
    tasks, batch = _world(tmp_path)
    p, _ = _stamp_file(batch, tasks)
    pre_stamp_path(p).write_bytes(b"an earlier stamper's original\n")
    c = stamp_task_hashes(p, tasks)
    assert c["written"] and c["original_kept"] is None
    assert pre_stamp_path(p).read_bytes() == b"an earlier stamper's original\n"


def test_a_crash_mid_backup_write_leaves_no_partial_original(tmp_path, monkeypatch):
    """A write failure while the temp backup is being written -- before it is
    published under the final name by an exclusive hard link -- leaves no
    ``<name>.pre-stamp.jsonl`` at all (never a partial one) and no leftover temp file;
    the source file is untouched, and the next (unpatched) stamp still keeps a
    complete original."""
    tasks, batch = _world(tmp_path)
    p, _ = _stamp_file(batch, tasks)
    original = p.read_bytes()
    real_write = jg.os.write

    def flaky_write(fd, data):
        buf = bytes(data)
        real_write(fd, buf[: max(1, len(buf) // 2)])  # some bytes land, then it dies
        raise OSError("simulated crash mid-write")

    monkeypatch.setattr(jg.os, "write", flaky_write)
    with pytest.raises(OSError, match="simulated crash"):
        stamp_task_hashes(p, tasks)
    assert not pre_stamp_path(p).exists()
    assert list(p.parent.glob(pre_stamp_path(p).name + ".*")) == []  # no leftover temp
    assert p.read_bytes() == original  # nothing replaced either

    monkeypatch.setattr(jg.os, "write", real_write)
    c = stamp_task_hashes(p, tasks)
    assert c["written"] and c["original_kept"] == pre_stamp_path(p).name
    assert pre_stamp_path(p).read_bytes() == original


def test_a_non_string_task_is_left_alone_not_crashed_on(tmp_path):
    """A malformed record whose "task" is not a string (e.g. a list) cannot be looked
    up by task id; it is left alone and counted as changed or unknown -- the same
    bucket as a task id this run does not have -- never a TypeError."""
    tasks, batch = _world(tmp_path)
    gh = judge_hash(tasks)
    p = batch / "judging" / "verdicts.jsonl"
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(json.dumps({"pair": "p1", "task": [], "judge_hash": gh}) + "\n"
                 + json.dumps({"pair": "p2", "task": "A", "judge_hash": gh}) + "\n")
    c = stamp_task_hashes(p, tasks)
    assert c["stamped"] == 1 and c["task_changed_or_unknown"] == 1 and c["written"]
    recs = list(rp.read_jsonl(p))
    assert "task_judge_hash" not in recs[0] and recs[0]["task"] == []
    assert recs[1]["task_judge_hash"] == task_judge_hash(tasks["A"])


def test_a_mode_change_with_the_same_rubric_is_not_stamped(tmp_path):
    """The global hash leaves out the task's mode -- which changes the judge's packet (the
    plan note) -- so it cannot tell such a change; the batch's task fingerprint can."""
    tasks, batch = _world(tmp_path)
    changed = {**tasks, "A": _write_task(tmp_path / "tasks", "A", ["a1", "a2"], mode="plan")}
    assert judge_hash(changed) == judge_hash(tasks)  # the legacy hash's blind spot
    assert task_judge_hash(changed["A"]) != task_judge_hash(tasks["A"])
    gh = judge_hash(tasks)
    p = batch / "judging" / "verdicts.jsonl"
    p.parent.mkdir(parents=True)
    p.write_text(json.dumps({"pair": "a", "task": "A", "judge_hash": gh}) + "\n"
                 + json.dumps({"pair": "b", "task": "B", "judge_hash": gh}) + "\n")
    c = stamp_task_hashes(p, changed)
    assert c["stamped"] == 1 and c["task_changed_or_unknown"] == 1
    recs = list(rp.read_jsonl(p))
    assert "task_judge_hash" not in recs[0] and recs[1]["task_judge_hash"] == task_judge_hash(changed["B"])


def test_no_batch_fingerprint_no_stamp(tmp_path):
    tasks, _ = _world(tmp_path)
    loose = tmp_path / "elsewhere" / "verdicts.jsonl"  # no batch.json above it
    loose.parent.mkdir()
    loose.write_text(json.dumps({"pair": "a", "task": "A", "judge_hash": judge_hash(tasks)}) + "\n")
    c = stamp_task_hashes(loose, tasks)
    assert c["stamped"] == 0 and c["task_changed_or_unknown"] == 1 and not c["written"]
    # A held-out verdict file finds its batch two levels up.
    held = tmp_path / "batch" / "judging" / "heldout" / "verdicts.jsonl"
    held.parent.mkdir(parents=True)
    held.write_text(json.dumps({"pair": "a", "task": "A", "judge_hash": judge_hash(tasks)}) + "\n")
    assert stamp_task_hashes(held, tasks)["stamped"] == 1


def test_an_append_during_a_stamp_is_not_lost(tmp_path, monkeypatch):
    """A judge appending while the stamper holds the file (between its read and its
    replace) waits on the shared lock and lands in the stamped file."""
    tasks, batch = _world(tmp_path)
    p, lines = _stamp_file(batch, tasks)
    real = jg._replace_with
    seen: dict = {}

    def racing_replace(path, data):
        late = {"pair": "late", "task": "B", "judge_hash": "OLD"}
        t = threading.Thread(target=jg._append, args=(path, late))
        t.start()
        t.join(0.5)
        seen["blocked"] = t.is_alive()  # it cannot append while the stamp holds the lock
        seen["thread"] = t
        real(path, data)

    monkeypatch.setattr(jg, "_replace_with", racing_replace)
    c = stamp_task_hashes(p, tasks)
    seen["thread"].join(10)
    assert c["written"] and seen["blocked"] and not seen["thread"].is_alive()
    recs = [json.loads(x) for x in p.read_text().splitlines() if x.startswith("{")]
    assert [r["pair"] for r in recs] == ["p1", "p2", "p3", "p4", "p5", "p6", "late"]
    assert recs[0]["task_judge_hash"] == task_judge_hash(tasks["A"])


def test_untouched_non_ascii_lines_keep_their_bytes(tmp_path):
    tasks, batch = _world(tmp_path)
    wide = json.dumps({"pair": "w", "task": "A", "judge_hash": "OLD", "reasons": "naïve café – ≥ 2 €"},
                      ensure_ascii=False)
    p, _ = _stamp_file(batch, tasks, extra=[wide])
    c = stamp_task_hashes(p, tasks)
    assert c["written"] and c["stamped"] == 2
    assert p.read_bytes().split(b"\n")[7] == wide.encode("utf-8")


def test_stamp_refuses_what_it_must_not_touch(tmp_path):
    tasks, batch = _world(tmp_path)
    p, _ = _stamp_file(batch, tasks)
    stamp_task_hashes(p, tasks)
    with pytest.raises(StampError, match="pre-stamp"):
        stamp_task_hashes(pre_stamp_path(p), tasks)
    link = tmp_path / "link.jsonl"
    link.symlink_to(p)
    with pytest.raises(StampError, match="regular file"):
        stamp_task_hashes(link, tasks)


def test_judge_hash_stamp_cli(tmp_path, capsys):
    from crazeeval.cli import main
    from crazeeval.tasks import load_tasks

    real = load_tasks()
    gh = judge_hash(real)
    batch = tmp_path / "batch"
    (batch / "judging").mkdir(parents=True)
    (batch / "batch.json").write_text(json.dumps({"tasks": {"T-E1": task_fingerprint(real["T-E1"])}}))
    p = batch / "judging" / "verdicts.jsonl"
    p.write_text(json.dumps({"pair": "p1", "task": "T-E1", "judge_hash": gh}) + "\n"
                 + json.dumps({"pair": "p2", "task": "T-E1", "judge_hash": "OLD"}) + "\n"
                 + json.dumps({"pair": "p3", "task": "T-E2", "judge_hash": gh}) + "\n")
    before = p.read_bytes()
    assert main(["judge-hash", "--stamp", str(p), "--dry-run"]) == 0
    out = capsys.readouterr().out
    assert out.splitlines()[0] == gh and "would stamp 1" in out and "another global hash 1" in out
    assert "task changed or unknown 1" in out  # T-E2 is not in this batch.json
    assert p.read_bytes() == before and not (batch / "judging" / "verdicts.pre-stamp.jsonl").exists()
    assert main(["judge-hash", "--stamp", str(p)]) == 0
    out = capsys.readouterr().out
    assert "stamped 1" in out and "original kept as verdicts.pre-stamp.jsonl" in out
    assert (batch / "judging" / "verdicts.pre-stamp.jsonl").read_bytes() == before
    assert next(rp.read_jsonl(p))["task_judge_hash"] == task_judge_hash(real["T-E1"])
    assert p.read_text().splitlines()[1:] == before.decode().splitlines()[1:]
    assert main(["judge-hash", "--stamp", str(tmp_path / "missing.jsonl")]) == 1
    assert main(["judge-hash", "--dry-run"]) == 2
