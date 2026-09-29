"""Plan 029 W2: campaigns (where runs, the ledger and config snapshots live), a craze-repo
task's own commit, and the batch identity that records both."""

from __future__ import annotations

import dataclasses
import json
import shutil
import subprocess

import pytest

from crazeeval import paths
from crazeeval.batch import (
    FINGERPRINT_BASE_COMMIT,
    BatchConfig,
    BatchDirError,
    batch_identity,
    identity_diff,
    open_batch_dir,
    task_fingerprint,
    tree_hash,
)
from crazeeval.config import Snapshot, load_models
from crazeeval.pricing import load_prices
from crazeeval.tasks import TaskError, load_task, load_tasks
from crazeeval.workspace import craze_template, git, materialise, template_hidden

EXECS = {"craze": {"sha256": "c0ffee", "version": "dev"}}


@pytest.fixture
def home(tmp_path, monkeypatch):
    """A home directory of the test's own and no campaign selected anywhere."""
    h = tmp_path / "home"
    h.mkdir()
    monkeypatch.setenv("HOME", str(h))
    monkeypatch.delenv(paths.CAMPAIGN_ENV, raising=False)
    monkeypatch.delenv(paths.LEGACY_CAMPAIGN_ENV, raising=False)
    paths.set_campaign(None)
    yield h
    paths.set_campaign(None)


def _under(p, root) -> bool:
    return p == root or root in p.parents


# -- campaigns ----------------------------------------------------------------------------


def test_an_unset_campaign_never_resolves_into_the_plan_folder(home, tmp_path):
    default = home / ".craze-eval" / "default"
    assert paths.campaign_dir() == default and paths.campaign_source() == "default"
    got = {
        "campaign": paths.campaign_dir(), "runs": paths.runs_dir(), "ledger": paths.ledger_path(),
        "config": paths.config_root(),
        # The old import-time names resolve lazily to the same.
        "PLAN_DIR": paths.PLAN_DIR, "DEFAULT_LEDGER": paths.DEFAULT_LEDGER,
        "DEFAULT_RUNS_DIR": paths.DEFAULT_RUNS_DIR, "DEFAULT_CONFIG_ROOT": paths.DEFAULT_CONFIG_ROOT,
        "BatchConfig.ledger_path": BatchConfig(harnesses=[], models=[], tasks=[], reps=1, out=tmp_path / "b",
                                               label="t", snap=Snapshot(dir=tmp_path, config={}, hash="x")).ledger_path,
    }
    for name, p in got.items():
        assert _under(p, default), (name, p)
        assert ".claude" not in p.parts and "plans" not in p.parts, (name, p)
    assert paths.runs_dir() == default / "eval-runs" and paths.ledger_path() == default / "ledger.jsonl"
    assert paths.config_root() == default / "eval-config"


def test_campaign_option_env_and_alias_resolve_in_order(home, tmp_path, monkeypatch):
    legacy, explicit = tmp_path / "plan-folder", tmp_path / "explicit"
    monkeypatch.setenv(paths.LEGACY_CAMPAIGN_ENV, str(legacy))
    assert paths.campaign_dir() == legacy and paths.campaign_source() == paths.LEGACY_CAMPAIGN_ENV
    assert paths.ledger_path() == legacy / "ledger.jsonl"  # plan 029's scripts keep working
    monkeypatch.setenv(paths.CAMPAIGN_ENV, str(explicit))
    assert paths.campaign_dir() == explicit and paths.campaign_source() == paths.CAMPAIGN_ENV
    assert paths.runs_dir() == explicit / "eval-runs"
    paths.set_campaign("spring-2027")
    assert paths.campaign_dir() == home / ".craze-eval" / "spring-2027" and paths.campaign_source() == "--campaign"
    assert paths.campaign_name() == "spring-2027"
    paths.set_campaign(None)
    assert paths.campaign_dir() == explicit
    for bad in ("../up", "a/b", "", ".", "..", "-x"):
        with pytest.raises(paths.CampaignError):
            paths.set_campaign(bad)


