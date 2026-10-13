"""Generated homes hold no key; the child environment is built from nothing
(plan 029 §3.1.4, AC-A3)."""

from __future__ import annotations

import json
import re
import tomllib
from pathlib import Path

import pytest

from crazeeval import paths
from crazeeval.config import EvalModel, Snapshot, catalog_on, load_models, shipped_driver, snapshot_base_url
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
    "anthropic": "sk-ant-FAKE-anthropic-5d4c3b2a1f0e",
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
    # craze's locked Anthropic provider: the owner's entry holds a key and nothing else.
    pl += ["[providers.anthropic]", f'api_key = "{FAKE["anthropic"]}"', ""]
    (craze / "providers.toml").write_text("\n".join(pl))
    gl = []
    for name, url in prov_urls.items():
        gl += [f'[model_providers."{name}"]', f'base_url = "{url}"', f'api_key = "{FAKE[name]}"',
               'env_key = "SOME_KEY"', 'api_backend = "chat_completions"',
               f'[model_providers."{name}".extra_headers]', f'x-secret = "{FAKE[name]}"', ""]
    # gx on Anthropic, as its user guide sets it up: the key in an extra header.
    gl += ['[model_providers."anthropic"]', 'base_url = "https://api.anthropic.com/v1"', 'api_backend = "messages"',
           '[model_providers."anthropic".extra_headers]', f'x-api-key = "{FAKE["anthropic"]}"',
           'anthropic-version = "2023-06-01"', ""]
    for em in models.values():
        gl += [f'[model."{em.gx}"]', f'model = "{em.wire_model}"', f'model_provider = "{em.provider}"',
               f'api_key = "{FAKE[em.provider]}"', 'env_key = "ANOTHER_KEY"', 'reasoning_effort = "low"',
               'reasoning_efforts = ["low", "high", "max"]', "context_window = 1000", ""]
    (grok / "config.toml").write_text("\n".join(gl))
    return craze, grok


def fake_snapshot(tmp: Path, providers_edit=None) -> Snapshot:
    """A snapshot of owner_tree; providers_edit, if given, rewrites its providers.toml
    text first, and must change it."""
    craze, grok = owner_tree(tmp)
    if providers_edit:
        text = (craze / "providers.toml").read_text()
        edited = providers_edit(text)
        assert edited != text, "the edit changed nothing, so the test would check owner_tree as it is"
        (craze / "providers.toml").write_text(edited)
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
            base = f"http://127.0.0.1:9999/r/{'a' * 32}/host/v1"
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


# -- the craze home's driver (craze plan 047 D14) ------------------------------------

META_HEAD = "[providers.meta]\n"
META_DRIVER = META_HEAD + 'driver = "openai-compat"\n'  # owner_tree's hand-written Meta entry begins so


def marked(source: str):
    """An edit marking owner_tree's Meta entry ``source = <source>``."""
    return lambda t: t.replace(META_HEAD, META_HEAD + f'source = "{source}"\n', 1)


def home_driver(tmp: Path, snap: Snapshot, key: str) -> str:
    """The driver the generated craze home names for eval model ``key``'s provider."""
    em = load_models()[key]
    home = tmp / "h" / em.slug
    HOMES["craze"](home, em, snap, "http://127.0.0.1:9999/r/t/api.meta.ai/v1")
    return tomllib.loads((home / ".craze/native/providers.toml").read_text())["providers"][em.provider]["driver"]


def test_crazes_catalog_ships_meta_on_the_responses_api():
    """The catalog the homes read is craze's own, and it puts Meta on the Responses API;
    the rest of this section relies on that."""
    assert shipped_driver("meta") == "openai-responses"
    assert shipped_driver("zai-coding-plan") == shipped_driver("fireworks") == "openai-compat"
    assert tomllib.loads(paths.CRAZE_CATALOG.read_text())["providers"]["meta"]["driver"] == "openai-responses"


def test_a_gx_imports_meta_entry_gives_crazes_shipped_driver(tmp_path):
    """An old gx import's entry (``source = "gx"``) for a provider craze ships yields to
    craze's catalog (its legacySource rule): craze takes its key and ignores its driver
    and base_url. So the snapshot keeps neither, and Meta's craze home names the driver
    craze ships, openai-responses, not the import's openai-compat. The other providers'
    hand-written entries are kept as written."""
    snap = fake_snapshot(tmp_path, marked("gx"))
    for key in ("muse-spark-1.3-contributor", "muse-wonder-1.0"):
        assert snap.model(key)["craze_provider"] == {}, key
        assert home_driver(tmp_path / key, snap, key) == "openai-responses"
    assert snap.model("glm-5.3")["craze_provider"] == {"driver": "openai-compat",
                                                        "base_url": "https://api.z.ai/api/coding/paas/v4"}
    assert home_driver(tmp_path, snap, "glm-5.3") == "openai-compat"


