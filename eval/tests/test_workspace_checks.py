"""Workspaces, manifests, checks and validators (plan 029 §3.1.5)."""

from __future__ import annotations

import asyncio
import json
import os
import shutil
import subprocess
import threading

import pytest

from crazeeval import paths, safefs
from crazeeval.checks import (
    ScoreContext,
    agent_testmains,
    check_executed_code,
    check_facts,
    check_structural_count,
    check_tests,
    contamination_scan,
    match_expected,
    run_tests,
    scoring_copy,
)
from crazeeval.gitpost import git_postrun
from crazeeval.keys import KeyRing, scan_tree
from crazeeval.sandbox import Toolchains, bwrap_available
from crazeeval.tasks import TaskError, load_task, load_tasks
from crazeeval.validate import _synthetic_capture, validate_task
from crazeeval.workspace import (
    build_manifest,
    compare,
    craze_template,
    git,
    ignores_for,
    is_ignored,
    materialise,
    verify_craze_repo,
)

needs_bwrap = pytest.mark.skipif(not bwrap_available(), reason="bubblewrap is not usable here")


@pytest.fixture(scope="module")
def tc():
    if not bwrap_available():
        pytest.skip("bubblewrap is not usable here")
    return Toolchains.resolve()


def test_fixture_materialises_deterministically(tmp_path):
    t = load_tasks()["smoke-explain"]
    a = materialise(t, tmp_path / "a" / "smoke-py")
    b = materialise(t, tmp_path / "b" / "smoke-py")
    assert a.commit == b.commit  # fixed author and date
    assert "inventory.py" in a.manifest and ".gitignore" in a.manifest
    assert not any(p.startswith(".git/") for p in a.manifest)
    assert git(tmp_path / "a" / "smoke-py", "log", "--format=%an <%ae> %aI").strip() == \
        "Eval Fixture <fixture@craze.invalid> 2026-01-01T12:00:00+00:00"
    assert git(tmp_path / "a" / "smoke-py", "remote").strip() == ""


def test_manifest_compare_sees_untracked_and_ignores_generated(tmp_path):
    t = load_tasks()["smoke-explain"]
    ws = tmp_path / "smoke-py"
    start = materialise(t, ws)
    (ws / "notes.txt").write_text("untracked\n")  # untracked, not ignored by git either
    (ws / "__pycache__").mkdir()
    (ws / "__pycache__" / "inventory.cpython-312.pyc").write_bytes(b"x")
    (ws / ".pytest_cache").mkdir()
    (ws / ".pytest_cache" / "v").write_text("x")
    (ws / "README.md").unlink()
    with open(ws / "inventory.py", "a") as f:
        f.write("# edit\n")
    d = compare(start.manifest, build_manifest(ws), ignores_for(t))
    assert d == {"added": ["notes.txt"], "modified": ["inventory.py"], "deleted": ["README.md"]}


def test_manifest_sees_a_mode_change_and_ignored_start_files(tmp_path):
    """Review r1-c1 finding 15: a chmod is a change; a start file matching an ignore
    pattern is still compared (ignores apply to additions only)."""
    t = load_tasks()["smoke-explain"]
    ws = tmp_path / "smoke-py"
    materialise(t, ws)
    os.chmod(ws / "inventory.py", 0o755)
    start_manifest = build_manifest(ws)
    assert start_manifest["inventory.py"].startswith("f:0755:")
    # Full permission bits (review r2-c1 item 14): 0755 -> 0655 keeps "some execute bit".
    os.chmod(ws / "inventory.py", 0o655)
    d = compare(start_manifest, build_manifest(ws), ignores_for(t))
    assert d["modified"] == ["inventory.py"]
    start = type("S", (), {"manifest": start_manifest})
    # A start file that looks generated: its deletion still counts.
    fake_start = dict(start.manifest, **{"build/tool.test": "f:-:abc"})
    d = compare(fake_start, build_manifest(ws), ignores_for(t))
    assert "build/tool.test" in d["deleted"]


