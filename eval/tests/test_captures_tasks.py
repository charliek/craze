"""The capture report on a recorded capture (plan 029 §3.1.10, AC-A8), the C2 task set,
and the helpers C2 added to scoring and pruning."""

from __future__ import annotations

import asyncio
import json
import shutil
from pathlib import Path

import pytest

from crazeeval import captures, paths
from crazeeval import capture as cap
from crazeeval.batch import _prune
from crazeeval.checks import check_structural_count
from crazeeval.config import Snapshot, catalog_max_output, load_models
from crazeeval.sandbox import Toolchains, bwrap_available
from crazeeval.tasks import load_tasks
from crazeeval.validate import validate_task

S = Path(__file__).parent / "samples"
SAMPLE = S / "capture-craze-smoke-fix.jsonl"  # craze on deepseek-v4p1-flash, smoke-fix, first two requests

C2_TASKS = ["T-E1", "T-E2", "T-E3", "T-I1", "T-I2", "T-B1", "T-B2", "T-B3", "T-F1", "T-F2", "T-R1", "T-M1", "T-V1",
            "T-P1", "T-P2"]
DEV = {"T-E1", "T-E2", "T-I1", "T-B1", "T-F1", "T-R1", "T-P1"}


def test_capture_helpers_on_a_recorded_capture():
    recs = cap.read_capture(SAMPLE)
    assert len(recs) == 2
    first, second = recs[0]["request"], recs[1]["request"]
    st = captures.system_text(first)
    assert st.startswith("You are craze") and "/sandbox/work/smoke-py" in st
    assert captures.env_location(st, captures.first_user_text(first))["workspace"] == "system"
    tools = captures.tool_descriptions(first)
    assert len(tools) == 11 and {t["name"] for t in tools} >= {"bash", "read", "edit", "todo_write", "agent"}
    assert all(t["description_bytes"] > 0 for t in tools)
    p = captures.params(first)
    assert p["reasoning_effort"] == "high" and p["stream"] is True and "messages" not in p
    assert captures.replay_counts(first) == (0, 0)  # nothing replayed yet
    turns, with_r = captures.replay_counts(second)
    assert turns == 1 and with_r in (0, 1)
    assert captures.role_sequence(second) == "system, user, assistant, tool"
    assert captures.effective_effort({"reasoning_effort": "high"}) == "high"
    assert captures.effective_effort({"reasoning.effort": "max"}) == "max"
    assert captures.effective_effort({}) == "(none)"


def _batch_with(tmp_path: Path, harness: str, slug: str, capture: Path, metrics: dict | None = None) -> Path:
    rep = tmp_path / "b" / "runs" / harness / slug / "smoke-fix" / "rep1"
    adir = rep / "attempt-1"
    adir.mkdir(parents=True)
    shutil.copy(capture, adir / "capture.jsonl")
    recs = cap.read_capture(adir)
    m = metrics or cap.metrics(recs)
    res = {"harness": harness, "status": "ok", "contamination": [], "metrics": m,
           "attempts": [{"attempt": 1, "dir": "attempt-1"}]}
    (rep / "result.json").write_text(json.dumps(res))
    return tmp_path / "b"