def test_a_hand_written_driver_is_kept(tmp_path):
    """An owner's own driver -- an entry with no source, or ``source = "manual"`` -- is
    craze's, so the snapshot keeps it and the home names it: Meta stays on Chat
    Completions (craze plan 047 D15's way back)."""
    for name, edit in [("none", None), ("manual", marked("manual"))]:
        snap = fake_snapshot(tmp_path / name, edit)
        assert snap.model("muse-spark-1.3-contributor")["craze_provider"]["driver"] == "openai-compat", name
        assert home_driver(tmp_path / name, snap, "muse-spark-1.3-contributor") == "openai-compat", name


def test_an_entry_naming_no_driver_gives_crazes_shipped_driver(tmp_path):
    """An owner entry that names no driver (a base_url and a key) runs on craze's
    shipped driver, so the home names it rather than a guess."""
    snap = fake_snapshot(tmp_path, lambda t: t.replace(META_DRIVER, META_HEAD))
    assert snap.model("muse-wonder-1.0")["craze_provider"] == {"base_url": "https://api.meta.ai/v1"}
    assert home_driver(tmp_path, snap, "muse-wonder-1.0") == "openai-responses"


def test_a_gx_import_of_a_provider_craze_does_not_ship_keeps_its_driver(tmp_path):
    """craze's legacySource rule is for the providers it ships: a gx import's entry for
    another provider is the owner's own, driver and base_url included, so the snapshot
    keeps them. And a provider craze does not ship has no shipped driver to fall back
    on: a home for one whose entry names none is refused."""
    craze, grok = owner_tree(tmp_path)
    with (craze / "models.toml").open("a") as f:
        f.write('\n[models."acme-m"]\nprovider = "acme"\nwire_model = "acme-1"\nsource = "gx"\n')
    with (craze / "providers.toml").open("a") as f:
        f.write('\n[providers.acme]\nsource = "gx"\ndriver = "openai-compat"\nbase_url = "https://api.acme.example/v1"\n')
    with (grok / "config.toml").open("a") as f:
        f.write('\n[model_providers."acme"]\nbase_url = "https://api.acme.example/v1"\n'
                '\n[model."acme-m"]\nmodel = "acme-1"\nmodel_provider = "acme"\n')
    em = EvalModel(key="acme-m", provider="acme", wire_model="acme-1", effort="high", craze="acme-m", gx="acme-m",
                   opencode="acme/acme-1")
    snap = build_snapshot({"acme-m": em}, craze_dir=craze, grok_dir=grok)
    assert snap["models"]["acme-m"]["craze_provider"] == {"driver": "openai-compat",
                                                          "base_url": "https://api.acme.example/v1"}
    with pytest.raises(ValueError):
        shipped_driver("acme")


def test_an_openrouter_target_is_refused_before_its_home(tmp_path):
    """craze's openrouter driver has a fixed endpoint and refuses a base_url, so a craze
    home cannot point it at the proxy: an OpenRouter target is refused, with a clear error,
    before any file of its home is written -- whether the driver is the owner's own or the
    catalog's (an entry naming none, or an old gx import's)."""
    em = EvalModel(key="openrouter/x", provider="openrouter", wire_model="~x/y-latest", effort="high",
                   craze="openrouter/x", gx="x", opencode="openrouter/x")
    assert shipped_driver("openrouter") == "openrouter"
    for name, cprov in [("catalog", {}), ("owner", {"driver": "openrouter"})]:
        snap = Snapshot(dir=tmp_path, config={"models": {em.key: {"craze": {"provider": "openrouter", "wire_model": "~x/y-latest"},
                                                                   "craze_provider": cprov}}}, hash="t")
        home = tmp_path / name
        with pytest.raises(ValueError, match="fixed endpoint"):
            HOMES["craze"](home, em, snap, "http://127.0.0.1:9999/r/t/openrouter.ai/api/v1")
        assert not home.exists(), name
        with pytest.raises(ValueError, match="fixed endpoint"):
            craze_home_multi(tmp_path / f"multi-{name}", [em], snap, {"openrouter": "http://127.0.0.1:9999/r/t/x/v1"})
        assert not (tmp_path / f"multi-{name}").exists(), name


OLD_GATEWAY = "https://old-gateway.example/v1"


