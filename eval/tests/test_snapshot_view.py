"""snapshot-config's craze view is craze's merged table (its shipped catalog with the
owner's models.toml laid over it, or the owner's file alone with ``catalog = false``),
and a gx entry is optional per model. Fake owner trees under tmp_path only."""

from __future__ import annotations

import json
import tomllib
from pathlib import Path

import pytest

from crazeeval import paths
from crazeeval.config import EvalModel, Snapshot, craze_models, gx_problems, load_models, owner_overrides
from crazeeval.homes import HOMES, craze_home, craze_web_home
from crazeeval.pricing import load_prices
from crazeeval.snapshot import CRAZE_MODEL_FIELDS, build_snapshot, take

from test_homes_env import catalog_off, owner_tree


def shipped_models() -> dict:
    return tomllib.loads(paths.CRAZE_CATALOG.read_text())["models"]


def write_models(craze: Path, body: str) -> None:
    (craze / "models.toml").write_text("version = 1\n\n" + body)


def as_snapshot(tmp: Path, config: dict) -> Snapshot:
    d = tmp / "snap"
    d.mkdir()
    (d / "config.json").write_text(json.dumps(config))
    (d / "opencode-models.json").write_text("{}")
    return Snapshot(dir=d, config=config, hash="t")


def test_no_owner_models_toml_every_model_is_the_catalogs(tmp_path):
    """craze no longer needs a models.toml: with none, every eval model snapshots, its
    craze entry the shipped catalog's allowlisted fields."""
    craze, grok = owner_tree(tmp_path, craze_models=False)
    assert not (craze / "models.toml").exists()
    snap = build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)
    cat = shipped_models()
    assert set(snap["models"]) == set(load_models())
    for key, em in load_models().items():
        want = take(cat[em.craze], CRAZE_MODEL_FIELDS)
        if em.provider == "anthropic":
            # craze gives each Claude model [anthropic_defaults] default_effort.
            assert "default_effort" not in want and "medium" in want["efforts"], key
            want["default_effort"] = "medium"
        assert snap["models"][key]["craze"] == want, key
    # Not vacuous: the catalog's fields arrive, beyond provider and wire model.
    assert snap["models"]["muse-spark-1.3-contributor"]["craze"]["max_output_tokens"] == 131072
    assert snap["models"]["fireworks/kimi-k3p1-pro"]["craze"]["name"] == "Kimi K3.1 Pro (Fireworks)"


