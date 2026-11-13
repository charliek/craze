"""The craze-web arm, the web split and its first task (craze plan 051 E2, E4; C9)."""

from __future__ import annotations

import asyncio
import shutil
import tomllib
from dataclasses import replace
from pathlib import Path

import pytest

from crazeeval import paths
from crazeeval.config import DEFAULT_HARNESSES, HARNESSES, SUPPORTED_HARNESSES, load_models
from crazeeval.homes import EXA_MCP_ENV, HOMES, WEB_CONFIG, craze_home, craze_web_home, generated_files, web_mcp_url
from crazeeval.pack import wanted_file
from crazeeval.runners import RUNNERS
from crazeeval.tasks import TaskError, load_task, load_tasks, select_tasks
from crazeeval.validate import web_lint
from fakes import rig
from test_homes_env import fake_snapshot


def test_default_harnesses_and_task_selection_unchanged():
    assert DEFAULT_HARNESSES == HARNESSES == ("craze", "gx", "opencode", "codex")
    assert SUPPORTED_HARNESSES == DEFAULT_HARNESSES + ("craze-web",)
    assert set(RUNNERS) == set(HOMES) == set(SUPPORTED_HARNESSES)
    tasks = load_tasks()
    assert tasks["T-W1"].split == "web"
    # The default selection and split:all hold no web task: every existing campaign's
    # selection is what it was.
    assert select_tasks(tasks, None) == [t for t in tasks.values() if t.split in ("dev", "heldout")]
    assert [t.id for t in select_tasks(tasks, "split:all")] == [t.id for t in tasks.values() if t.split != "web"]
    assert [t.id for t in select_tasks(tasks, "split:web")] == ["T-W1"]
    assert [t.id for t in select_tasks(tasks, "T-W1")] == ["T-W1"]


def test_craze_web_runs_only_web_tasks_and_only_when_named():
    from crazeeval.batch import BatchConfig, plan_runs

    tasks = load_tasks()
    em = load_models()["glm-5.3-flash"]
    web, dev = tasks["T-W1"], tasks["T-F1"]
    assert RUNNERS["craze-web"].supports(em, web) and not RUNNERS["craze-web"].supports(em, dev)
    assert RUNNERS["craze"].supports(em, web)
    assert not RUNNERS["gx"].supports(em, web) and not RUNNERS["opencode"].supports(em, web)
    assert RUNNERS["craze-web"].web and not any(RUNNERS[h].web for h in DEFAULT_HARNESSES)

    def runs(harnesses, chosen):
        cfg = BatchConfig(label="t", out=Path("/nonexistent"), harnesses=list(harnesses), models=[em], tasks=chosen,
                          reps=1, snap=None, ledger_path=Path("/nonexistent/ledger.jsonl"))
        return [(r.harness, r.task.id) for r in plan_runs(cfg)]

    # The default harnesses on a web task: craze alone, never craze-web.
    assert runs(DEFAULT_HARNESSES, [web]) == [("craze", "T-W1")]
    assert runs(("craze", "craze-web"), [web, dev]) == [("craze", "T-W1"), ("craze-web", "T-W1"), ("craze", "T-F1")]


def test_a_craze_web_run_requires_craze_bin(capsys):
    from crazeeval.cli import main

    assert main(["run", "--harness", "craze-web", "--tasks", "split:web"]) == 2
    assert "--craze-bin is required" in capsys.readouterr().err
    assert main(["run", "--harness", "gx,craze-web"]) == 2
    assert "--craze-bin is required" in capsys.readouterr().err
    assert main(["run", "--harness", "craze-webb", "--craze-bin", "/bin/true"]) == 2
    assert "unknown harness 'craze-webb'" in capsys.readouterr().err


