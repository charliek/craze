"""Review r2-c1: private Go caches for scoring, trusted test directories, post-run
limits, the orphan import commit for planted tasks, and accepted git evidence
(items E, F, G, I, J)."""

from __future__ import annotations

import asyncio
import os
import shutil
import subprocess

import pytest

from crazeeval import checks as checksmod
from crazeeval import paths, safefs
from crazeeval.batch import classify, gitpost_problem
from crazeeval.checks import ScoreContext, check_tests, sanitize_trusted_dirs, trusted_dirs
from crazeeval.gitpost import git_postrun
from crazeeval.runners.base import Extract
from crazeeval.sandbox import SandboxResult, Toolchains, bwrap_available
from crazeeval.tasks import load_task, load_tasks
from crazeeval.workspace import build_manifest, craze_template, git, ignores_for, materialise, oversize

needs_bwrap = pytest.mark.skipif(not bwrap_available(), reason="bubblewrap is not usable here")


@pytest.fixture(scope="module")
def tc():
    if not bwrap_available():
        pytest.skip("bubblewrap is not usable here")
    return Toolchains.resolve()


# -- E: a private Go cache per scoring invocation -----------------------------------------


def test_every_scoring_invocation_gets_its_own_go_cache(tmp_path, monkeypatch):
    seen = []

    async def fake_run(spec, tc, cmd, **kw):
        seen.append(spec.gocache)
        assert spec.gocache.is_dir() and not any(spec.gocache.iterdir())  # fresh and empty
        (spec.gocache / "planted").write_text("left by submitted code")
        kw["stdout"].write_text("ok")
        kw["stderr"].write_text("")
        return SandboxResult(0, False, 0.1, None, True, [], {}, [])

    monkeypatch.setattr(checksmod, "run_sandboxed", fake_run)
    t = load_tasks()["smoke-fix"]
    ctx = ScoreContext(task=t, ws=tmp_path, start=None, answer="", records=[], scoring_dir=tmp_path / "scoring",
                       tc=object(), ignores=[], ws_inside="/sandbox/work/x")
    for label in ("one", "two"):
        asyncio.run(checksmod.exec_scoring(ctx, tmp_path, label, ["true"], 10))
    assert len(set(seen)) == 2 and all(str(g).startswith(str(tmp_path / "scoring")) for g in seen)
    assert not any(g.exists() for g in seen)  # deleted: nothing survives into the next run
    assert not hasattr(Toolchains, "gocache") and "gocache" not in Toolchains.__dataclass_fields__


# -- F: trusted test directories ------------------------------------------------------------


def test_trusted_dirs_are_derived_from_the_test_files_never_the_root():
    t = load_tasks()["smoke-fix"]
    assert trusted_dirs(t.checks[0]) == ["tests"]
    assert trusted_dirs({"add": {"test_root.py": "x"}, "restore_from_start": ["conftest.py"]}) == []
    assert trusted_dirs({"trusted_dirs": ["pkg/tests/"], "add": {}}) == ["pkg/tests"]


def test_trusted_dirs_hold_only_the_trusted_set(tmp_path):
    t = load_tasks()["smoke-fix"]
    pristine = tmp_path / "pristine" / "smoke-py"
    materialise(t, pristine)
    dst = tmp_path / "copy"
    shutil.copytree(pristine, dst)
    (dst / "tests" / "__init__.py").write_text("import _pytest.python\n")
    (dst / "tests" / "helper.py").write_text("X = 1\n")
    (dst / "tests" / "sub").mkdir()
    (dst / "tests" / "sub" / "evil.py").write_text("")
    (dst / "tests" / "test_inventory.py").write_text("def test_ok():\n    pass\n")
    (dst / "inventory.py").write_text("# the agent's implementation stays\n")
    acts = sanitize_trusted_dirs(dst, pristine, ["tests"], {"tests/test_hidden_total.py"}, "pytest")
    assert sorted(p.name for p in (dst / "tests").rglob("*") if p.is_file()) == ["test_inventory.py"]
    assert (dst / "tests" / "test_inventory.py").read_bytes() == (pristine / "tests" / "test_inventory.py").read_bytes()
    assert (dst / "inventory.py").read_text() == "# the agent's implementation stays\n"
    assert "removed tests/__init__.py" in acts and "restored tests/test_inventory.py" in acts


@needs_bwrap
def test_a_tests_package_initializer_cannot_patch_pytest(tmp_path, tc):
    """Item F: total() is still broken; tests/__init__.py replaces Function.runtest so
    every test "passes" without running. The check must fail."""
    t = load_tasks()["smoke-fix"]
    ws = tmp_path / "run" / "ws" / "smoke-py"
    start = materialise(t, ws)
    (ws / "tests" / "__init__.py").write_text(
        "import _pytest.python\n_pytest.python.Function.runtest = lambda self: None\n")
    ctx = ScoreContext(task=t, ws=ws, start=start, answer="", records=[], scoring_dir=tmp_path / "run" / "scoring",
                       tc=tc, ignores=ignores_for(t), ws_inside="/sandbox/work/smoke-py")
    r = asyncio.run(check_tests(t.checks[0], ctx))
    assert not r["passed"]
    assert "removed tests/__init__.py" in r["details"]["sanitized"]
    assert r["details"]["expected"]["tests/test_hidden_total.py::test_total_counts_every_line"] == "failed"