def test_an_owner_override_wins_key_by_key(tmp_path):
    """An owner entry (no source) for a shipped alias overrides only the keys it writes.
    One that writes efforts keeps the shipped default_effort only if the new list has it,
    unless it writes its own; an empty list is no effort control; a default_effort the
    model does not offer is ignored, as craze ignores it."""
    craze, grok = owner_tree(tmp_path, craze_models=False)
    write_models(craze, '[models."fireworks/kimi-k3p1-pro"]\nname = "Mine"\ncontext_window = 500000\n\n'
                        '[models."glm-5.3-flash"]\nefforts = ["max"]\n\n'
                        '[models."muse-spark-1.3-contributor"]\nefforts = ["low", "high"]\n\n'
                        '[models."muse-wonder-1.0"]\nefforts = ["low"]\ndefault_effort = "low"\n\n'
                        '[models."glm-5.3"]\nefforts = []\n\n'
                        '[models."muse-spark-1.3"]\ndefault_effort = "ultra"\n\n'
                        '[models."fireworks/kimi-k3"]\nefforts = ["low"]\ndefault_effort = "high"\n\n'
                        '[models."fireworks/deepseek-v4p1-flash"]\ndefault_effort = ""\n')
    merged = craze_models(craze)
    cat = shipped_models()

    def entry(alias):
        return take(merged[alias], CRAZE_MODEL_FIELDS)

    def shipped(alias, **changes):
        out = {**take(cat[alias], CRAZE_MODEL_FIELDS), **changes}
        return {k: v for k, v in out.items() if v is not None}

    assert entry("fireworks/kimi-k3p1-pro") == shipped("fireworks/kimi-k3p1-pro", name="Mine", context_window=500000)
    assert cat["glm-5.3-flash"]["default_effort"] == "high"
    assert entry("glm-5.3-flash") == shipped("glm-5.3-flash", efforts=["max"], default_effort=None)
    assert entry("muse-spark-1.3-contributor") == shipped("muse-spark-1.3-contributor", efforts=["low", "high"])
    assert entry("muse-spark-1.3-contributor")["default_effort"] == "high"
    assert entry("muse-wonder-1.0") == shipped("muse-wonder-1.0", efforts=["low"], default_effort="low")
    assert entry("glm-5.3") == shipped("glm-5.3", efforts=None, default_effort=None)
    # A default the model does not offer: the key is ignored and the shipped one stands,
    # or none when the new efforts do not have the shipped one either.
    assert entry("muse-spark-1.3") == shipped("muse-spark-1.3") and entry("muse-spark-1.3")["default_effort"] == "high"
    assert entry("fireworks/kimi-k3") == shipped("fireworks/kimi-k3", efforts=["low"], default_effort=None)
    # An empty default turns the shipped one off.
    assert entry("fireworks/deepseek-v4p1-flash") == shipped("fireworks/deepseek-v4p1-flash", default_effort=None)
    # An untouched alias is the catalog's.
    assert entry("fireworks/kimi-k3p1-pro")["efforts"] == cat["fireworks/kimi-k3p1-pro"]["efforts"]
    assert entry("fireworks/deepseek-v4p1-flash-1017") == shipped("fireworks/deepseek-v4p1-flash-1017")
    # The snapshot takes the same table.
    one = {k: v for k, v in load_models().items() if k == "fireworks/kimi-k3p1-pro"}
    snap = build_snapshot(one, craze_dir=craze, grok_dir=grok)
    assert snap["models"]["fireworks/kimi-k3p1-pro"]["craze"] == entry("fireworks/kimi-k3p1-pro")
    assert owner_overrides(craze)["fireworks/kimi-k3p1-pro"] == ["context_window", "name"]


def test_the_shipped_claude_models_get_the_anthropic_default_effort(tmp_path):
    """craze reads [anthropic_defaults] default_effort into each Claude model that offers
    it and has none of its own; so does the merged view, before any owner entry."""
    craze, _ = owner_tree(tmp_path, craze_models=False)
    cat = shipped_models()
    assert "default_effort" not in cat["anthropic/claude-haiku-5-5"]
    assert craze_models(craze)["anthropic/claude-haiku-5-5"]["default_effort"] == "medium"
    write_models(craze, '[models."anthropic/claude-haiku-5-5"]\ndefault_effort = "high"\n\n'
                        '[models."anthropic/claude-sonnet-5-5"]\nefforts = ["low", "high"]\n')
    merged = craze_models(craze)
    assert merged["anthropic/claude-haiku-5-5"]["default_effort"] == "high"
    assert "default_effort" not in merged["anthropic/claude-sonnet-5-5"]  # medium is no longer offered
    assert merged["anthropic/claude-opus-5-5"]["default_effort"] == "medium"
    assert craze_models(craze)["glm-5.3"]["default_effort"] == "max"  # another provider's is its own


def test_an_effort_crazes_entry_does_not_offer_is_refused(tmp_path):
    """A generated craze home pins default_effort to the eval's effort, and craze refuses
    a default the model does not offer: the snapshot stops there instead."""
    craze, grok = owner_tree(tmp_path, craze_models=False)
    one = {k: v for k, v in load_models().items() if k == "glm-5.3-flash"}
    build_snapshot(one, craze_dir=craze, grok_dir=grok)
    write_models(craze, '[models."glm-5.3-flash"]\nefforts = ["max"]\n')
    with pytest.raises(ValueError, match="glm-5.3-flash: the eval pins effort 'high'"):
        build_snapshot(one, craze_dir=craze, grok_dir=grok)
    write_models(craze, '[models."glm-5.3-flash"]\nefforts = []\n')
    with pytest.raises(ValueError, match="no efforts"):
        build_snapshot(one, craze_dir=craze, grok_dir=grok)