def test_the_generated_home(tmp_path):
    snap = fake_snapshot(tmp_path)
    em = load_models()["glm-5.3-flash"]
    base = f"http://127.0.0.1:4242/r/{'a' * 32}/api.z.ai/api/coding/paas/v4"
    plain, web = tmp_path / "plain", tmp_path / "web"
    env_plain = craze_home(plain, em, snap, base)
    env_web = craze_web_home(web, em, snap, base)
    # craze-web is craze's home, byte for byte, plus config.toml and one variable.
    files = {str(p.relative_to(plain)): p.read_bytes() for p in generated_files(plain)}
    web_files = {str(p.relative_to(web)): p.read_bytes() for p in generated_files(web)}
    assert ".craze/config.toml" not in files
    assert web_files == {**files, ".craze/config.toml": WEB_CONFIG.encode()}
    assert tomllib.loads(WEB_CONFIG) == {"websearch": {"provider": "exa"}}
    assert (web / ".craze" / "config.toml").stat().st_mode & 0o777 == 0o600
    assert env_web == {**env_plain, EXA_MCP_ENV: f"http://127.0.0.1:4242/r/{'a' * 32}/web/mcp"}
    assert EXA_MCP_ENV not in env_plain
    # An Anthropic model's route ends in /v1 and is the same run's: the same web route.
    assert web_mcp_url("http://127.0.0.1:9/r/tok/api.anthropic.com/v1") == "http://127.0.0.1:9/r/tok/web/mcp"
    for bad in ("http://localhost:9/r/t/x", "https://127.0.0.1:9/r/t/x", "http://10.0.0.1:9/r/t/x",
                "http://127.0.0.1:9/x/t/y", "http://127.0.0.1:9/r/t/x?q=1", "http://u@127.0.0.1:9/r/t/x"):
        with pytest.raises(ValueError):
            web_mcp_url(bad)
    with pytest.raises(ValueError):
        craze_web_home(tmp_path / "bad", em, snap, "http://p/r/t/api.z.ai/api/coding/paas/v4")


def test_the_home_names_the_proxys_own_web_route(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r", "craze-web", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl",
                                       corpus=load_tasks()["T-W1"].web_corpus)
            assert web_mcp_url(proxy.base_url(route, "zai-coding-plan")) == proxy.web_url(route)
            assert web_mcp_url(proxy.base_url(route, "anthropic")) == proxy.web_url(route)
            await proxy.end_run(route)

    asyncio.run(go())


def test_probe_homes_leave_out_craze_web(tmp_path):
    from crazeeval.probe import probe_env

    snap = fake_snapshot(tmp_path)
    env = probe_env(tmp_path / "home", load_models()["glm-5.3-flash"], snap)
    assert EXA_MCP_ENV not in env and not (tmp_path / "home" / ".craze" / "config.toml").exists()


def test_web_journal_is_packed():
    rel = "runs/craze-web/glm-5.3-flash/T-W1/rep1/attempt-1/web.jsonl"
    assert wanted_file(rel) and not wanted_file("runs/craze-web/glm-5.3-flash/T-W1/rep1/attempt-1/home/web.jsonl")


# -- the task and its lint ----------------------------------------------------------------


def test_t_w1_is_a_web_task():
    t = load_tasks()["T-W1"]
    assert t.harnesses == ["craze", "craze-web"] and t.repo_kind == "fixture" and t.fixture == "quayside"
    assert t.corpus == t.dir / "testdata" / "corpus" and t.web_corpus is not None
    assert t.relevant == ["v3-authentication.md", "v3-berth-slots.md"]
    assert len(t.web_corpus.pages) - len(t.relevant) >= 3 * len(t.relevant)
    assert web_lint(t) == []
    # Scored by its hidden tests alone.
    assert [c["type"] for c in t.checks] == ["tests"]
    assert all(t.harnesses is None for t in load_tasks().values() if t.split != "web")


def test_the_lint_catches_a_leaked_or_missing_need(tmp_path, monkeypatch):
    t = load_tasks()["T-W1"]
    need = "X-Quayside-Key"
    assert any(f"need {need!r} is in the prompt" in p for p in web_lint(replace(t, prompt=t.prompt + " x-quayside-key")))
    assert any("'nowhere-at-all' is in no relevant corpus page" in p
               for p in web_lint(replace(t, needs=t.needs + ["nowhere-at-all"])))
    # A need in a distractor page only is in no relevant page.
    assert any("'next_page_mark' is in no relevant corpus page" in p
               for p in web_lint(replace(t, relevant=["v3-authentication.md"])))
    patch = tmp_path / "task" / "ref.patch"
    patch.parent.mkdir()
    patch.write_text(t.testdata("testdata/reference.patch").read_text().replace("page_mark", "cursor"))
    leaky = replace(t, dir=patch.parent, validate={"reference_patch": "ref.patch"})
    assert any("'page_mark' is not in the reference patch" in p for p in web_lint(leaky))
    few = [p for p in t.web_corpus.pages if p.file in t.relevant] + [p for p in t.web_corpus.pages
                                                                     if p.file not in t.relevant][:5]
    assert web_lint(replace(t, web_corpus=replace(t.web_corpus, pages=tuple(few)))) == [
        "5 distractor pages for 2 relevant: needs 3 per relevant page"]
    # A need in the fixture, in any case, leaks.
    fixtures = tmp_path / "fixtures"
    shutil.copytree(paths.FIXTURES_DIR / "quayside", fixtures / "quayside")
    with open(fixtures / "quayside" / "files" / "README.md", "a") as f:
        f.write("\nPaging uses PAGE_MARK.\n")
    monkeypatch.setattr(paths, "FIXTURES_DIR", fixtures)
    assert web_lint(t) == ["need 'page_mark' is in the fixture's files/README.md"]


