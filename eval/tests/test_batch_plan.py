"""Batch planning: supported pairs, ordering, held-out sealing paths."""

from __future__ import annotations

from pathlib import Path

from crazeeval.batch import BatchConfig, plan_runs, run_dir
from crazeeval.config import Snapshot, load_models
from crazeeval.tasks import load_tasks, select_tasks


def cfg(tmp_path, harnesses, models, tasks, reps=1):
    return BatchConfig(harnesses=harnesses, models=models, tasks=tasks, reps=reps, out=tmp_path, label="t",
                       snap=Snapshot(dir=tmp_path, config={}, hash="x"))


def test_plan_runs_skips_unsupported_and_orders_kimi_last(tmp_path):
    ms = load_models()
    models = [ms["fireworks/kimi-k3"], ms["glm-5.3-flash"], ms["muse-spark-1.3"]]
    tasks = select_tasks(load_tasks(), "smoke-explain,smoke-fix")
    runs = plan_runs(cfg(tmp_path, ["craze", "gx", "opencode", "codex"], models, tasks, reps=2))
    keys = [r.key for r in runs]
    assert not any(k.startswith("codex/glm") for k in keys)  # Z.AI has no Responses API
    assert any(k.startswith("codex/muse-spark-1.3/") for k in keys)
    assert all(r.em.key == "fireworks/kimi-k3" for r in runs[-len([r for r in runs if r.em.last]):])
    assert len(keys) == len(set(keys))
    assert {r.rep for r in runs} == {1, 2}


def test_plan_runs_first_rep_plans_only_that_rep(tmp_path):
    """A later batch adding rep2 to an earlier one: ``--first-rep 2 --reps 1`` plans
    exactly rep2, and its run directory ends in ``rep2`` (never re-planning rep1)."""
    ms = load_models()
    models = [ms["glm-5.3-flash"]]
    tasks = select_tasks(load_tasks(), "smoke-explain")
    c = cfg(tmp_path, ["craze"], models, tasks)
    c.first_rep = 2
    runs = plan_runs(c)
    assert [r.rep for r in runs] == [2]
    assert run_dir(c, runs[0]).name == "rep2"


def test_heldout_runs_live_in_the_sealed_part(tmp_path):
    ms = load_models()
    t = load_tasks()["smoke-explain"]
    c = cfg(tmp_path, ["craze"], [ms["glm-5.3-flash"]], [t])
    r = plan_runs(c)[0]
    assert run_dir(c, r) == tmp_path / "runs" / "craze" / "glm-5.3-flash" / "smoke-explain" / "rep1"
    t.split = "heldout"
    assert run_dir(c, r).parts[len(Path(tmp_path).parts)] == "heldout"


def test_classify_statuses():
    from crazeeval.batch import classify
    from crazeeval.runners.base import Extract
    from crazeeval.sandbox import SandboxResult

    def sres(exit_code=0, timed_out=False, quiescent=True, key_hits=0):
        return SandboxResult(exit_code, timed_out, 1.0, "pid:[1]", quiescent, [], {"key_hits": key_hits}, [])

    ok = Extract(answer="done", completed=True)
    none = Extract(answer="", failure="no result line")
    fine = [{"status": 200}]
    assert classify(sres(), None, {"flags": []}, ok, fine, [], None) == "ok"
    assert classify(None, "OSError: x", {"flags": []}, ok, fine, [], None) == "launch-error"
    assert classify(sres(), None, {"flags": [], "key_exposure_refusals": 1}, ok, fine, [], None) == "key-exposure"
    assert classify(sres(key_hits=1), None, {"flags": []}, ok, fine, [], None) == "key-exposure"
    assert classify(sres(), None, {"flags": ["budget-capped"]}, ok, fine, [], None) == "budget-capped"
    assert classify(sres(quiescent=False), None, {"flags": []}, ok, fine, [], None) == "infra"
    assert classify(sres(), None, {"flags": []}, ok, fine, [{"rule": "eval-dir"}], None) == "contaminated"
    assert classify(sres(timed_out=True, exit_code=-9), None, {"flags": []}, none, fine, [], None) == "timeout"
    assert classify(sres(exit_code=1), None, {"flags": []}, none, [{"status": 429}], [], None) == "infra"
    assert classify(sres(exit_code=1), None, {"flags": []}, none, [{"status": 502}], [], None) == "infra"
    # A harness that hung up on its own stream and then failed is a crash, not infra.
    assert classify(sres(exit_code=1), None, {"flags": []}, none, [{"status": 200, "error": "cancelled"}], [], None) == "crashed"
    assert classify(sres(exit_code=0), None, {"flags": []}, none, fine, [], None) == "crashed"