def test_postprocessing_never_blocks_on_a_fifo_or_follows_a_link(tmp_path):
    """Review r1-c1 findings 1, 16: a FIFO is classified, never opened; a symlink to a
    secret outside is recorded as a link, never read -- by the manifest, the key scan
    and the scoring copy."""
    secret = tmp_path / "outside" / "providers.toml"
    secret.parent.mkdir()
    secret.write_text('api_key = "sk-FAKE-outside-0123456789"\n')
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "a.txt").write_text("hello\n")
    os.mkfifo(ws / "pipe")
    os.symlink(secret, ws / "leak.toml")
    os.symlink(secret.parent, ws / "linkdir")
    out: dict = {}

    def work():
        out["manifest"] = build_manifest(ws)
        out["scan"] = scan_tree(ws, KeyRing({"p": "sk-FAKE-outside-0123456789"}))
        out["copy"] = safefs.copy_tree(ws, tmp_path / "copy")

    th = threading.Thread(target=work, daemon=True)
    th.start()
    th.join(20)
    assert not th.is_alive(), "post-processing blocked"
    m = out["manifest"]
    assert m["pipe"] == "s:fifo" and m["leak.toml"] == f"l:{secret}" and m["linkdir"] == f"l:{secret.parent}"
    assert not any(k.startswith("linkdir/") for k in m)
    assert out["scan"]["files_with_key"] == [] and "pipe" in out["scan"]["skipped"]
    assert out["copy"]["skipped_special"] == ["pipe"]
    assert (tmp_path / "copy" / "leak.toml").is_symlink()
    assert safefs.read_text(ws, "leak.toml") is None and safefs.read_text(ws, "linkdir/providers.toml") is None
    assert safefs.read_text(ws, "a.txt") == "hello\n"


def test_restores_replace_symlinks_instead_of_writing_through_them(tmp_path):
    """Review r1-c1 finding 3: a restore target (or a directory on its way) that is a
    symlink to an owner file is replaced, never written through; pruning never
    follows a planted link."""
    owner = tmp_path / "owner"
    owner.mkdir()
    (owner / "precious.txt").write_text("keep me\n")
    (owner / "cache").mkdir()
    (owner / "cache" / "big").write_text("keep me too\n")
    dst = tmp_path / "copy"
    (dst / "tests").mkdir(parents=True)
    os.symlink(owner / "precious.txt", dst / "tests" / "test_inventory.py")
    os.symlink(owner, dst / "sub")
    safefs.write_bytes(dst, "tests/test_inventory.py", b"trusted\n")
    safefs.write_bytes(dst, "sub/precious.txt", b"trusted\n")
    assert (owner / "precious.txt").read_text() == "keep me\n"
    assert not (dst / "tests" / "test_inventory.py").is_symlink()
    assert (dst / "tests" / "test_inventory.py").read_text() == "trusted\n"
    assert not (dst / "sub").is_symlink() and (dst / "sub" / "precious.txt").read_text() == "trusted\n"
    # Pruning ".xdg/config/opencode/node_modules" through a planted ".xdg" -> owner link.
    from crazeeval.batch import _prune

    adir = tmp_path / "attempt"
    (adir / "home").mkdir(parents=True)
    os.symlink(owner, adir / "home" / ".xdg")
    (owner / "config" / "opencode").mkdir(parents=True)
    (owner / "config" / "opencode" / "node_modules").mkdir()
    assert _prune(adir, ["home/.xdg/config/opencode/node_modules", "home/.xdg"]) == {"home/.xdg": 0}
    assert (owner / "config" / "opencode" / "node_modules").is_dir() and (owner / "cache" / "big").exists()
    assert not (adir / "home" / ".xdg").exists()  # the link itself was removed, not its target


def test_structural_count_reads_no_link(tmp_path):
    outside = tmp_path / "outside.py"
    outside.write_text("def validate():\n" * 5)
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "a.py").write_text("def validate():\n")
    os.symlink(outside, ws / "b.py")
    r = check_structural_count({"type": "structural_count", "glob": "*.py", "regex": r"def validate", "value": 1}, ws)
    assert r["passed"] and r["details"]["count"] == 1


def test_ignore_patterns():
    pats = ["__pycache__/", "*.pyc", ".pytest_cache/", "*.test", ".opencode/"]
    assert is_ignored("a/__pycache__/x.py", pats)
    assert is_ignored("x.pyc", pats) and is_ignored("pkg/foo.test", pats)
    assert is_ignored(".opencode/plans/p.md", pats)
    assert not is_ignored("tests/test_x.py", pats) and not is_ignored("pycache.py", pats)