# -- G: limits, tails, permissions ------------------------------------------------------------


def test_copy_refuses_an_oversized_file_or_tree(tmp_path):
    src = tmp_path / "ws"
    src.mkdir()
    (src / "small.txt").write_text("x")
    with open(src / "sparse.bin", "wb") as f:
        f.truncate(60 * 1024 * 1024)  # 60 MiB apparent, nothing on disk
    with pytest.raises(safefs.Oversize):
        safefs.copy_tree(src, tmp_path / "copy")
    assert not (tmp_path / "copy").exists()  # refused before anything was copied
    m = build_manifest(src)
    assert m["sparse.bin"].startswith("o:") and oversize(m) == ["sparse.bin"]  # never partly hashed
    (src / "sparse.bin").unlink()
    for i in range(4):
        (src / f"f{i}").write_bytes(b"y" * 1000)
    with pytest.raises(safefs.Oversize):
        safefs.copy_tree(src, tmp_path / "copy2", file_limit=2000, tree_limit=3000)
    safefs.copy_tree(src, tmp_path / "copy3", file_limit=2000, tree_limit=10_000)


def test_log_tails_are_read_by_seeking(tmp_path):
    p = tmp_path / "big.log"
    with open(p, "wb") as f:
        f.truncate(200 * 1024 * 1024)
        f.seek(0, os.SEEK_END)
        f.write(b"the end\n")
    assert safefs.tail_text(p, 8) == "the end\n"


def test_the_aggregate_size_is_checked_before_the_manifest_is_hashed(tmp_path, monkeypatch):
    """Review r3-c1 item G: many files each under the per-file limit but well over the
    aggregate limit fail as oversize from the cheap lstat walk alone -- the hash
    function is never called."""
    from crazeeval import batch as batchmod

    ws = tmp_path / "ws"
    ws.mkdir()
    for i in range(6):
        (ws / f"f{i}.bin").write_bytes(b"x" * (10 * 1024))  # 10 KiB each: none individually oversize

    def boom(*a, **k):
        raise AssertionError("sha256_regular must not run before the aggregate size check")

    monkeypatch.setattr(safefs, "sha256_regular", boom)
    final, diff, too_big, workspace_bytes = batchmod.final_manifest(ws, {}, [], tree_limit=50 * 1024)
    assert workspace_bytes == 6 * 10 * 1024
    assert final == {} and diff == {"added": [], "modified": [], "deleted": []}
    assert too_big and "workspace" in too_big[0]

    # Under the (patched) limit, the manifest builds and hashes normally.
    monkeypatch.undo()
    final, diff, too_big, workspace_bytes = batchmod.final_manifest(ws, {}, [], tree_limit=10 * 1024 * 1024)
    assert not too_big and len(final) == 6 and all(v.startswith("f:") for v in final.values())


def test_an_oversize_workspace_fails_the_run():
    ok = SandboxResult(0, False, 1.0, None, True, [], {"key_hits": 0}, [])
    ext = Extract(answer="done", completed=True)
    assert classify(ok, None, {"flags": []}, ext, [{"status": 200}], [], None, oversize=["big.bin"]) == "oversize"


# -- I: the planted task's orphan commit --------------------------------------------------------


@pytest.fixture(scope="module")
def cache(tmp_path_factory):
    return tmp_path_factory.mktemp("cache")


def test_a_planted_task_has_one_parentless_commit_and_no_history(tmp_path, cache):
    """Item I (plan X7): the planted change is not visible in git: a fresh object store,
    one parentless commit of the patched tree, 3eabb31 and its history absent."""
    base = craze_template(cache=cache)
    edit = tmp_path / "edit"
    shutil.copytree(base, edit, symlinks=True)
    with open(edit / "README.md", "a") as f:
        f.write("\nPLANTED LINE\n")
    patch = git(edit, "diff").encode()
    td = tmp_path / "tasks" / "x7-probe"
    (td / "testdata").mkdir(parents=True)
    (td / "testdata" / "setup.patch").write_bytes(patch)
    (td / "task.toml").write_text(
        'id = "x7-probe"\ncategory = "explain"\nsplit = "smoke"\nprompt = "p"\n'
        '[repo]\nkind = "craze"\nsetup_patch = "testdata/setup.patch"\n'
        '[[checks]]\ntype = "facts"\n[[checks.items]]\nregex = "x"\n')
    task = load_task(td)
    ws = tmp_path / "ws" / "craze"
    start = materialise(task, ws, cache=cache)
    assert start.isolation["ok"], start.isolation
    assert git(ws, "rev-list", "--all").split() == [start.commit]
    assert git(ws, "rev-list", "--max-parents=0", "--all").split() == [start.commit]
    r = subprocess.run(["git", "cat-file", "-e", f"{paths.CRAZE_TEMPLATE_COMMIT}^{{commit}}"], cwd=ws, capture_output=True)
    assert r.returncode != 0
    assert start.isolation["history_commits_checked"] > 100 and start.isolation["history_commits_present"] == []
    assert start.isolation["objects_stored"] == start.isolation["objects_reachable"]
    assert "PLANTED LINE" in (ws / "README.md").read_text()
    assert git(ws, "status", "--porcelain").strip() == ""
    # git show HEAD reveals no previous version of anything: the one commit only adds
    # files (no deleted line anywhere), so the planted edit reads as original content.
    numstat = [line.split("\t") for line in git(ws, "show", "--numstat", "--format=", "HEAD").splitlines() if line]
    assert numstat and all(d in ("0", "-") for _, d, _ in numstat)
    # A task without a setup.patch keeps 3eabb31's ancestry.
    plain = materialise(load_tasks()["smoke-explain"], tmp_path / "fixture" / "smoke-py", cache=cache)
    assert plain.commit