def test_an_old_gx_imports_copy_of_a_shipped_alias_is_ignored(tmp_path):
    """An owner entry marked source = "gx" for an alias the catalog ships is an old
    import's copy: the catalog's entry stands. The control is the same entry unmarked,
    which overrides."""
    body = ('[models."muse-wonder-1.0"]\nprovider = "meta"\nwire_model = "muse-wonder-1.0"\nname = "x"\n'
            'context_window = 1000\ndefault_effort = "low"\n')
    cat = take(shipped_models()["muse-wonder-1.0"], CRAZE_MODEL_FIELDS)
    for name, text in (("gx", body + 'source = "gx"\n'), ("own", body)):
        craze, grok = owner_tree(tmp_path / name, craze_models=False)
        write_models(craze, text)
        got = build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)["models"]["muse-wonder-1.0"]["craze"]
        if name == "gx":
            assert got == cat
        else:
            assert got == {**cat, "name": "x", "context_window": 1000, "default_effort": "low"}


def test_with_the_catalog_off_the_owners_file_is_the_whole_table(tmp_path):
    """catalog = false: each craze entry is the owner's alone, and an alias missing from
    the owner's file is an error even though the catalog ships it."""
    craze, grok = owner_tree(tmp_path)
    catalog_off(craze)
    own = tomllib.loads((craze / "models.toml").read_text())["models"]
    snap = build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)
    for key, em in load_models().items():
        assert snap["models"][key]["craze"] == take(own[em.craze], CRAZE_MODEL_FIELDS), key
    assert "vision" not in snap["models"]["muse-wonder-1.0"]["craze"]  # the catalog's, not taken
    text = (craze / "models.toml").read_text()
    start = text.index('[models."muse-wonder-1.0"]')
    (craze / "models.toml").write_text(text[:start] + text[text.index("[models.", start + 1):])
    assert "muse-wonder-1.0" in shipped_models()
    with pytest.raises(KeyError, match="'muse-wonder-1.0' is not in the owner's models.toml"):
        build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)


def test_an_alias_the_catalog_does_not_ship_is_taken_whole(tmp_path):
    """An owner entry for an alias craze does not ship is the whole entry, default_context
    included; an alias in neither the catalog nor the owner's file is an error naming
    both."""
    craze, grok = owner_tree(tmp_path, craze_models=False, gx_models=False)
    entry = {"provider": "acme", "wire_model": "acme-1", "name": "Acme", "context_window": 400000,
             "default_context": 200000, "max_output_tokens": 8000, "efforts": ["high"], "default_effort": "high"}
    write_models(craze, '[models."acme-m"]\n' + "".join(f"{k} = {json.dumps(v)}\n" for k, v in entry.items()))
    with (craze / "providers.toml").open("a") as f:
        f.write('\n[providers.acme]\ndriver = "openai-compat"\nbase_url = "https://api.acme.example/v1"\n')
    em = EvalModel(key="acme-m", provider="acme", wire_model="acme-1", effort="high", craze="acme-m", gx="acme-m",
                   opencode="acme/acme-1")
    assert "acme-m" not in shipped_models()
    assert build_snapshot({"acme-m": em}, craze_dir=craze, grok_dir=grok)["models"]["acme-m"]["craze"] == entry
    nope = EvalModel(key="nope", provider="acme", wire_model="acme-1", effort="high", craze="nope", gx="nope",
                     opencode="acme/acme-1")
    with pytest.raises(KeyError, match="in neither craze's shipped catalog nor the owner's models.toml"):
        build_snapshot({"nope": nope}, craze_dir=craze, grok_dir=grok)