def test_facts_and_executed_code():
    c = {"type": "facts", "name": "f", "items": [{"id": "a", "regex": "KESTREL-7"}, {"id": "b", "regex": r"\bNone\b"}]}
    assert check_facts(c, "It uses KESTREL-7 and returns None.")["passed"]
    r = check_facts(c, "It uses HERON-2.")
    assert not r["passed"] and r["details"]["satisfied"] == 0
    e = {"type": "executed_code", "name": "e", "patterns": ["python|pytest"]}
    assert check_executed_code(e, _synthetic_capture("python -m pytest -q"))["passed"]
    assert not check_executed_code(e, _synthetic_capture(None))["passed"]
    assert not check_executed_code(e, _synthetic_capture("ls -la"))["passed"]


def _call(name, args):
    return {"seq": 1, "path": "/chat/completions", "request": {}, "response": {"events": [
        {"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "id": "c", "function": {"name": name, "arguments": json.dumps(args)}}]}}]}]}}


def test_contamination_scan():
    ws = "/sandbox/work/craze"
    clean = [
        _call("read", {"file_path": "/sandbox/work/craze/internal/harness/testdata/system_prompt.golden"}),
        _call("grep", {"pattern": "github.com/charliek/craze/internal/harness", "path": "internal"}),
        _call("bash", {"command": "go test ./internal/harness/... && grep -rn .craze/native internal/"}),
        _call("read", {"file_path": "internal/harness/tool/opencode/testdata/specs.golden"}),
        # craze's plan mode writes its plan into the run's own (sandbox) home.
        _call("write", {"filePath": "/sandbox/home/.craze/native/sessions/--w--/20260927T1_x.plan.md", "content": "# Plan"}),
    ]
    assert contamination_scan(clean, ws) == []
    dirty = {
        "eval-dir": _call("bash", {"command": "ls ../../eval/tasks"}),
        "reference-patch": _call("read", {"file_path": "/x/reference.patch"}),
        "eval-runs": _call("bash", {"command": "find / -name eval-runs"}),
        "claude-plans": _call("bash", {"command": "cat ~/.claude/plans/craze/x.md"}),
        "craze-native": _call("read", {"file_path": "/home/charliek/.craze/native/providers.toml"}),
        "craze-github": _call("bash", {"command": "curl -sL https://github.com/charliek/craze/archive/main.zip"}),
        "outside-testdata-or-tasks": _call("read", {"file_path": "/opt/tasks/b1/testdata/hidden_test.py"}),
    }
    for rule, rec in dirty.items():
        hits = contamination_scan([rec], ws)
        assert rule in {h["rule"] for h in hits}, rule


def test_craze_template_isolation(tmp_path):
    tmpl = craze_template(cache=tmp_path / "cache")
    check = verify_craze_repo(tmpl, paths.CRAZE_TEMPLATE_COMMIT, paths.REPO_ROOT)
    assert check["ok"], check
    assert check["refs"] == ["refs/heads/main"] and check["remotes"] == [] and not check["alternates"]
    assert git(tmpl, "rev-parse", "HEAD").strip() == paths.CRAZE_TEMPLATE_COMMIT
    assert not (tmpl / "eval").exists()
    assert not (tmpl / ".git" / "FETCH_HEAD").exists()
    # A commit that is not in 3eabb31's ancestry cannot be read from the template.
    later = subprocess.run(["git", "rev-parse", "HEAD"], cwd=paths.REPO_ROOT, capture_output=True, text=True).stdout.strip()
    if later != paths.CRAZE_TEMPLATE_COMMIT:
        r = subprocess.run(["git", "cat-file", "-e", f"{later}^{{commit}}"], cwd=tmpl, capture_output=True)
        assert r.returncode != 0


@needs_bwrap
def test_postrun_git_is_sandboxed_and_ignores_agent_filters(tmp_path, tc):
    """Review r1-c1 finding 2: the agent's .git/config and .gitattributes name a clean
    filter, a textconv and an fsmonitor hook that would write a marker on the host.
    Post-run git runs in a sandbox, in its own repository: none of them runs here,
    and the diff still shows the real change."""
    t = load_tasks()["smoke-explain"]
    ws = tmp_path / "run" / "ws" / "smoke-py"
    start = materialise(t, ws)
    marker = tmp_path / "pwned"
    evil = f"sh -c 'touch {marker}; cat'"
    with open(ws / ".git" / "config", "a") as f:
        f.write(f'[filter "evil"]\n\tclean = {evil}\n\tsmudge = {evil}\n[diff "evil"]\n\ttextconv = {evil}\n'
                f"[core]\n\tfsmonitor = {evil}\n")
    (ws / ".gitattributes").write_text("* filter=evil diff=evil\n")
    with open(ws / "inventory.py", "a") as f:
        f.write("# agent edit\n")
    (ws / "new.txt").write_text("new file\n")
    trusted = tmp_path / "pristine" / "smoke-py"
    materialise(t, trusted)
    post = asyncio.run(git_postrun(ws, "/sandbox/work/smoke-py", start.commit, trusted, tmp_path / "run" / "gitpost", tc))
    assert not marker.exists()
    assert "+# agent edit" in post["diff"] and "new.txt" in post["diff"] and ".gitattributes" in post["diff"]
    assert post["head"] == start.commit and post["moved"] is False
    assert not (ws / ".git" / "crazeeval-diff.index").exists()
    # An agent commit is seen as a moved HEAD with a tree diff.
    git(ws, "-c", "core.fsmonitor=false", "add", "inventory.py")
    git(ws, "-c", "core.fsmonitor=false", "commit", "-q", "-m", "agent")
    marker.unlink(missing_ok=True)  # host-side git in this test itself may run the filter
    post = asyncio.run(git_postrun(ws, "/sandbox/work/smoke-py", start.commit, trusted, tmp_path / "run" / "gitpost2", tc))
    assert post["moved"] is True and "inventory.py" in post["tree_diff"]
    assert not marker.exists()


@needs_bwrap
def test_trusted_test_restore_defeats_a_weakened_test(tmp_path, tc):
    t = load_tasks()["smoke-fix"]
    ws = tmp_path / "run" / "ws" / "smoke-py"
    start = materialise(t, ws)
    # The agent "fixes" nothing, deletes the hidden test's target file content and
    # weakens the visible tests to pass trivially.
    (ws / "tests" / "test_inventory.py").write_text("def test_ok():\n    assert True\n")
    ctx = ScoreContext(task=t, ws=ws, start=start, answer="", records=[], scoring_dir=tmp_path / "run" / "scoring",
                       tc=tc, ignores=ignores_for(t), ws_inside="/sandbox/work/smoke-py")
    r = asyncio.run(check_tests(t.checks[0], ctx))
    assert not r["passed"]
    assert "tests/test_inventory.py" in r["details"]["restored"]
    # The restored trusted file really is the original.
    assert "def test_ok" not in (tmp_path / "run" / "scoring" / "tests-hidden" / "tests" / "test_inventory.py").read_text()
    # Now a real fix passes, with the trusted tests restored and the hidden test added.
    git(ws, "apply", "-", input=t.testdata("testdata/reference.patch").read_bytes())
    r = asyncio.run(check_tests(t.checks[0], ctx))
    assert r["passed"], r["details"]["tail"]
    assert (ws / "tests" / "test_inventory.py").read_text().startswith("def test_ok")  # the final workspace is untouched
    assert all(v == "passed" for v in r["details"]["expected"].values())


@needs_bwrap
def test_a_shadowing_pytest_or_conftest_cannot_pass_the_tests(tmp_path, tc):
    """Review r1-c1 finding 14: total() is still broken; a root pytest.py exiting 0 and
    a conftest.py hook forcing success must not make the hidden tests pass."""
    t = load_tasks()["smoke-fix"]
    ws = tmp_path / "run" / "ws" / "smoke-py"
    start = materialise(t, ws)
    (ws / "pytest.py").write_text("raise SystemExit(0)\n")
    (ws / "tests" / "conftest.py").write_text(
        "import pytest\n@pytest.hookimpl(hookwrapper=True)\ndef pytest_runtest_makereport(item, call):\n"
        "    out = yield\n    rep = out.get_result()\n    rep.outcome = 'passed'\n"
        "def pytest_sessionfinish(session, exitstatus):\n    session.exitstatus = 0\n")
    (ws / "sitecustomize.py").write_text("import os; os._exit(0)\n")
    ctx = ScoreContext(task=t, ws=ws, start=start, answer="", records=[], scoring_dir=tmp_path / "run" / "scoring",
                       tc=tc, ignores=ignores_for(t), ws_inside="/sandbox/work/smoke-py")
    r = asyncio.run(check_tests(t.checks[0], ctx))
    assert not r["passed"]
    assert "removed pytest.py" in r["details"]["sanitized"] and "removed tests/conftest.py" in r["details"]["sanitized"]
    assert r["details"]["expected"]["tests/test_hidden_total.py::test_total_counts_every_line"] == "failed"


def test_a_passing_exit_is_not_enough_without_the_expected_tests():
    assert match_expected("pytest", {}, ["tests/t.py::test_a"]) == {"tests/t.py::test_a": "missing"}
    res = {"example.com/m/pkg::TestA": "passed", "example.com/m/pkg::TestA/sub": "failed"}
    assert match_expected("go", res, ["pkg::TestA"]) == {"pkg::TestA": "passed"}
    assert match_expected("go", res, ["::TestA/sub"]) == {"::TestA/sub": "failed"}
    assert match_expected("go", res, ["other::TestA"]) == {"other::TestA": "missing"}


def _go_module(root, test_src):
    root.mkdir(parents=True)
    (root / "go.mod").write_text("module example.com/m\n\ngo 1.22\n")
    (root / "calc.go").write_text("package m\n\nfunc Add(a, b int) int { return a - b }\n")
    (root / "calc_test.go").write_text(test_src)


GO_TEST = "package m\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
GO_TESTMAIN = GO_TEST + "\nfunc TestMain(m *testing.M) {}\n"


def test_agent_testmain_is_found():
    import tempfile
    from pathlib import Path

    with tempfile.TemporaryDirectory() as d:
        d = Path(d)
        _go_module(d / "pristine", GO_TEST)
        _go_module(d / "agent", GO_TESTMAIN)
        assert agent_testmains(d / "agent", d / "pristine") == ["calc_test.go"]
        assert agent_testmains(d / "pristine", d / "pristine") == []


@needs_bwrap
def test_go_runner_rejects_testmain_and_reads_json(tmp_path, tc):
    """Review r1-c1 finding 14 (Go): an agent TestMain that exits 0 is rejected; the
    expected test must report a pass in ``go test -json``."""
    t = load_tasks()["smoke-fix"]
    _go_module(tmp_path / "pristine", GO_TEST)
    ctx = ScoreContext(task=t, ws=tmp_path / "pristine", start=None, answer="", records=[],
                       scoring_dir=tmp_path / "scoring", tc=tc, ignores=[], ws_inside="/sandbox/work/m",
                       _pristine=tmp_path / "pristine")
    _go_module(tmp_path / "agent", GO_TESTMAIN)
    dst = scoring_copy(ctx, tmp_path / "agent", "go-agent")
    r = asyncio.run(run_tests(ctx, dst, "go-agent", "go", ["./..."], 300))
    assert r["rejected"] and "calc_test.go" in r["rejected"]
    dst = scoring_copy(ctx, tmp_path / "pristine", "go-pristine")
    r = asyncio.run(run_tests(ctx, dst, "go-pristine", "go", ["./..."], 300))
    assert r["exit"] != 0 and match_expected("go", r["results"], ["::TestAdd"]) == {"::TestAdd": "failed"}


def test_tests_checks_need_a_trusted_runner_and_expected_ids(tmp_path):
    src = paths.TASKS_DIR / "smoke-fix"
    dst = tmp_path / "tasks" / "smoke-fix"
    shutil.copytree(src, dst)
    text = (dst / "task.toml").read_text()
    (dst / "task.toml").write_text(text.replace('runner = "pytest"', 'command = ["true"]'))
    with pytest.raises(TaskError):
        load_task(dst)


@needs_bwrap
@pytest.mark.parametrize("task_id", ["smoke-explain", "smoke-fix"])
def test_smoke_validators(task_id, tc):
    v = asyncio.run(validate_task(load_tasks()[task_id], tc))
    assert v.ok, v.to_dict()
    names = [c.name for c in v.controls]
    if task_id == "smoke-explain":
        assert any("decoy" in n for n in names) and any("planted" in n for n in names)
    else:
        assert any("untouched" in n for n in names) and any("reference.patch" in n for n in names)


def test_validator_catches_a_broken_task(tmp_path, monkeypatch):
    """A decoy that passes the facts makes the task invalid."""
    src = paths.TASKS_DIR / "smoke-explain"
    dst = tmp_path / "tasks" / "smoke-explain"
    shutil.copytree(src, dst)
    (dst / "testdata" / "decoy_answer.md").write_text("KESTREL-7 and None\n")
    from crazeeval.tasks import load_task

    v = asyncio.run(validate_task(load_task(dst), None))
    assert not v.ok
    bad = [c for c in v.controls if not c.ok]
    assert bad and "decoy" in bad[0].name