def test_capture_report_and_fidelity_checks(tmp_path):
    b = _batch_with(tmp_path, "craze", "fireworks_deepseek-v4p1-flash", SAMPLE)
    other = cap.metrics(cap.read_capture(SAMPLE))
    other["main_efforts"] = [{"reasoning.effort": "medium"}]
    _batch_with(tmp_path, "codex", "fireworks_deepseek-v4p1-flash", SAMPLE, other)
    out = captures.write(b)
    rep = json.loads((out / "captures.json").read_text())
    block = rep["models"]["fireworks/deepseek-v4p1-flash"]
    row = next(r for r in block["harnesses"] if r["harness"] == "craze")
    assert row["system_prompt"]["bytes"] > 1000 and len(row["system_prompt"]["sha256"]) == 64
    assert Path(row["system_prompt"]["file"]).read_text().startswith("You are craze")
    assert row["tools"]["count"] == 11 and row["params"]["reasoning_effort"] == ["high"]
    assert row["served_models"] and row["request_models"] == ["accounts/fireworks/models/deepseek-v4p1-flash"]
    f = block["fidelity"]
    assert f["effort_parity"] is False and f["effort_by_harness"]["craze"] == ["high"]
    assert f["web_tools_absent"] is True and f["one_model_per_run"] is True and f["no_contamination_in_accepted_runs"]
    assert f["main_request_coverage"] == {"craze": True, "codex": True}
    assert f["opencode_glm_thinking_flag"] is None  # not a GLM model
    assert row["reasoning_replay"]["assistant_turns"] == 1 and row["runs_without_main_requests"] == 0
    md = (out / "captures.md").read_text()
    assert "effort parity: **no**" in md and "| craze |" in md


def test_the_fifteen_tasks_load_with_their_split():
    tasks = load_tasks()
    assert set(C2_TASKS) <= set(tasks)
    assert {t for t in C2_TASKS if tasks[t].split == "dev"} == DEV
    assert all(tasks[t].split == "heldout" for t in set(C2_TASKS) - DEV)
    assert {tasks[t].id for t in C2_TASKS if tasks[t].is_plan} == {"T-P1", "T-P2"}
    for t in C2_TASKS:
        task = tasks[t]
        assert task.name and task.rubric, t
        if task.repo_kind == "craze" and task.category in ("explain", "answer", "plan"):
            assert 5 <= len(task.rubric) <= 9 and 2 <= len(task.false_claims) <= 3, t
            assert all(":" in item for item in task.rubric), t  # every craze fact cites file:line
    assert tasks["T-B3"].setup_patch is not None


@pytest.mark.skipif(not bwrap_available(), reason="bubblewrap is not usable here")
@pytest.mark.parametrize("task_id", C2_TASKS)
def test_every_task_validator_passes(task_id):
    v = asyncio.run(validate_task(load_tasks()[task_id], Toolchains.resolve()))
    assert v.ok, v.to_dict()
    names = " | ".join(c.name for c in v.controls)
    # The controls review r1-c2 asked for are there, not only passing.
    must = {
        "T-B3": ["plus a denied edit", "plus an edit outside the scope", "setup.patch reversed"],
        "T-V1": ["completed call that exercised the case", "irrelevant completed call", "attempted, no result"],
        "T-R1": ["one-helper-at-all-three-call-sites: untouched workspace",
                 "one-helper-at-all-three-call-sites: reference.patch"],
    }.get(task_id, [])
    for fragment in must:
        assert fragment in names, (fragment, names)


def test_task_fixes_from_review_r1_c2():
    tasks = load_tasks()
    hidden = (tasks["T-F2"].dir / "testdata" / "hidden" / "bytes_hidden_test.go").read_text()
    assert '{math.MinInt64, "-8 EiB"}' in hidden
    t = tasks["T-M1"]
    add = next(c for c in t.checks if c["type"] == "tests")["add"]
    assert "cmd/kv/kv_cli_hidden_test.go" in add and "-namespace" in t.prompt and "list-namespaces" in t.prompt
    f1 = tasks["T-F1"]
    assert "README" in f1.prompt and "README.md" in (f1.dir / "testdata" / "reference.patch").read_text()
    assert any("docs" in item.lower() for item in f1.rubric)
    assert any("compaction is the exception" in item for item in tasks["T-E1"].rubric)
    assert any("Session.CostPicoUSD == 0 && Session.Unpriced" in item for item in tasks["T-E3"].rubric)
    v1 = next(c for c in tasks["T-V1"].checks if c["type"] == "executed_code")
    assert v1["require_result"] and v1["evidence"]