def test_craze_models_takes_the_catalog_path(tmp_path):
    """The helper reads the catalog it is given: an alias only that catalog ships."""
    craze = tmp_path / "native"
    craze.mkdir()
    cat = tmp_path / "catalog.toml"
    cat.write_text('[models.m]\nprovider = "p"\nwire_model = "w"\nefforts = ["low", "high"]\ndefault_effort = "high"\n')
    write_models(craze, '[models.m]\nefforts = ["low"]\n')
    assert craze_models(craze, cat) == {"m": {"provider": "p", "wire_model": "w", "efforts": ["low"]}}


# -- no gx entries --------------------------------------------------------------------

GX_LESS = ("muse-spark-1.3-contributor", "anthropic/claude-haiku-5-5")


def gx_less_snapshot(tmp: Path) -> Snapshot:
    """The owner's state the design is for: no models.toml, gx tables naming no model,
    and no [providers.anthropic] (an env-only key)."""
    craze, grok = owner_tree(tmp, craze_models=False, gx_models=False)
    text = (craze / "providers.toml").read_text()
    (craze / "providers.toml").write_text(text[: text.index("[providers.anthropic]")])
    return as_snapshot(tmp, build_snapshot(load_models(), craze_dir=craze, grok_dir=grok))


def test_empty_gx_tables_give_no_gx_entry_and_craze_still_runs(tmp_path):
    snap = gx_less_snapshot(tmp_path)
    for key in load_models():
        m = snap.model(key)
        assert (m["gx"], m["gx_provider_id"], m["gx_provider"]) == (None, None, None), key
    assert snap.config["version"] == 1
    ms = load_models()
    for key in GX_LESS:
        em = ms[key]
        base = f"http://127.0.0.1:4242/r/{'a' * 32}/{em.provider}.example/v1"
        env = craze_home(tmp_path / "craze" / em.slug, em, snap, base)
        assert env["CRAZE_HOME"]
        native = tmp_path / "craze" / em.slug / ".craze" / "native"
        assert list(tomllib.loads((native / "models.toml").read_text())["models"]) == [em.craze]
        env = craze_web_home(tmp_path / "web" / em.slug, em, snap, base)
        assert (tmp_path / "web" / em.slug / ".craze" / "config.toml").exists()
        HOMES["opencode"](tmp_path / "oc" / em.slug, em, snap, base)
        with pytest.raises(ValueError, match=f"{key}: the config snapshot has no gx entry"):
            HOMES["gx"](tmp_path / "gx" / em.slug, em, snap, base)
        assert not (tmp_path / "gx" / em.slug).exists()
    HOMES["codex"](tmp_path / "cx", ms["muse-spark-1.3-contributor"], snap, "http://127.0.0.1:1/r/t/h/v1")
    # The proxy's reservation still reads the snapshot.
    assert snap.max_output(list(ms.values()))["muse-spark-1.3-contributor"] == 131072


def test_the_probe_builds_no_gx_home_without_a_gx_entry(tmp_path):
    from crazeeval.probe import probe_env

    snap = gx_less_snapshot(tmp_path)
    for key in GX_LESS:
        home = tmp_path / "probe" / load_models()[key].slug
        env = probe_env(home, load_models()[key], snap)
        assert env["CRAZE_HOME"] and (home / ".craze" / "native" / "models.toml").exists()
        assert not (home / ".grok").exists()