def test_error_after_partial_text_is_a_failure():
    """Review r1-c1 finding 17: preliminary text, then an error event, exit 0, and a
    last upstream 500 -- never ``ok``."""
    from crazeeval.batch import classify
    from crazeeval.runners.base import Extract
    from crazeeval.runners.codex import extract_codex
    from crazeeval.runners.craze import extract_craze
    from crazeeval.runners.gx import extract_gx
    from crazeeval.runners.opencode import extract_opencode
    from crazeeval.sandbox import SandboxResult

    oc = extract_opencode([
        {"type": "step_start", "part": {"messageID": "m1"}},
        {"type": "text", "part": {"id": "p1", "messageID": "m1", "text": "Looking into it..."}},
        {"type": "step_finish", "part": {"messageID": "m1", "reason": "stop"}},
        {"type": "error", "error": {"name": "APIError", "data": {"message": "upstream 500"}}},
    ])
    assert oc.answer == "Looking into it..." and not oc.completed and "error event" in oc.failure
    ok = SandboxResult(0, False, 1.0, "pid:[1]", True, [], {"key_hits": 0}, [])
    assert classify(ok, None, {"flags": []}, oc, [{"status": 500}], [], None) == "infra"
    assert classify(ok, None, {"flags": []}, oc, [{"status": 200}], [], None) == "crashed"
    cz = extract_craze([{"type": "text", "text": "partial"}, {"type": "error", "message": "boom"},
                        {"type": "done", "stopReason": "end_turn"}])
    assert not cz.completed
    assert not extract_craze([{"type": "text", "text": "x"}, {"type": "done", "stopReason": "cancelled"}]).completed
    assert extract_craze([{"type": "text", "text": "x"}, {"type": "done", "stopReason": "end_turn"}]).completed
    assert not extract_gx([{"type": "result", "subtype": "error_during_execution", "is_error": True}]).completed
    assert extract_gx([{"type": "result", "subtype": "success", "is_error": False, "result": "x"}]).completed
    assert not extract_codex([{"type": "item.completed", "item": {"type": "agent_message", "text": "partial"}},
                              {"type": "turn.failed", "error": {"message": "x"}}], None).completed
    assert extract_codex([{"type": "error", "message": "Reconnecting..."}, {"type": "turn.completed", "usage": {}}], "x").completed
    assert classify(ok, None, {"flags": []}, Extract(answer="", completed=True), [{"status": 200}], [], None) == "ok"


def test_prune_removes_only_the_named_caches(tmp_path):
    from crazeeval.batch import _prune

    (tmp_path / "home" / ".npm" / "a").mkdir(parents=True)
    (tmp_path / "home" / ".npm" / "a" / "blob").write_bytes(b"x" * 100)
    (tmp_path / "home" / ".xdg" / "data" / "opencode").mkdir(parents=True)
    (tmp_path / "home" / ".xdg" / "data" / "opencode" / "opencode.db").write_bytes(b"db")
    out = _prune(tmp_path, ["home/.npm", "home/.xdg/config/opencode/node_modules"])
    assert out == {"home/.npm": 100}
    assert not (tmp_path / "home/.npm").exists() and (tmp_path / "home/.xdg/data/opencode/opencode.db").exists()


def _ident(c):
    from crazeeval.batch import batch_identity
    from crazeeval.pricing import load_prices

    return batch_identity(c, {"craze": {"sha256": "c0ffee", "version": "dev"}}, load_prices())