def test_structural_count_by_path_and_with_excludes(tmp_path):
    (tmp_path / "bucket.go").write_text("package x\n")
    (tmp_path / "tests").mkdir()
    (tmp_path / "tests" / "t.py").write_text("raise X('invalid email address')\n")
    (tmp_path / "a.py").write_text("raise X('invalid email address')\n")
    by_path = {"type": "structural_count", "target": "path", "glob": ["*"], "regex": "(?i)bucket", "value": 0}
    assert not check_structural_count(by_path, tmp_path)["passed"]
    content = {"type": "structural_count", "glob": ["*.py"], "exclude": ["tests/*"], "regex": "invalid email",
               "value": 1}
    r = check_structural_count(content, tmp_path)
    assert r["passed"] and r["details"]["files"] == {"a.py": 1}


def test_prune_expands_a_glob_without_following_links(tmp_path):
    adir = tmp_path / "attempt-1"
    (adir / "home" / ".codex" / "skills" / "x").mkdir(parents=True)
    (adir / "home" / ".codex" / "state_5.sqlite").write_text("s")
    (adir / "home" / ".codex" / "state_5.sqlite-wal").write_text("w" * 10)
    (adir / "home" / ".codex" / "sessions.jsonl").write_text("keep")
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "a.sqlite").write_text("x")
    (adir / "home" / "link").symlink_to(outside)
    pruned = _prune(adir, ["home/.codex/skills", "home/.codex/*.sqlite", "home/.codex/*.sqlite-wal", "home/link/*.sqlite"])
    assert set(pruned) == {"home/.codex/skills", "home/.codex/state_5.sqlite", "home/.codex/state_5.sqlite-wal"}
    assert (adir / "home" / ".codex" / "sessions.jsonl").exists() and (outside / "a.sqlite").exists()


def test_reservation_maximum_is_the_largest_applicable_limit(tmp_path):
    catalog = {
        "fireworks-ai": {"models": {"accounts/fireworks/models/deepseek-v4p1-flash": {"limit": {"output": 384000}},
                                    "accounts/fireworks/models/kimi-k3": {"limit": {"output": 65536}}}},
        "meta": {"models": {"muse-spark-1.3": {"limit": {"output": 131072}}}},
    }
    (tmp_path / "opencode-models.json").write_text(json.dumps(catalog))
    models = load_models()
    cfg = {"models": {k: {"craze": {}, "gx": {}} for k in models}}
    cfg["models"]["muse-spark-1.3"]["craze"] = {"max_output_tokens": 100000}
    cfg["models"]["fireworks/deepseek-v4p1-flash"]["craze"] = {"max_output_tokens": 100000}
    cfg["models"]["fireworks/deepseek-v4p1-flash"]["gx"] = {"max_completion_tokens": 200000}
    snap = Snapshot(dir=tmp_path, config=cfg, hash="h")
    mo = snap.max_output(models.values())
    # craze 100k, gx 200k, models.dev 384k: a request with no ceiling may run to the largest.
    assert mo["accounts/fireworks/models/deepseek-v4p1-flash"] == 384000
    assert mo["muse-spark-1.3"] == 131072  # craze lists less than the catalog: the catalog
    assert mo["accounts/fireworks/models/kimi-k3"] == 65536  # the only limit known
    assert mo["glm-5.3"] == 131072  # none known: the default
    assert catalog_max_output(catalog, "nope/x") is None


# -- fidelity needs evidence; replay counted per turn ----------------------------------------------


def _row(harness, runs):
    return {"harness": harness, "per_run": runs, "effective_efforts": sorted({e for r in runs for e in r["efforts"]}),
            "contaminated_accepted_runs": 0, "thinking_flag": {"requests": sum(r["main_requests"] for r in runs),
                                                              "with_flag": sum(r["main_requests"] for r in runs)}}