def test_a_named_gx_alias_must_still_be_complete(tmp_path):
    """A gx entry is optional, but one the owner's tables do name must still have its
    provider and the eval's wire model."""
    craze, grok = owner_tree(tmp_path)
    text = (grok / "config.toml").read_text()
    (grok / "config.toml").write_text(text.replace('model_provider = "meta"', 'model_provider = "nope"', 1))
    with pytest.raises(KeyError, match="gx provider 'nope' not found"):
        build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)
    (grok / "config.toml").write_text(text.replace('model = "glm-5.3"\n', 'model = "glm-5.2"\n', 1))
    with pytest.raises(ValueError, match="gx wire model 'glm-5.2'"):
        build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)


def test_the_run_precheck_refuses_only_gx(tmp_path):
    snap = gx_less_snapshot(tmp_path)
    ms = [load_models()[k] for k in GX_LESS]
    assert gx_problems(ms, snap, ["craze", "gx"]) == [
        f"{em.key}: the config snapshot has no gx entry (the owner's gx tables do not name {em.gx}); drop gx "
        "from --harness, or add it there and re-run snapshot-config" for em in ms]
    for harnesses in (["craze"], ["craze", "craze-web"], ["opencode"], ["codex"]):
        assert gx_problems(ms, snap, harnesses) == [], harnesses
    full = as_snapshot(tmp_path / "full", build_snapshot(load_models(), *owner_tree(tmp_path / "full")))
    assert gx_problems(ms, full, ["gx"]) == []


def test_run_refuses_gx_on_a_gx_less_model(tmp_path, monkeypatch, capsys):
    """``crazeeval run`` itself: --harness gx on a model with no gx entry stops before
    anything is set up (exit 2); --harness craze on it gets past the check (to the
    toolchains, stopped here)."""
    import crazeeval.keys as keymod
    import crazeeval.sandbox as sandbox
    from crazeeval.cli import main

    snap = gx_less_snapshot(tmp_path)
    craze = tmp_path / "owner" / ".craze" / "native"
    fake_bin = tmp_path / "craze"
    fake_bin.write_text("")
    owner_routes = keymod.load_providers

    class PastTheCheck(Exception):
        pass

    def toolchains(*a, **k):
        raise PastTheCheck

    monkeypatch.setattr(sandbox, "bwrap_available", lambda: True)
    monkeypatch.setattr(sandbox.Toolchains, "resolve", classmethod(lambda cls, *a, **k: toolchains()))
    monkeypatch.setattr(keymod, "load_providers", lambda *a, **k: owner_routes(craze / "providers.toml", env={}))
    common = ["--model", "muse-spark-1.3-contributor", "--config", str(snap.dir), "--out", str(tmp_path / "out"),
              "--tasks", "split:smoke"]
    capsys.readouterr()
    assert main(["run", "--harness", "craze,gx", "--craze-bin", str(fake_bin), *common]) == 2
    err = capsys.readouterr().err
    assert "muse-spark-1.3-contributor: the config snapshot has no gx entry" in err, err
    assert not (tmp_path / "out").exists()
    with pytest.raises(PastTheCheck):
        main(["run", "--harness", "craze", "--craze-bin", str(fake_bin), *common])


def test_the_proxy_command_refuses_a_gx_route_on_a_gx_less_model(tmp_path, monkeypatch, capsys):
    """``crazeeval proxy --harness gx`` would open a spending route for a hand-made gx
    home: refused as a run is, and only for gx."""
    import crazeeval.keys as keymod
    import crazeeval.proxy as proxymod
    from crazeeval.cli import main

    snap = gx_less_snapshot(tmp_path)
    craze = tmp_path / "owner" / ".craze" / "native"
    owner_routes = keymod.load_providers

    class PastTheCheck(Exception):
        pass

    def proxy(*a, **k):
        raise PastTheCheck

    monkeypatch.setattr(keymod, "load_providers", lambda *a, **k: owner_routes(craze / "providers.toml", env={}))
    monkeypatch.setattr(proxymod, "Proxy", proxy)
    common = ["--model", "muse-spark-1.3-contributor", "--config", str(snap.dir), "--out", str(tmp_path / "out"),
              "--ledger", str(tmp_path / "ledger.jsonl"), "--run-id", "r1"]
    capsys.readouterr()
    assert main(["proxy", "--harness", "gx", *common]) == 2
    assert "muse-spark-1.3-contributor: the config snapshot has no gx entry" in capsys.readouterr().err
    for harness in ("manual", "craze", "craze-web", "opencode"):
        with pytest.raises(PastTheCheck):
            main(["proxy", "--harness", harness, *common])