def test_batch_dir_is_exclusive_locked_and_resumed_only_on_match(tmp_path):
    """Review r1-c1 finding 18: an existing directory is never reused silently, a
    second process cannot take a live one, and --resume checks what it reopens."""
    import pytest

    from crazeeval.batch import BatchDirError, _next_attempt, done_result, open_batch_dir

    ms = load_models()
    t = select_tasks(load_tasks(), "smoke-explain")
    c = cfg(tmp_path / "b1", ["craze"], [ms["glm-5.3-flash"]], t)
    lock = open_batch_dir(c, _ident(c))
    assert (tmp_path / "b1" / "batch.json").exists()
    with pytest.raises(BatchDirError, match="already exists"):
        c1 = cfg(tmp_path / "b1", ["craze"], [ms["glm-5.3-flash"]], t)
        open_batch_dir(c1, _ident(c1))
    c2 = cfg(tmp_path / "b1", ["craze"], [ms["glm-5.3-flash"]], t)
    c2.resume = True
    with pytest.raises(BatchDirError, match="in use"):
        open_batch_dir(c2, _ident(c2))  # the first batch still holds the lock
    lock.close()
    open_batch_dir(c2, _ident(c2)).close()  # now it may be resumed
    c3 = cfg(tmp_path / "b1", ["gx"], [ms["glm-5.3-flash"]], t)
    c3.resume = True
    with pytest.raises(BatchDirError, match="different batch"):
        open_batch_dir(c3, _ident(c3))
    # Attempt numbering continues; a finished run is recognised, an unfinished one is not.
    rdir = tmp_path / "b1" / "runs" / "craze" / "glm-5.3-flash" / "smoke-explain" / "rep1"
    (rdir / "attempt-1").mkdir(parents=True)
    (rdir / "attempt-2").mkdir()
    assert _next_attempt(rdir) == 2 and done_result(rdir) is None
    (rdir / "result.json").write_text('{"status": "ok"}')
    assert done_result(rdir) == {"status": "ok"}