def _run(main=3, req=("w",), served=("w",), efforts=("high",), web=(), refusals=0):
    return {"run": "r", "main_requests": main, "request_models": list(req), "served_models": list(served),
            "model_refusals": refusals, "efforts": list(efforts), "web_tools_offered": list(web)}


def test_fidelity_is_no_evidence_without_main_requests():
    f = captures.fidelity("m", [_row("craze", [_run()]), _row("gx", [_run(), _run(main=0, req=(), served=())])],
                          "high", "w")
    assert f["main_request_coverage"] == {"craze": True, "gx": False}
    assert f["effort_parity"] == captures.NO_EVIDENCE
    assert f["web_tools_absent"] == captures.NO_EVIDENCE and f["one_model_per_run"] == captures.NO_EVIDENCE
    # No rows at all is no evidence either, never true.
    assert captures.fidelity("m", [], "high", "w")["one_model_per_run"] == captures.NO_EVIDENCE
    ok = captures.fidelity("m", [_row("craze", [_run()]), _row("gx", [_run()])], "high", "w")
    assert ok["effort_parity"] is True and ok["web_tools_absent"] is True and ok["one_model_per_run"] is True


def test_fidelity_checks_served_models_too():
    served_other = captures.fidelity("m", [_row("craze", [_run(served=("w-2026-preview",))])], "high", "w")
    assert served_other["one_model_per_run"] is False and served_other["one_model_failures"]
    two = captures.fidelity("m", [_row("craze", [_run(served=("w", "x"))])], "high", "w")
    assert two["one_model_per_run"] is False
    asked = captures.fidelity("m", [_row("craze", [_run(req=("w", "small"))])], "high", "w")
    assert asked["one_model_per_run"] is False
    refused = captures.fidelity("m", [_row("craze", [_run(refusals=1)])], "high", "w")
    assert refused["one_model_per_run"] is False
    unknown = captures.fidelity("m", [_row("craze", [_run(served=())])], "high", "w")
    assert unknown["one_model_per_run"] == captures.NO_EVIDENCE
    web = captures.fidelity("m", [_row("craze", [_run(web=("webfetch",)), _run(main=0)])], "high", "w")
    assert web["web_tools_absent"] is False  # a failing run outweighs a missing one


def test_glm_thinking_flag_needs_evidence():
    oc = _row("opencode", [_run(main=0, req=(), served=())])
    assert captures.fidelity("glm-5.3", [oc], "max", "glm-5.3")["opencode_glm_thinking_flag"] == captures.NO_EVIDENCE
    oc = _row("opencode", [_run()])
    oc["thinking_flag"] = {"requests": 3, "with_flag": 2}
    assert captures.fidelity("glm-5.3", [oc], "max", "w")["opencode_glm_thinking_flag"] is False


def test_replay_counts_each_assistant_turn():
    chat = {"messages": [
        {"role": "system", "content": "s"}, {"role": "user", "content": "q"},
        {"role": "assistant", "content": None, "reasoning_content": "thought", "tool_calls": []},
        {"role": "tool", "tool_call_id": "a", "content": "r"},
        {"role": "assistant", "content": None, "tool_calls": []},  # reasoning stripped
        {"role": "tool", "tool_call_id": "b", "content": "r"},
        {"role": "assistant", "content": "x", "reasoning": ""},  # empty: not carried
    ]}
    assert captures.replay_counts(chat) == (3, 1)
    responses = {"input": [
        {"role": "user", "content": [{"type": "input_text", "text": "q"}]},
        {"type": "reasoning", "summary": []}, {"type": "function_call", "call_id": "a", "name": "exec_command"},
        {"type": "function_call_output", "call_id": "a", "output": "ok"},
        {"type": "function_call", "call_id": "b", "name": "exec_command"},  # a turn without its reasoning
        {"type": "function_call_output", "call_id": "b", "output": "ok"},
        {"role": "assistant", "type": "message", "content": [{"type": "output_text", "text": "a"}]},
        {"type": "reasoning", "summary": []},  # the same turn's reasoning, after its message
        {"role": "user", "content": [{"type": "input_text", "text": "more"}]},
    ]}
    assert captures.replay_counts(responses) == (3, 2)
    assert captures.replay_counts({"messages": [{"role": "user", "content": "q"}]}) == (0, 0)