def test_snapshot_config_names_the_owner_overrides_it_took(tmp_path, monkeypatch, capsys):
    """An override craze would reject is still taken (its validation is not mirrored), so
    snapshot-config names each one an eval model's entry came through."""
    from crazeeval.cli import main

    craze, grok = owner_tree(tmp_path, craze_models=False)
    write_models(craze, '[models."glm-5.3"]\ncontext_window = 500000\nname = "Mine"\n\n'
                        '[models."glm-5.3-flash"]\nsource = "gx"\nprovider = "zai-coding-plan"\n'
                        'wire_model = "glm-5.3-flash"\ncontext_window = 1\n\n'
                        '[models."not-an-eval-model"]\nprovider = "meta"\nwire_model = "x"\n')
    monkeypatch.setattr(paths, "OWNER_CRAZE_NATIVE", craze)
    monkeypatch.setattr(paths, "OWNER_GROK", grok)
    catalog: dict = {}
    for em in load_models().values():
        pid, _, mid = em.opencode.partition("/")
        catalog.setdefault(pid, {"models": {}})["models"][mid] = {}
    (tmp_path / "api.json").write_text(json.dumps(catalog))
    capsys.readouterr()
    assert main(["snapshot-config", "--models-dev", str(tmp_path / "api.json"), "--no-variants",
                 "--root", str(tmp_path / "cfg")]) == 0
    notes = [line for line in capsys.readouterr().err.splitlines() if line.startswith("note: ")]
    assert notes == ["note: glm-5.3: the owner's models.toml overrides context_window, name of craze's shipped entry, "
                     "and the snapshot takes them; check that `craze models glm-5.3` agrees"], notes


def test_snapshot_config_prints_a_gx_less_model(tmp_path, monkeypatch, capsys):
    from crazeeval.cli import main

    craze, grok = owner_tree(tmp_path, craze_models=False, gx_models=False)
    monkeypatch.setattr(paths, "OWNER_CRAZE_NATIVE", craze)
    monkeypatch.setattr(paths, "OWNER_GROK", grok)
    catalog: dict = {}
    for em in load_models().values():
        pid, _, mid = em.opencode.partition("/")
        catalog.setdefault(pid, {"models": {}})["models"][mid] = {}
    (tmp_path / "api.json").write_text(json.dumps(catalog))
    assert main(["snapshot-config", "--models-dev", str(tmp_path / "api.json"), "--no-variants",
                 "--root", str(tmp_path / "cfg")]) == 0
    out = capsys.readouterr().out
    assert "  glm-5.3: craze=glm-5.3 gx=- (not in the owner's gx tables) opencode=" in out, out


# -- drift: the real catalog and eval/models.toml --------------------------------------


def test_every_eval_model_is_in_crazes_catalog_and_priced():
    """Every eval model's craze alias is shipped with the eval's provider and wire model,
    so an owner with no models.toml can snapshot it; every wire model has a price."""
    cat, prices = shipped_models(), load_prices()
    for key, em in load_models().items():
        assert em.craze in cat, key
        assert (cat[em.craze]["provider"], cat[em.craze]["wire_model"]) == (em.provider, em.wire_model), key
        assert em.wire_model in prices, key
        # A generated craze home (catalog = false) cannot carry the catalog-only
        # `thinking`, and craze then reads a Claude model as adaptive: an eval model that
        # ships another mode would be run differently from the shipped one.
        assert cat[em.craze].get("thinking", "adaptive") == "adaptive", key