def test_the_cli_campaign_option_before_or_after_the_command(home, capsys):
    from crazeeval.cli import main

    assert main(["--campaign", "c1", "ledger"]) == 0
    assert (home / ".craze-eval" / "c1" / "ledger.jsonl").is_file()
    assert main(["ledger", "--campaign", "c2"]) == 0
    assert (home / ".craze-eval" / "c2" / "ledger.jsonl").is_file()
    assert main(["--campaign", "../escape", "ledger"]) == 2
    assert "invalid campaign name" in capsys.readouterr().err
    assert main(["ledger"]) == 0  # no option: the default campaign again
    assert (home / ".craze-eval" / "default" / "ledger.jsonl").is_file()
    assert not (home / ".claude").exists()


def test_batch_json_records_the_campaign_outside_the_fingerprint(home, tmp_path):
    ms = load_models()
    tasks = [load_tasks()["smoke-explain"]]

    def cfg(out, explicit_ledger=False):
        return BatchConfig(harnesses=["craze"], models=[ms["glm-5.3-flash"]], tasks=tasks, reps=1, out=out,
                           label="t", snap=Snapshot(dir=tmp_path, config={}, hash="x"),
                           explicit_ledger=explicit_ledger)

    paths.set_campaign("first")
    c = cfg(tmp_path / "b")
    ident = batch_identity(c, EXECS, load_prices())
    open_batch_dir(c, ident).close()
    saved = json.loads((tmp_path / "b" / "batch.json").read_text())
    assert saved["campaign"] == str(home / ".craze-eval" / "first")
    # Another campaign: the same fingerprint (campaign is outside it), but --resume
    # refuses it -- its ledger would come from the new campaign, not "first" -- unless
    # an explicit --ledger was given.
    paths.set_campaign("second")
    c2 = cfg(tmp_path / "b")
    c2.resume = True
    ident2 = batch_identity(c2, EXECS, load_prices())
    assert ident2["fingerprint"] == ident["fingerprint"] and ident2["campaign"] != ident["campaign"]
    assert identity_diff(saved, ident2) == []
    with pytest.raises(BatchDirError, match="ran in campaign"):
        open_batch_dir(c2, ident2)
    # An explicit --ledger is a deliberate choice of ledger: the mismatch is allowed.
    c3 = cfg(tmp_path / "b", explicit_ledger=True)
    c3.resume = True
    ident3 = batch_identity(c3, EXECS, load_prices())
    open_batch_dir(c3, ident3).close()
    assert json.loads((tmp_path / "b" / "batch.json").read_text())["campaign"] == saved["campaign"]


def test_resume_within_the_same_campaign_is_unaffected(home, tmp_path):
    """The common case: --resume in the same campaign it ran in works exactly as
    before the campaign check (no --ledger needed)."""
    ms = load_models()
    tasks = [load_tasks()["smoke-explain"]]
    c = BatchConfig(harnesses=["craze"], models=[ms["glm-5.3-flash"]], tasks=tasks, reps=1, out=tmp_path / "b",
                    label="t", snap=Snapshot(dir=tmp_path, config={}, hash="x"))
    paths.set_campaign("only")
    open_batch_dir(c, batch_identity(c, EXECS, load_prices())).close()
    c2 = BatchConfig(harnesses=["craze"], models=[ms["glm-5.3-flash"]], tasks=tasks, reps=1, out=tmp_path / "b",
                     label="t", snap=Snapshot(dir=tmp_path, config={}, hash="x"), resume=True)
    open_batch_dir(c2, batch_identity(c2, EXECS, load_prices())).close()


def test_resume_of_a_legacy_batch_without_a_recorded_campaign_is_unaffected(home, tmp_path):
    """A batch.json written before campaigns were recorded has no ``campaign`` key:
    --resume never checks it, in any campaign, with no --ledger needed."""
    ms = load_models()
    tasks = [load_tasks()["smoke-explain"]]
    c = BatchConfig(harnesses=["craze"], models=[ms["glm-5.3-flash"]], tasks=tasks, reps=1, out=tmp_path / "b",
                    label="t", snap=Snapshot(dir=tmp_path, config={}, hash="x"))
    paths.set_campaign("first")
    open_batch_dir(c, batch_identity(c, EXECS, load_prices())).close()
    saved_path = tmp_path / "b" / "batch.json"
    saved = json.loads(saved_path.read_text())
    del saved["campaign"]
    saved_path.write_text(json.dumps(saved))

    paths.set_campaign("second")
    c2 = BatchConfig(harnesses=["craze"], models=[ms["glm-5.3-flash"]], tasks=tasks, reps=1, out=tmp_path / "b",
                     label="t", snap=Snapshot(dir=tmp_path, config={}, hash="x"), resume=True)
    open_batch_dir(c2, batch_identity(c2, EXECS, load_prices())).close()