# -- the executed-code check needs a completed call that exercised the case -----------------------


def _exec_capture(calls, results):
    from test_judge import _chat_record

    return [_chat_record(1, calls=calls), _chat_record(2, tool_results=results)]


V1_CHECK = {"type": "executed_code", "name": "x", "patterns": [r"\bpython[0-9.]*\b", r"\bpytest\b"],
            "require_result": True, "evidence": [r'parse_batch\(\s*(""|\x27\x27|str\(\))\s*\)',
                                                 "not enough values to unpack"]}


def test_executed_code_requires_the_case_and_a_result():
    from crazeeval.checks import check_executed_code

    run_it = ("c1", "bash", {"command": "python3 -c 'import batch; batch.parse_batch(\"\")'"})
    tb = "ValueError: not enough values to unpack (expected at least 1, got 0)"
    assert check_executed_code(V1_CHECK, _exec_capture([run_it], [("c1", tb)]))["passed"]
    # Attempted, never answered: no.
    r = check_executed_code(V1_CHECK, _exec_capture([run_it], []))
    assert not r["passed"] and r["details"]["attempted_without_result"]
    # A Python call that has nothing to do with it: no.
    version = ("c1", "bash", {"command": "python3 --version"})
    assert not check_executed_code(V1_CHECK, _exec_capture([version], [("c1", "Python 3.12.3")]))["passed"]
    # A script run whose output shows the case: yes.
    script = ("c1", "run_terminal_command", {"command": "python3 check.py"})
    assert check_executed_code(V1_CHECK, _exec_capture([script], [("c1", "Traceback ...\n" + tb)]))["passed"]
    # A script the run wrote first, then ran (its output says nothing specific): yes.
    wrote = ("w1", "write", {"filePath": "/sandbox/work/batchexport/probe.py",
                             "content": "import batch\nprint(batch.parse_batch(''))\n"})
    ran = ("c2", "exec_command", {"cmd": "python probe.py || true"})
    recs = _exec_capture([wrote, ran], [("w1", "ok"), ("c2", "exit 1")])
    got = check_executed_code(V1_CHECK, recs)
    assert got["passed"] and got["details"]["hits"][0]["evidence_in"] == "a file this command runs"


def test_executed_code_evidence_in_narrows_where_it_shows(tmp_path):
    """plan 033 C11r2 (review r8 #7): ``evidence_in = ["result"]`` counts only what the
    call returned -- not the evidence its own command spells (a comment), nor a file an
    earlier call wrote that it runs. The control is the default, every place, under which
    each capture passes; a value outside the three places does not load."""
    from crazeeval.checks import check_executed_code
    from crazeeval.tasks import TaskError, load_task

    anywhere = {"type": "executed_code", "name": "e", "patterns": [r"\bcurl\b"], "evidence": ["BODY"]}
    returned = {**anywhere, "evidence_in": ["result"]}
    in_command = _exec_capture([("c1", "bash", {"command": "curl -s x  # BODY"})], [("c1", "curl: (7) Failed to connect")])
    in_result = _exec_capture([("c1", "bash", {"command": "curl -s x"})], [("c1", "BODY")])
    in_file = _exec_capture([("w1", "write", {"filePath": "/sandbox/work/probe.sh", "content": "echo BODY"}),
                             ("c1", "bash", {"command": "curl -s x; sh probe.sh"})],
                            [("w1", "ok"), ("c1", "curl: (7) Failed to connect")])
    assert [check_executed_code(anywhere, c)["details"]["hits"][0]["evidence_in"] for c in (in_command, in_result, in_file)] == \
        ["command", "result", "a file this command runs"]
    assert check_executed_code(returned, in_result)["passed"]
    assert not check_executed_code(returned, in_command)["passed"]
    assert not check_executed_code(returned, in_file)["passed"]

    src = paths.TASKS_DIR / "T-D2"
    dst = tmp_path / "tasks" / "T-D2"
    shutil.copytree(src, dst)
    text = (dst / "task.toml").read_text()
    for bad in ('["stdout"]', "[]", '"result"'):
        (dst / "task.toml").write_text(text.replace('evidence_in = ["result"]', f"evidence_in = {bad}"))
        with pytest.raises(TaskError, match="evidence_in"):
            load_task(dst)