# -- J: post-run git evidence must be accepted -------------------------------------------------------


def test_failed_git_evidence_is_an_infrastructure_failure():
    assert gitpost_problem({"exit": 0, "timed_out": False, "diff_rc": "0"}) is None
    assert "timed out" in gitpost_problem({"exit": None, "timed_out": True, "diff_rc": ""})
    assert "exited 1" in gitpost_problem({"exit": 1, "timed_out": False, "diff_rc": "0"})
    assert "read-tree" in gitpost_problem({"exit": 0, "timed_out": False, "diff_rc": "read-tree"})
    ok = SandboxResult(0, False, 1.0, None, True, [], {"key_hits": 0}, [])
    ext = Extract(answer="done", completed=True)
    assert classify(ok, None, {"flags": []}, ext, [{"status": 200}], [], None, evidence_error="diff failed") == "infra"


def test_gitpost_add_failure_is_accepted_only_for_the_embedded_repo_case():
    """Review r3-c1 item J: ``--ignore-errors`` stays (an embedded repository with no
    commit is legitimate to skip), but any other ``git add`` staging error makes the
    attempt ``infra`` instead of being silently accepted."""
    from crazeeval.batch import gitpost_note
    from crazeeval.gitpost import add_errors_benign

    embedded = {"exit": 0, "timed_out": False, "diff_rc": "0", "add_rc": "1",
                "add_errors": "error: 'sub/' does not have a commit checked out\n"}
    assert add_errors_benign(embedded["add_errors"])
    assert gitpost_problem(embedded) is None
    assert gitpost_note(embedded) == "post-run git: skipped an embedded repository with no commit (add_rc=1)"

    # An unreadable file (or any other staging error) is not the accepted case.
    unreadable = {"exit": 0, "timed_out": False, "diff_rc": "0", "add_rc": "1",
                  "add_errors": 'error: open("unreadable.txt"): Permission denied\n'
                                 "error: unable to index file 'unreadable.txt'\n"}
    assert not add_errors_benign(unreadable["add_errors"])
    problem = gitpost_problem(unreadable)
    assert problem is not None and "post-run git add failed" in problem
    assert gitpost_note(unreadable) is None

    # A mix of the benign line and something else is not accepted either.
    mixed = {"exit": 0, "timed_out": False, "diff_rc": "0", "add_rc": "1",
             "add_errors": "error: 'sub/' does not have a commit checked out\n"
                            "error: unable to index file 'x'\n"}
    assert not add_errors_benign(mixed["add_errors"])
    assert gitpost_problem(mixed) is not None

    ok = {"exit": 0, "timed_out": False, "diff_rc": "0", "add_rc": "0", "add_errors": ""}
    assert gitpost_problem(ok) is None and gitpost_note(ok) is None


@needs_bwrap
def test_gitpost_reports_a_failed_diff_and_skips_an_unindexable_repo(tmp_path, tc):
    t = load_tasks()["smoke-explain"]
    ws = tmp_path / "ws" / "smoke-py"
    start = materialise(t, ws)
    trusted = tmp_path / "trusted" / "smoke-py"
    materialise(t, trusted)
    # A start commit the trusted objects do not hold: the diff cannot be made.
    post = asyncio.run(git_postrun(ws, "/sandbox/work/smoke-py", "0" * 40, trusted, tmp_path / "g1", tc))
    assert post["diff_rc"] == "read-tree" and gitpost_problem(post)
    # An embedded repository with no commit is skipped, the rest of the diff stands.
    (ws / "sub").mkdir()
    subprocess.run(["git", "init", "-q"], cwd=ws / "sub", check=True, capture_output=True,
                   env={"PATH": "/usr/bin:/bin", "GIT_CONFIG_GLOBAL": "/dev/null", "HOME": str(tmp_path)})
    (ws / "notes.txt").write_text("new\n")
    post = asyncio.run(git_postrun(ws, "/sandbox/work/smoke-py", start.commit, trusted, tmp_path / "g2", tc))
    assert gitpost_problem(post) is None, post
    assert "notes.txt" in post["diff"]