def test_the_lint_catches_a_need_in_a_fixture_file_name(tmp_path, monkeypatch):
    """An empty file whose name holds a need leaks it too: the name is copied into the
    workspace and its git tree. Names are compared ignoring case, as contents are."""
    t = load_tasks()["T-W1"]
    fixtures = tmp_path / "fixtures"
    shutil.copytree(paths.FIXTURES_DIR / "quayside", fixtures / "quayside")
    (fixtures / "quayside" / "files" / "docs").mkdir()
    (fixtures / "quayside" / "files" / "docs" / "x-quayside-key.md").write_text("")
    monkeypatch.setattr(paths, "FIXTURES_DIR", fixtures)
    assert web_lint(t) == ["need 'X-Quayside-Key' is in the fixture's file name files/docs/x-quayside-key.md"]


def test_craze_web_rows_keep_their_executable():
    """An archived craze-web run names its own harness and the craze binary it ran:
    that executable's version and sha256, and the craze build, as a craze run's row."""
    from crazeeval.archive import run_row

    build = {"binary": "craze-B0", "sha256": "abc123", "version": "craze 0.9", "source_commit": "f00",
             "source_dirty": False}
    info = {"campaign": "c", "name": "b", "executables": {"craze": {"version": "craze 0.9", "sha256": "abc123"},
                                                          "gx": {"version": "gx 1", "sha256": "def456"}},
            "craze_build": build}
    plain, web = (run_row({"harness": h, "metrics": {}}, info) for h in ("craze", "craze-web"))
    assert web["harness"] == "craze-web" and plain["harness"] == "craze"
    for row in (plain, web):
        assert (row["harness_version"], row["harness_sha256"], row["craze_build"]) == ("craze 0.9", "abc123", build)
    gx = run_row({"harness": "gx", "metrics": {}}, info)
    assert gx["harness_version"] == "gx 1" and "craze_build" not in gx


def _task(tmp_path: Path, extra: str, split: str = "web") -> Path:
    d = tmp_path / "tasks" / "T-X"
    shutil.copytree(load_tasks()["T-W1"].dir / "testdata", d / "testdata")
    (d / "task.toml").write_text(f'id = "T-X"\ncategory = "feature"\nsplit = "{split}"\nprompt = "p"\n{extra}\n'
                                 '[repo]\nkind = "fixture"\nname = "quayside"\n')
    return d


def test_web_fields_are_checked_at_load(tmp_path):
    ok = ('harnesses = ["craze", "craze-web"]\ncorpus = "testdata/corpus"\nrelevant = ["v3-berth-slots.md"]\n'
          'needs = ["page_mark"]\n')
    assert load_task(_task(tmp_path / "ok", ok)).web_corpus is not None
    cases = {
        "web fields elsewhere": (ok, "dev"),
        "no corpus": (ok.replace('corpus = "testdata/corpus"\n', ""), "web"),
        "no needs": (ok.replace('needs = ["page_mark"]\n', ""), "web"),
        "no craze-web": (ok.replace('"craze", "craze-web"', '"craze"'), "web"),
        "unknown relevant": (ok.replace("v3-berth-slots.md", "nope.md"), "web"),
        "corpus escapes": (ok.replace("testdata/corpus", "../../x"), "web"),
    }
    for name, (extra, split) in cases.items():
        with pytest.raises(TaskError):
            load_task(_task(tmp_path / name.replace(" ", "-"), extra, split))
    bad = _task(tmp_path / "bad-corpus", ok)
    (bad / "testdata" / "corpus" / "stray.txt").write_text("x")
    with pytest.raises(TaskError, match="corpus"):
        load_task(bad)
