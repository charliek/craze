"""Generated homes hold no key; the child environment is built from nothing
(plan 029 §3.1.4, AC-A3)."""

from __future__ import annotations

import json
import re
import tomllib
from pathlib import Path

import pytest

from crazeeval import paths
from crazeeval.config import Snapshot, load_models
from crazeeval.homes import HOMES, craze_home_multi, generated_files
from crazeeval.keys import KeyRing, load_providers
from crazeeval.runners import RUNNERS
from crazeeval.sandbox import SandboxSpec, build_env, env_problems
from crazeeval.snapshot import build_snapshot

FAKE = {
    "meta": "mk-FAKE-meta-6f0c1e9d2b8a4c7e",
    "zai-coding-plan": "zk-FAKE-zai-1a2b3c4d5e6f7a8b",
    "fireworks": "fw_FAKE_fireworks_9e8d7c6b5a4f",
    "openrouter": "sk-or-FAKE-0f1e2d3c4b5a6978",
}


def owner_tree(tmp: Path) -> tuple[Path, Path]:
    """Fake owner files shaped like the real ones, with fake keys in every key field."""
    craze = tmp / "owner" / ".craze" / "native"
    grok = tmp / "owner" / ".grok"
    craze.mkdir(parents=True)
    grok.mkdir(parents=True)
    models = load_models()
    prov_urls = {"meta": "https://api.meta.ai/v1", "zai-coding-plan": "https://api.z.ai/api/coding/paas/v4",
                 "fireworks": "https://api.fireworks.ai/inference/v1"}
    lines = ["version = 1", 'default_model = "glm-5.3-flash"', ""]
    for em in models.values():
        lines += [f'[models."{em.craze}"]', f'provider = "{em.provider}"', f'wire_model = "{em.wire_model}"',
                  'name = "x"', "context_window = 1000000", 'efforts = ["low", "medium", "high", "xhigh", "max"]',
                  'default_effort = "low"', 'source = "gx"', ""]
    (craze / "models.toml").write_text("\n".join(lines))
    pl = ["version = 1", ""]
    for name, url in prov_urls.items():
        pl += [f"[providers.{name}]", 'driver = "openai-compat"', f'base_url = "{url}"', 'env_keys = ["NOPE_KEY_VAR"]',
               f'api_key = "{FAKE[name]}"', ""]
    (craze / "providers.toml").write_text("\n".join(pl))
    gl = []
    for name, url in prov_urls.items():
        gl += [f'[model_providers."{name}"]', f'base_url = "{url}"', f'api_key = "{FAKE[name]}"',
               'env_key = "SOME_KEY"', 'api_backend = "chat_completions"',
               f'[model_providers."{name}".extra_headers]', f'x-secret = "{FAKE[name]}"', ""]
    for em in models.values():
        gl += [f'[model."{em.gx}"]', f'model = "{em.wire_model}"', f'model_provider = "{em.provider}"',
               f'api_key = "{FAKE[em.provider]}"', 'env_key = "ANOTHER_KEY"', 'reasoning_effort = "low"',
               'reasoning_efforts = ["low", "high", "max"]', "context_window = 1000", ""]
    (grok / "config.toml").write_text("\n".join(gl))
    return craze, grok


def fake_snapshot(tmp: Path) -> Snapshot:
    craze, grok = owner_tree(tmp)
    snap = build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)
    d = tmp / "snap"
    d.mkdir()
    (d / "config.json").write_text(json.dumps(snap))
    (d / "opencode-models.json").write_text("{}")
    return Snapshot(dir=d, config=snap, hash="test")


def test_snapshot_keeps_no_secret(tmp_path):
    snap = fake_snapshot(tmp_path)
    blob = json.dumps(snap.config)
    for k in FAKE.values():
        assert k not in blob
    assert "env_key" not in blob and "api_key" not in blob and "extra_headers" not in blob and "NOPE_KEY_VAR" not in blob


def test_no_generated_home_holds_a_key(tmp_path):
    snap = fake_snapshot(tmp_path)
    ring = KeyRing(FAKE)
    homes_root = tmp_path / "homes"
    for em in load_models().values():
        for h, fn in HOMES.items():
            if not em.supports(h):
                continue
            home = homes_root / h / em.slug
            base = f"http://127.0.0.1:9999/r/{'a' * 32}/host/prefix"
            env = fn(home, em, snap, base)
            files = generated_files(home)
            assert files
            for p in files:
                assert not ring.contains_key(p.read_bytes()), p
            # The dummy is the only credential: in a file, or (codex) in the env.
            holders = [p for p in files if paths.DUMMY_KEY.encode() in p.read_bytes()]
            assert holders or env.get(paths.DUMMY_KEY_VAR) == paths.DUMMY_KEY
            assert not env_problems(env, ring)
    craze_home_multi(tmp_path / "multi" / ".craze", list(load_models().values())[:2], snap,
                     {"meta": "http://x/meta", "zai-coding-plan": "http://x/zai", "fireworks": "http://x/fw"})
    for p in generated_files(tmp_path / "multi"):
        assert not ring.contains_key(p.read_bytes())
    multi = tomllib.loads((tmp_path / "multi" / ".craze" / "native" / "models.toml").read_text())
    assert multi["catalog"] is False  # craze plan 031 P10: no shipped model joins the live smoke's home