# -- a craze-repo task's own commit -----------------------------------------------------------


def rev(ref: str) -> str:
    return git(paths.REPO_ROOT, "rev-parse", ref).strip()


def _craze_task(root, name: str, commit: str | None = None, patch: bytes | None = None):
    td = root / name
    (td / "testdata").mkdir(parents=True)
    extra = ""
    if patch is not None:
        (td / "testdata" / "setup.patch").write_bytes(patch)
        extra = 'setup_patch = "testdata/setup.patch"\n'
    pin = f'commit = "{commit}"\n' if commit else ""
    (td / "task.toml").write_text(
        f'id = "{name}"\ncategory = "explain"\nsplit = "smoke"\nprompt = "p"\n'
        f'[repo]\nkind = "craze"\n{pin}{extra}'
        '[[checks]]\ntype = "facts"\n[[checks.items]]\nregex = "x"\n')
    return load_task(td)


@pytest.fixture(scope="module")
def cache(tmp_path_factory):
    return tmp_path_factory.mktemp("cache")


def test_repo_commit_parsing(tmp_path):
    assert all(t.commit == paths.CRAZE_TEMPLATE_COMMIT for t in load_tasks().values() if t.repo_kind == "craze")
    assert all(t.commit is None and t.craze_commit is None for t in load_tasks().values() if t.repo_kind == "fixture")
    sha = "0123456789abcdef0123456789abcdef01234567"
    assert _craze_task(tmp_path, "pinned", sha).craze_commit == sha
    assert _craze_task(tmp_path, "unpinned").craze_commit == paths.CRAZE_TEMPLATE_COMMIT
    for i, bad in enumerate(("3eabb31", "main", "3eabb31~1", sha.upper())):
        with pytest.raises(TaskError, match="full 40-character"):
            _craze_task(tmp_path, f"bad{i}", bad)
    fx = tmp_path / "fx"
    shutil.copytree(paths.TASKS_DIR / "smoke-explain", fx / "smoke-explain")
    toml = fx / "smoke-explain" / "task.toml"
    toml.write_text(toml.read_text().replace('kind = "fixture"', f'kind = "fixture"\ncommit = "{sha}"'))
    with pytest.raises(TaskError, match="craze-repo task only"):
        load_task(fx / "smoke-explain")


def test_a_pinned_commit_materialises_that_commits_tree(tmp_path, cache):
    earlier = rev(f"{paths.CRAZE_TEMPLATE_COMMIT}~1")
    task = _craze_task(tmp_path / "tasks", "pin-earlier", earlier)
    ws = tmp_path / "ws" / "craze"
    start = materialise(task, ws, cache=cache)
    assert start.commit == earlier and start.isolation["ok"], start.isolation
    assert start.isolation["craze_commit"] == earlier
    assert git(ws, "rev-parse", "HEAD^{tree}").strip() == rev(f"{earlier}^{{tree}}")
    # A file 3eabb31 added is not there; 3eabb31 itself is not in the object store.
    added = git(paths.REPO_ROOT, "diff", "--name-only", "--diff-filter=A", earlier, paths.CRAZE_TEMPLATE_COMMIT).split()
    assert added and not (ws / added[0]).exists()
    r = subprocess.run(["git", "cat-file", "-e", f"{paths.CRAZE_TEMPLATE_COMMIT}^{{commit}}"], cwd=ws, capture_output=True)
    assert r.returncode != 0
    # The template cache is keyed by commit; the default task is unchanged.
    assert (cache / "templates" / f"craze-{earlier[:12]}").is_dir()
    dflt = materialise(load_tasks()["T-E1"], tmp_path / "ws2" / "craze", cache=cache)
    assert dflt.commit == paths.CRAZE_TEMPLATE_COMMIT and dflt.isolation["ok"]
    assert (cache / "templates" / f"craze-{paths.CRAZE_TEMPLATE_COMMIT[:12]}").is_dir()
    assert not template_hidden(cache / "templates" / f"craze-{earlier[:12]}")
    # The scoring side's pristine copy is the pinned commit too.
    from crazeeval.checks import ScoreContext

    ctx = ScoreContext(task=task, ws=ws, start=start, answer="", records=[], scoring_dir=tmp_path / "scoring",
                       tc=None, ignores=[], ws_inside="/sandbox/work/craze", cache=cache)
    assert git(ctx.pristine(), "rev-parse", "HEAD").strip() == earlier


