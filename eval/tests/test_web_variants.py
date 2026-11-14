"""Web-task variants and plain craze's home on a web task (craze plan 051 follow-ups).

T-WS1–T-WS6 are T-W1–T-W6 with the docs site unnamed (``variant_of``): the prompt names
only the product and a setup patch takes the address out of the fixture's README, so a
craze-web run has to search. Plain craze's home on a web task turns web search off, which
craze's ``--web-search`` cannot override (C12's plain-arm escape attempts)."""

from __future__ import annotations

import shutil
import tomllib
from dataclasses import replace
from pathlib import Path
from urllib.parse import urlsplit

import pytest

from crazeeval.batch import task_fingerprint, tree_hash
from crazeeval.config import load_models
from crazeeval.homes import WEB_CONFIG, WEB_OFF_CONFIG, craze_home, generated_files
from crazeeval.runners import RUNNERS
from crazeeval.tasks import TaskError, load_task, load_tasks
from crazeeval.validate import web_lint
from crazeeval.workspace import git, materialise
from test_homes_env import fake_snapshot

PAIRS = [(f"T-W{n}", f"T-WS{n}") for n in range(1, 7)]


def hosts(t):
    return {urlsplit(p.url).hostname for p in t.web_corpus.pages}


@pytest.mark.parametrize("base_id,var_id", PAIRS)
def test_a_variant_is_its_base_with_the_docs_site_unnamed(base_id, var_id):
    tasks = load_tasks()
    base, var = tasks[base_id], tasks[var_id]
    assert var.base_dir == base.dir and var.dir.name == var_id and base.base_dir is None
    assert var.name == base.name + "-search"
    # Everything a run is scored on is the base's.
    for f in ("category", "split", "mode", "fixture", "checks", "validate", "harnesses", "timeout_s",
              "relevant", "needs", "corpus", "web_corpus", "rubric"):
        assert getattr(var, f) == getattr(base, f), f
    assert var.testdata("testdata/reference.patch") == base.testdata("testdata/reference.patch")
    # The base names the docs site; the variant names no corpus host, and its lint holds.
    docs = f"docs.{base.fixture}.example"
    assert docs in base.prompt and not any(h in var.prompt.casefold() for h in hosts(var))
    assert base.setup_patch is None and var.setup_patch == var.dir / "testdata" / "setup.patch"
    assert web_lint(base) == [] and web_lint(var) == []
    # A variant's fingerprint folds in its base's tree; the base's is its bare tree hash.
    assert task_fingerprint(base) == tree_hash(base.dir)
    assert task_fingerprint(var) not in (tree_hash(var.dir), tree_hash(base.dir))


def test_a_variants_workspace_has_no_docs_address_and_no_trace_of_one(tmp_path):
    tasks = load_tasks()
    base, var = tasks["T-W3"], tasks["T-WS3"]
    a, b = tmp_path / "base" / "bindlewick", tmp_path / "var" / "bindlewick"
    sa, sb = materialise(base, a), materialise(var, b)
    assert "https://docs.bindlewick.example" in (a / "README.md").read_text()
    assert "docs.bindlewick.example" not in (b / "README.md").read_text()
    # One commit, the fixture's own message: the patch is in it, never a commit of its own.
    assert git(b, "rev-list", "--count", "HEAD").strip() == "1"
    assert git(b, "log", "-1", "--format=%s").strip() == git(a, "log", "-1", "--format=%s").strip()
    assert git(b, "status", "--porcelain").strip() == ""
    assert set(sa.manifest) == set(sb.manifest) and sa.manifest["README.md"] != sb.manifest["README.md"]
    assert {k: v for k, v in sa.manifest.items() if k != "README.md"} == \
        {k: v for k, v in sb.manifest.items() if k != "README.md"}


def test_the_lint_catches_a_docs_address_a_variant_leaves_in():
    var = load_tasks()["T-WS1"]
    # Without its setup patch the fixture's README still names the docs site.
    assert any("'docs.quayside.example' is in the fixture's files/README.md" in p
               for p in web_lint(replace(var, setup_patch=None)))
    assert any("'docs.quayside.example' is in the prompt" in p
               for p in web_lint(replace(var, prompt=var.prompt + " See https://docs.quayside.example.")))
    assert any("'community.harborware.example' is in the prompt" in p
               for p in web_lint(replace(var, prompt=var.prompt + " community.harborware.example")))