def gx_meta(text: str) -> str:
    """owner_tree's providers.toml with its Meta entry an old gx import's, at an old
    Chat Completions gateway and listing a variable of its own."""
    return marked("gx")(text).replace('base_url = "https://api.meta.ai/v1"', f'base_url = "{OLD_GATEWAY}"', 1)


def catalog_off(craze: Path) -> None:
    """Turn craze's catalog off in owner_tree's models.toml (catalog = false)."""
    text = (craze / "models.toml").read_text()
    assert text.startswith("version = 1\n")
    (craze / "models.toml").write_text(text.replace("version = 1\n", "version = 1\ncatalog = false\n", 1))


def test_with_the_catalog_off_a_gx_entry_is_the_owners_own(tmp_path):
    """With ``catalog = false`` craze keeps the owner's entries whole, an old gx import's
    included (its legacySource rule needs the catalog), so the snapshot keeps the entry's
    driver and base_url, the home names its driver and the route goes to its URL. The
    control is the same tree with the catalog on, where all three are craze's shipped."""
    on, off = tmp_path / "on", tmp_path / "off"
    for d in (on, off):
        craze, grok = owner_tree(d)
        (craze / "providers.toml").write_text(gx_meta((craze / "providers.toml").read_text()))
        if d is off:
            catalog_off(craze)
            assert not catalog_on(craze)
        snap = Snapshot(dir=d, config=build_snapshot(load_models(), craze_dir=craze, grok_dir=grok), hash="t")
        table, _ = load_providers(craze / "providers.toml", env={})
        cprov = snap.model("muse-wonder-1.0")["craze_provider"]
        if d is off:
            assert cprov == {"driver": "openai-compat", "base_url": OLD_GATEWAY}
            assert home_driver(d, snap, "muse-wonder-1.0") == "openai-compat"
            assert (table["meta"].driver, table["meta"].base_url) == ("openai-compat", OLD_GATEWAY)
        else:
            assert cprov == {}
            assert home_driver(d, snap, "muse-wonder-1.0") == "openai-responses"
            assert (table["meta"].driver, table["meta"].base_url) == ("openai-responses", "https://api.meta.ai/v1")
        # The run's check (cli): the snapshot's endpoint is the route's.
        assert snapshot_base_url(cprov, "meta") == table["meta"].base_url


def test_a_gx_entrys_route_is_crazes_shipped_endpoint(tmp_path):
    """The proxy's upstream for an old gx import's entry of a provider craze ships is
    the endpoint craze itself uses, the catalog's, not the URL the import wrote; its key
    is chosen as craze chooses it: the catalog's variables first, then the entry's own
    names, then its api_key. A hand-written entry's URL is still its own."""
    craze, _ = owner_tree(tmp_path)
    (craze / "providers.toml").write_text(gx_meta((craze / "providers.toml").read_text()))
    table, ring = load_providers(craze / "providers.toml", env={})
    meta = table["meta"]
    assert (meta.driver, meta.base_url, meta.host, meta.prefix) == ("openai-responses", "https://api.meta.ai/v1",
                                                                    "api.meta.ai", "v1")
    assert ring.key_for("meta") == FAKE["meta"]  # no variable set: the entry's api_key
    _, ring = load_providers(craze / "providers.toml", env={"NOPE_KEY_VAR": "env-nope-value-123456"})
    assert ring.key_for("meta") == "env-nope-value-123456"  # the entry's own name, after the catalog's
    _, ring = load_providers(craze / "providers.toml",
                             env={"NOPE_KEY_VAR": "env-nope-value-123456", "META_API_KEY": "env-meta-value-123456"})
    assert ring.key_for("meta") == "env-meta-value-123456"  # the catalog's variable first
    assert table["zai-coding-plan"].base_url == "https://api.z.ai/api/coding/paas/v4"
    # A hand-written entry's URL is its own, and the snapshot's check follows it.
    text = (craze / "providers.toml").read_text().replace('source = "gx"\n', "", 1)
    (craze / "providers.toml").write_text(text)
    table, _ = load_providers(craze / "providers.toml", env={})
    assert (table["meta"].driver, table["meta"].base_url) == ("openai-compat", OLD_GATEWAY)
    assert snapshot_base_url({"driver": "openai-compat", "base_url": OLD_GATEWAY}, "meta") == OLD_GATEWAY
    assert snapshot_base_url({}, "meta") == "https://api.meta.ai/v1"
    assert snapshot_base_url({"driver": "openai-compat"}, "meta") == ""  # another driver: no shipped endpoint


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
            env = runner.home(home, em, snap, "http://127.0.0.1:1/r/t/h/v1")
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