def test_a_commit_that_holds_the_eval_is_materialised_without_it(tmp_path, cache):
    """A commit after eval/ landed: the workspace holds its tree without eval/ (every
    task's hidden tests and reference answers), as one parentless commit -- the history
    holds eval/ too."""
    commit = rev("HEAD")
    if not git(paths.REPO_ROOT, "ls-tree", "--name-only", commit, "--", "eval").strip():
        pytest.skip("HEAD has no eval/")
    task = _craze_task(tmp_path / "tasks", "pin-head", commit)
    ws = tmp_path / "ws" / "craze"
    start = materialise(task, ws, cache=cache)
    iso = start.isolation
    assert iso["ok"] and iso["hidden"] == ["eval"] and iso["hidden_absent"], iso
    assert not (ws / "eval").exists()
    assert git(ws, "rev-list", "--all").split() == [start.commit] and start.commit != commit
    assert iso["history_commits_present"] == [] and iso["objects_stored"] == iso["objects_reachable"]
    want = {p for p in git(paths.REPO_ROOT, "ls-tree", "-r", "--name-only", commit).split() if not p.startswith("eval/")}
    assert set(git(ws, "ls-tree", "-r", "--name-only", "HEAD").split()) == want
    base = craze_template(commit, cache=cache)
    assert base.name == f"craze-{commit[:12]}-noeval" and template_hidden(base) == ["eval"]
    # A planted task on that commit: the plant applied to the eval/-less tree.
    edit = tmp_path / "edit"
    shutil.copytree(base, edit, symlinks=True)
    with open(edit / "README.md", "a") as f:
        f.write("\nPLANTED LINE\n")
    planted = _craze_task(tmp_path / "tasks", "pin-head-planted", commit, git(edit, "diff").encode())
    ws2 = tmp_path / "ws2" / "craze"
    st2 = materialise(planted, ws2, cache=cache)
    assert st2.isolation["ok"] and st2.isolation["hidden_absent"], st2.isolation
    assert "PLANTED LINE" in (ws2 / "README.md").read_text() and not (ws2 / "eval").exists()
    assert git(ws2, "rev-list", "--all").split() == [st2.commit]


def _commit(repo, msg: str) -> str:
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", msg, date="2026-01-01T12:00:00+00:00")
    return git(repo, "rev-parse", "HEAD").strip()