def test_resume_refuses_a_changed_task_executable_or_setting(tmp_path):
    """Review r2-c1 item 16: the identity fingerprint covers task contents and
    testdata, executables and execution settings, not just names."""
    import shutil

    import pytest

    from crazeeval import paths
    from crazeeval.batch import BatchDirError, batch_identity, open_batch_dir
    from crazeeval.pricing import load_prices
    from crazeeval.tasks import load_task

    tasks_dir = tmp_path / "tasks"
    shutil.copytree(paths.TASKS_DIR / "smoke-explain", tasks_dir / "smoke-explain")
    ms = load_models()
    c = cfg(tmp_path / "b", ["craze"], [ms["glm-5.3-flash"]], [load_task(tasks_dir / "smoke-explain")])
    ident = _ident(c)
    assert ident == _ident(c)  # stable for the same inputs
    open_batch_dir(c, ident).close()

    def resumed(c2, execs=None):
        c2.resume = True
        return open_batch_dir(c2, batch_identity(c2, execs or {"craze": {"sha256": "c0ffee", "version": "dev"}},
                                                  load_prices()))

    # The same prompt text changed in task.toml, nothing else.
    toml = tasks_dir / "smoke-explain" / "task.toml"
    toml.write_text(toml.read_text().replace("answer two questions", "answer 2 questions"))
    with pytest.raises(BatchDirError, match=r"tasks\.smoke-explain"):
        resumed(cfg(tmp_path / "b", ["craze"], [ms["glm-5.3-flash"]], [load_task(tasks_dir / "smoke-explain")]))
    toml.write_text(toml.read_text().replace("answer 2 questions", "answer two questions"))
    # A decoy answer (testdata) changed.
    decoy = tasks_dir / "smoke-explain" / "testdata" / "decoy_answer.md"
    decoy.write_text(decoy.read_text() + "x")
    with pytest.raises(BatchDirError, match=r"tasks\.smoke-explain"):
        resumed(cfg(tmp_path / "b", ["craze"], [ms["glm-5.3-flash"]], [load_task(tasks_dir / "smoke-explain")]))
    decoy.write_text(decoy.read_text()[:-1])
    same = cfg(tmp_path / "b", ["craze"], [ms["glm-5.3-flash"]], [load_task(tasks_dir / "smoke-explain")])
    resumed(same).close()  # back to the original: accepted
    # A different harness binary, or a different timeout.
    with pytest.raises(BatchDirError, match=r"executables\.craze"):
        resumed(same, {"craze": {"sha256": "beef", "version": "dev"}})
    other = cfg(tmp_path / "b", ["craze"], [ms["glm-5.3-flash"]], [load_task(tasks_dir / "smoke-explain")])
    other.timeout_s = 60
    with pytest.raises(BatchDirError, match=r"execution\.timeout_s"):
        resumed(other)

    # Review r3-c1 item H: an opencode seed's contents (not just its directory name)
    # and the execution settings that affect concurrency (parallel, provider caps)
    # are part of what makes two batches' results comparable.
    seed = tmp_path / "seed"
    seed.mkdir()
    (seed / "package.json").write_text('{"v": 1}')
    (seed / "package-lock.json").write_text("{}")
    (seed / "node_modules").mkdir()

    def seeded(parallel=4):
        c2 = cfg(tmp_path / "b2", ["craze"], [ms["glm-5.3-flash"]], [load_task(tasks_dir / "smoke-explain")])
        c2.opencode_seed = seed
        c2.parallel = parallel
        return c2

    def resumed2(c2, execs=None):
        c2.resume = True
        return open_batch_dir(c2, batch_identity(c2, execs or {"craze": {"sha256": "c0ffee", "version": "dev"}},
                                                  load_prices()))

    open_batch_dir(seeded(), _ident(seeded())).close()
    resumed2(seeded()).close()  # unchanged seed and parallel: accepted

    (seed / "package.json").write_text('{"v": 2}')  # the seed's contents changed, its name did not
    with pytest.raises(BatchDirError, match="opencode_seed"):
        resumed2(seeded())
    (seed / "package.json").write_text('{"v": 1}')  # back to matching

    with pytest.raises(BatchDirError, match=r"execution\.parallel"):
        resumed2(seeded(parallel=8))


def test_batch_identity_records_first_rep_only_when_not_one(tmp_path):
    """A config with the default ``first_rep=1`` has the same identity dict
    FIELDS as before this field existed (no ``first_rep`` key); the fingerprint
    itself still moves with any evaluator source change, this one included.
    ``first_rep=2`` records it and changes the fingerprint too."""
    ms = load_models()
    t = select_tasks(load_tasks(), "smoke-explain")
    default = cfg(tmp_path, ["craze"], [ms["glm-5.3-flash"]], t)
    ident_default = _ident(default)
    assert "first_rep" not in ident_default

    bumped = cfg(tmp_path, ["craze"], [ms["glm-5.3-flash"]], t)
    bumped.first_rep = 2
    ident_bumped = _ident(bumped)
    assert ident_bumped["first_rep"] == 2
    assert ident_bumped["fingerprint"] != ident_default["fingerprint"]

    # Setting it back to 1 explicitly reproduces the pre-existing identity exactly.
    explicit_one = cfg(tmp_path, ["craze"], [ms["glm-5.3-flash"]], t)
    explicit_one.first_rep = 1
    assert _ident(explicit_one) == ident_default


def test_cli_rejects_first_rep_below_one(capsys):
    from crazeeval.cli import main

    assert main(["run", "--first-rep", "0"]) == 2
    assert "--first-rep must be >= 1" in capsys.readouterr().err


def test_select_tasks():
    all_tasks = load_tasks()
    assert [t.id for t in select_tasks(all_tasks, "split:smoke")] == ["smoke-explain", "smoke-fix"]
    assert select_tasks(all_tasks, None) == [t for t in all_tasks.values() if t.split in ("dev", "heldout")]