def test_generated_configs_name_only_the_target_model(tmp_path):
    snap = fake_snapshot(tmp_path)
    em = load_models()["glm-5.3-flash"]
    home = tmp_path / "h"
    HOMES["craze"](home / "craze", em, snap, "http://p/r/t/api.z.ai/api/coding/paas/v4")
    doc = tomllib.loads((home / "craze/.craze/native/models.toml").read_text())
    assert list(doc["models"]) == ["glm-5.3-flash"] and doc["subagents"]["model"] == "glm-5.3-flash"
    # craze's shipped catalog stays out of the eval (craze plan 031 P10): the home's two
    # files are the whole table.
    assert doc["catalog"] is False
    assert doc["models"]["glm-5.3-flash"]["default_effort"] == "high"  # the pinned effort, not the owner's
    prov = home / "craze/.craze/native/providers.toml"
    assert oct(prov.stat().st_mode & 0o777) == "0o600"
    HOMES["gx"](home / "gx", em, snap, "http://p/r/t/api.z.ai/api/coding/paas/v4")
    g = tomllib.loads((home / "gx/.grok/config.toml").read_text())
    assert list(g["model"]) == ["glm-5.3-flash"] and g["models"]["session_summary"] == "glm-5.3-flash"
    assert g["disable_web_search"] is True and g["features"]["web_fetch"] is False
    assert set(g["model"]["glm-5.3-flash"]) <= {"model", "model_provider", "reasoning_effort", "reasoning_efforts", "context_window"}
    HOMES["opencode"](home / "oc", em, snap, "http://p/r/t/api.z.ai/api/coding/paas/v4")
    o = json.loads((home / "oc/opencode.json").read_text())
    assert o["model"] == o["small_model"] == "zai-coding-plan/glm-5.3-flash"
    assert o["permission"] == {"webfetch": "deny", "websearch": "deny"} and list(o["provider"]) == ["zai-coding-plan"]
    with pytest.raises(ValueError):
        HOMES["codex"](home / "cx", em, snap, "http://p")  # Z.AI has no Responses API


def test_child_environment_is_built_from_nothing(tmp_path, monkeypatch):
    monkeypatch.setenv("OPENROUTER_API_KEY", FAKE["openrouter"])
    monkeypatch.setenv("SESSION_TOKEN", "t0k3n")
    snap = fake_snapshot(tmp_path)
    ring = KeyRing(FAKE)
    for em in load_models().values():
        for name, runner in RUNNERS.items():
            if not em.supports(name):
                continue
            home = tmp_path / "e" / name / em.slug
            env = runner.home(home, em, snap, "http://127.0.0.1:1/r/t/h/p")
            spec = SandboxSpec(workspace=tmp_path, ws_inside="/sandbox/work/x", home=home, tools=set(runner.tools), env=env)
            child = build_env(spec)
            for k, v in child.items():
                assert not ring.contains_key(v), k
                assert not re.search("KEY|TOKEN|SECRET", k, re.I) or k == paths.DUMMY_KEY_VAR, k
            assert "OPENROUTER_API_KEY" not in child and "SESSION_TOKEN" not in child
            assert child["PATH"].split(":")[0] == "/sandbox/tools/venv/bin"
            assert child["GIT_AUTHOR_NAME"] and child["GOTOOLCHAIN"] == "local" and child["GOPROXY"] == "off"
            if name == "codex":
                assert "/sandbox/tools/node/bin" in child["PATH"]
    # A planted key or credential-looking variable is caught before launch.
    assert env_problems({"X": FAKE["meta"]}, ring) == ["X"]
    assert env_problems({"GITHUB_TOKEN": "abc"}, ring) == ["GITHUB_TOKEN"]
    assert env_problems({paths.DUMMY_KEY_VAR: paths.DUMMY_KEY}, ring) == []


def test_helper_environment_is_an_allowlist_checked_by_value():
    """Review r1-c1 finding 6: a host helper (uv) gets named variables only, never a
    copy of the environment, and a key hiding under any name -- allowed or not --
    never reaches it."""
    from crazeeval.sandbox import helper_env

    ring = KeyRing(FAKE)
    source = {"HOME": "/home/u", "LANG": "C.UTF-8", "META_AUTH": FAKE["meta"], "SSH_AUTH_SOCK": "/run/x",
              "UV_CACHE_DIR": "/c"}
    env = helper_env(ring, extra_path=["/opt/uv"], source=source)
    assert env == {"HOME": "/home/u", "LANG": "C.UTF-8", "UV_CACHE_DIR": "/c", "PATH": "/opt/uv:/usr/bin:/bin"}
    with pytest.raises(RuntimeError):
        helper_env(ring, source={**source, "LANG": FAKE["zai-coding-plan"]})  # a key under an allowed name


def test_keyring_never_prints_a_key():
    ring = KeyRing(FAKE)
    for k in FAKE.values():
        assert k not in repr(ring) and k not in str(ring)
    assert ring.scrub(f"a {FAKE['meta']} b") == "a <scrubbed-key> b"


def test_load_providers_uses_crazes_rule(tmp_path):
    craze, _ = owner_tree(tmp_path)
    table, ring = load_providers(craze / "providers.toml", env={"NOPE_KEY_VAR": ""})
    assert ring.key_for("meta") == FAKE["meta"]  # empty env var -> the file's key
    table, ring = load_providers(craze / "providers.toml", env={"NOPE_KEY_VAR": "env-key-value-123456"})
    assert ring.key_for("meta") == "env-key-value-123456"
    assert FAKE["meta"] in ring.secrets()  # the unchosen candidate is still scrubbed
    assert table["zai-coding-plan"].host == "api.z.ai" and table["zai-coding-plan"].prefix == "api/coding/paas/v4"