def test_eval_anywhere_in_the_history_gets_a_history_free_template(tmp_path):
    """A commit whose own tree has no eval/ but an ancestor's did (added, then removed
    -- on the main line, or on a side branch merged in) must not expose it through
    git history: its template is the parentless eval/-less one. A commit with no eval/
    anywhere in its history keeps its history, as 3eabb31 does."""
    repo = tmp_path / "src"
    repo.mkdir()
    git(repo, "init", "-q", "-b", "main")
    (repo / "README.md").write_text("hello\n")
    a = _commit(repo, "A: no eval yet")
    (repo / "eval").mkdir()
    (repo / "eval" / "secret_test.py").write_text("HIDDEN-TEST-SECRET = 1\n")
    b = _commit(repo, "B: eval added")
    secret = git(repo, "rev-parse", f"{b}:eval/secret_test.py").strip()
    shutil.rmtree(repo / "eval")
    (repo / "code.go").write_text("package x\n")
    c = _commit(repo, "C: eval removed")
    # A side branch from A adds and removes eval/; its merge is TREESAME to main for eval/.
    git(repo, "checkout", "-q", "-b", "side", a)
    (repo / "eval").mkdir()
    (repo / "eval" / "side_secret.py").write_text("SIDE-SECRET = 1\n")
    _commit(repo, "S1: eval on a side branch")
    shutil.rmtree(repo / "eval")
    _commit(repo, "S2: gone again")
    git(repo, "checkout", "-q", "-b", "trunk", a)
    (repo / "other.txt").write_text("x\n")
    _commit(repo, "A2")
    git(repo, "merge", "-q", "--no-ff", "-m", "merge side", "side", date="2026-01-01T12:00:00+00:00")
    m = git(repo, "rev-parse", "HEAD").strip()
    assert not git(repo, "ls-tree", "--name-only", c, "--", "eval").strip()
    assert not git(repo, "ls-tree", "--name-only", m, "--", "eval").strip()

    from crazeeval.workspace import hidden_paths

    assert hidden_paths(a, repo) == [] and hidden_paths(b, repo) == ["eval"]
    assert hidden_paths(c, repo) == ["eval"] and hidden_paths(m, repo) == ["eval"]
    cache = tmp_path / "cache"
    for commit in (c, m):
        base = craze_template(commit, source_repo=repo, cache=cache)
        assert base.name == f"craze-{commit[:12]}-noeval" and template_hidden(base) == ["eval"]
        assert len(git(base, "rev-list", "--all").split()) == 1  # one parentless commit
        for gone in (a, b, c, secret):
            r = subprocess.run(["git", "cat-file", "-e", gone], cwd=base, capture_output=True)
            assert r.returncode != 0, gone
        assert not (base / "eval").exists()
        assert (set(git(base, "ls-tree", "-r", "--name-only", "HEAD").split())
                == set(git(repo, "ls-tree", "-r", "--name-only", commit).split()))
    # No eval/ anywhere in A's history: the template keeps the commit and its ancestry.
    plain = craze_template(a, source_repo=repo, cache=cache)
    assert plain.name == f"craze-{a[:12]}" and template_hidden(plain) == []
    assert git(plain, "rev-parse", "HEAD").strip() == a
    # 3eabb31, the default, is exactly as before: its history, no eval/ anywhere in it.
    assert hidden_paths(paths.CRAZE_TEMPLATE_COMMIT) == []


def test_a_task_commit_enters_the_fingerprint_only_off_the_base_commit(tmp_path, home):
    tasks = load_tasks()
    # A task on the base commit (every task so far) keeps its bare tree hash: its
    # fingerprint, and rescore's check against a batch's, is unchanged.
    on_base = [t for t in tasks.values() if t.craze_commit in (None, FINGERPRINT_BASE_COMMIT)]
    assert len(on_base) == len(tasks)
    for t in on_base:
        assert task_fingerprint(t) == tree_hash(t.dir), t.id
    t = tasks["T-E1"]
    moved = dataclasses.replace(t, commit=rev(f"{paths.CRAZE_TEMPLATE_COMMIT}~1"))  # a default that moved
    assert task_fingerprint(moved) != tree_hash(t.dir)
    ms = load_models()

    def ident(ts):
        return batch_identity(BatchConfig(harnesses=["craze"], models=[ms["glm-5.3-flash"]], tasks=ts, reps=1,
                                          out=tmp_path / "b", label="t", snap=Snapshot(dir=tmp_path, config={},
                                                                                       hash="x")), EXECS, load_prices())

    base, pinned = ident([t, tasks["smoke-explain"]]), ident([moved, tasks["smoke-explain"]])
    assert "craze_commits" not in base  # a default batch's identity fields are as before
    assert pinned["craze_commits"] == {"T-E1": moved.commit}
    assert pinned["tasks"]["T-E1"] != base["tasks"]["T-E1"] and pinned["fingerprint"] != base["fingerprint"]
    assert pinned["tasks"]["smoke-explain"] == base["tasks"]["smoke-explain"]