def _executed(task, calls, results) -> bool:
    """Whether every executed_code check of ``task`` passes on a capture of ``calls`` and
    their ``results`` -- by the task's own checks, whatever their names."""
    from crazeeval.checks import check_executed_code

    checks = [c for c in task.checks if c["type"] == "executed_code"]
    assert checks
    recs = _exec_capture(calls, results)
    return all(check_executed_code(c, recs)["passed"] for c in checks)


def test_t_d1_needs_a_completed_run_of_the_whole_suite():
    """plan 033 C11r2 (review r8 #7a): T-D1's execution evidence is a pytest call whose own
    result summarises a run of the whole suite that waited for the 150 s integration test,
    every test passing. A unit-only run, a run that skipped, deselected or failed a test, a
    run of the slow test alone, and a killed call whose command spells the summary are no
    evidence -- the old check (`\\d+ passed` anywhere) took each of them."""
    t = load_tasks()["T-D1"]

    def ran(command, output):
        return _executed(t, [("c1", "bash", {"command": command, "timeout": 300000})], [("c1", output)])

    assert ran("pytest tests", "tests/test_integration_slow.py .\ntests/test_ledger.py .....\n\n"
                               "======================== 6 passed in 151.42s (0:02:31) =========================")
    assert ran("python -m pytest -q tests", "......\n6 passed in 150.31s (0:02:30)")
    assert ran("pytest tests", "7 passed, 1 warning in 151.02s (0:02:31)")  # a regression test added
    assert not ran("pytest tests/test_ledger.py", "5 passed in 0.03s")
    assert not ran("pytest tests -m 'not slow'", "5 passed, 1 deselected in 0.04s")
    assert not ran("pytest tests", "5 passed, 1 skipped in 0.05s")
    assert not ran("pytest tests", "1 failed, 5 passed in 151.20s (0:02:31)")
    assert not ran("pytest tests/test_integration_slow.py", "1 passed in 150.01s (0:02:30)")
    assert not ran("pytest tests  # 6 passed in 151.42s (0:02:31)",
                   "tests/test_ledger.py .....\n\n<shell_metadata>\nbash tool terminated command after exceeding timeout 120000 ms\n"
                   "</shell_metadata>")