def test_the_lint_catches_a_docs_address_in_a_file_name(monkeypatch):
    import crazeeval.validate as v

    var = load_tasks()["T-WS1"]
    real = v._start_files
    monkeypatch.setattr(v, "_start_files", lambda t: real(t) + [("files/docs.quayside.example.txt", "")])
    assert any("'docs.quayside.example' is in the fixture's file name files/docs.quayside.example.txt" in p
               for p in web_lint(var))


def _variant(tmp_path: Path, body: str, base: str = "T-W1") -> Path:
    root = tmp_path / "tasks"
    src = load_tasks()[base].dir
    if not (root / base).exists():
        shutil.copytree(src, root / base)
    d = root / "T-X1"
    (d / "testdata").mkdir(parents=True, exist_ok=True)
    (d / "task.toml").write_text(body)
    return d


def test_the_variant_loader_refuses_what_it_cannot_honour(tmp_path):
    ok = 'id = "T-X1"\nvariant_of = "T-W1"\nprompt = "p"\n'
    assert load_task(_variant(tmp_path, ok)).base_dir.name == "T-W1"
    for body, why in [
        (ok + 'timeout_s = 5\n', "a variant sets only"),
        ('id = "T-X1"\nvariant_of = "T-NOPE"\nprompt = "p"\n', "no such task"),
        ('id = "T-X1"\nvariant_of = "../T-W1"\nprompt = "p"\n', "must name a task directory"),
        ('id = "T-X1"\nvariant_of = "T-W1"\n', "empty prompt"),
        (ok + 'setup_patch = "testdata/none.patch"\n', "setup patch"),
        (ok + 'setup_patch = "../../escape.patch"\n', "escapes the task directory"),
    ]:
        with pytest.raises(TaskError, match=why):
            load_task(_variant(tmp_path, body))
    # A variant of a variant, and of a task outside the web split, are refused.
    root = tmp_path / "tasks"
    (root / "T-X2").mkdir()
    (root / "T-X2" / "task.toml").write_text('id = "T-X2"\nvariant_of = "T-X1"\nprompt = "p"\n')
    _variant(tmp_path, ok)
    with pytest.raises(TaskError, match="is itself a variant"):
        load_task(root / "T-X2")
    shutil.copytree(load_tasks()["T-F1"].dir, root / "T-F1")
    (root / "T-X3").mkdir()
    (root / "T-X3" / "task.toml").write_text('id = "T-X3"\nvariant_of = "T-F1"\nprompt = "p"\n')
    with pytest.raises(TaskError, match="only a web task has variants"):
        load_task(root / "T-X3")


# -- plain craze's home on a web task --------------------------------------------------


def test_plain_crazes_home_turns_web_search_off_on_a_web_task_alone(tmp_path):
    snap = fake_snapshot(tmp_path)
    em = load_models()["glm-5.3-flash"]
    base = f"http://127.0.0.1:4242/r/{'a' * 32}/api.z.ai/api/coding/paas/v4"
    tasks = load_tasks()

    def home(harness, task, name):
        h = tmp_path / name
        env = RUNNERS[harness].home(h, em, snap, base)
        RUNNERS[harness].task_home(h, task)
        return env, {str(p.relative_to(h)): p.read_bytes() for p in generated_files(h)}

    plain_env, plain = home("craze", tasks["T-F1"], "dev")
    craze_home(tmp_path / "bare", em, snap, base)
    bare = {str(p.relative_to(tmp_path / "bare")): p.read_bytes() for p in generated_files(tmp_path / "bare")}
    # Every other task's home is craze_home's, byte for byte.
    assert plain == bare and ".craze/config.toml" not in plain
    for tid in ("T-W1", "T-WS1"):
        env, web_task = home("craze", tasks[tid], f"plain-{tid}")
        assert env == plain_env
        assert web_task == {**bare, ".craze/config.toml": WEB_OFF_CONFIG.encode()}
        assert tomllib.loads(WEB_OFF_CONFIG) == {"websearch": False}
        assert (tmp_path / f"plain-{tid}" / ".craze" / "config.toml").stat().st_mode & 0o777 == 0o600
        # craze-web's home is what it was: web search on.
        _, web = home("craze-web", tasks[tid], f"web-{tid}")
        assert web == {**bare, ".craze/config.toml": WEB_CONFIG.encode()}
    # The other harnesses add nothing.
    for h in ("gx", "opencode", "codex"):
        d = tmp_path / f"{h}-home"
        d.mkdir()
        RUNNERS[h].task_home(d, tasks["T-W1"])
        assert generated_files(d) == []