def test_t_d2_needs_both_served_bodies_in_returned_output():
    """plan 033 C11r2 (review r8 #7c): T-D2's execution evidence is each endpoint's served
    body in a curl call's own returned output -- one call for both, or one call each,
    compact or reformatted. A failed request whose command spells the expected body in a
    comment, one endpoint alone, a listing of app.py's source, and a file written earlier
    are no evidence -- the old check (either body, anywhere) took each of them."""
    t = load_tasks()["T-D2"]
    health = '{"status":"ok","checks":{"db":"up","queue":"up"}}'
    version = '{"version":"2.7.3","build":"a41c9e0"}'
    failed = "curl: (7) Failed to connect to localhost port 8000 after 0 ms: Couldn't connect to server"
    source = (paths.FIXTURES_DIR / "devserver" / "files" / "app.py").read_text()

    def curl(cid, command):
        return (cid, "bash", {"command": command})

    assert _executed(t, [curl("c1", "curl -s localhost:8000/health; echo; curl -s localhost:8000/version")],
                     [("c1", health + "\n" + version + "\n")])
    assert _executed(t, [curl("c1", "curl -s localhost:8000/health | python3 -m json.tool"),
                         curl("c2", "curl -s localhost:8000/version | jq .")],
                     [("c1", json.dumps(json.loads(health), indent=4)), ("c2", json.dumps(json.loads(version), indent=2))])
    assert not _executed(t, [curl("c1", "curl localhost:8000/health  # expected: " + health),
                             curl("c2", "curl localhost:8000/version  # expected: " + version)],
                         [("c1", failed), ("c2", failed)])
    assert not _executed(t, [curl("c1", "curl -s localhost:8000/health")], [("c1", health)])
    assert not _executed(t, [curl("c1", "curl -s localhost:8000/version"), curl("c2", "curl -s localhost:8000/health; cat app.py")],
                         [("c1", version), ("c2", failed + "\n" + source)])
    wrote = ("w1", "write", {"filePath": "/sandbox/work/devserver/expected.json", "content": health + "\n" + version + "\n"})
    assert not _executed(t, [wrote, curl("c1", "curl -s localhost:8000/health localhost:8000/version > got.json; diff got.json expected.json")],
                         [("w1", "ok"), ("c1", failed)])


# -- the shared-helper check ------------------------------------------------------------------------


def test_shared_helper_needs_one_helper_used_by_every_caller(tmp_path):
    from crazeeval.checks import check_shared_helper

    check = {"type": "shared_helper", "glob": ["*.py"], "exclude": ["tests/*"],
             "callers": ["D.a", "D.b", "D.c"], "markers": ["RE", "BLOCKED"]}
    helper = "def norm(e):\n    if not RE.match(e): raise ValueError\n    if e in BLOCKED: raise ValueError\n    return e\n"
    good = helper + "class D:\n" + "".join(f"    def {m}(self, e):\n        return norm(e)\n" for m in "abc")
    (tmp_path / "m.py").write_text(good)
    r = check_shared_helper(check, tmp_path)
    assert r["passed"] and r["details"]["helpers"] == ["norm"]
    # Three copies in new syntax, no helper: fails.
    copies = "class D:\n" + "".join(
        f"    def {m}(self, e):\n        ok = RE.fullmatch(e) is not None and e not in BLOCKED\n        return e\n" for m in "abc")
    (tmp_path / "m.py").write_text(copies)
    r = check_shared_helper(check, tmp_path)
    assert not r["passed"] and r["details"]["callers_with_markers"]
    # A helper, but one caller still validates inline: fails.
    mixed = helper + "class D:\n    def a(self, e):\n        return norm(e)\n    def b(self, e):\n        return norm(e)\n" \
        "    def c(self, e):\n        return e if e not in BLOCKED else None\n"
    (tmp_path / "m.py").write_text(mixed)
    assert not check_shared_helper(check, tmp_path)["passed"]
    # A helper composed of two private ones, in another module, called as an attribute: passes.
    (tmp_path / "v.py").write_text("def _fmt(e):\n    return RE.match(e)\ndef _dom(e):\n    return e in BLOCKED\n"
                                   "def check(e):\n    _fmt(e); _dom(e)\n    return e\n")
    (tmp_path / "m.py").write_text("import v\nclass D:\n" + "".join(
        f"    def {m}(self, e):\n        return v.check(e)\n" for m in "abc"))
    assert check_shared_helper(check, tmp_path)["details"]["helpers"] == ["check"]
    # A missing caller, or a file that does not parse: fails.
    (tmp_path / "m.py").write_text("class D:\n    def a(self, e):\n        return v.check(e)\n")
    assert check_shared_helper(check, tmp_path)["details"]["callers_missing"] == ["D.b", "D.c"]
