"""Generated harness homes (plan 029 §3.1.4).

Every file here is built from field allowlists out of the config snapshot, names only
the run's target model, points at the proxy route and holds only the dummy key. None
of the owner's files is read or written. A craze home's models.toml says
``catalog = false`` (craze plan 031 P10): craze ships a model catalog and merges it
under the user's files, which would otherwise add every shipped model to the run.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import tomli_w

from crazeeval import paths
from crazeeval.config import EvalModel, Snapshot

H = paths.SANDBOX_HOME


def _write(p: Path, text: str, mode: int = 0o644) -> None:
    p.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
    with os.fdopen(fd, "w") as f:
        f.write(text)
    os.chmod(p, mode)


# -- craze ------------------------------------------------------------------------


def _craze_model(m: dict, em: EvalModel) -> dict:
    return {**m["craze"], "default_effort": em.effort}


def _craze_provider(m: dict, base_url: str) -> dict:
    return {"driver": m["craze_provider"].get("driver", "openai-compat"), "base_url": base_url, "api_key": paths.DUMMY_KEY}


def _write_craze_native(crazedir: Path, models: dict, providers: dict) -> None:
    # The whole table (craze plan 031 P10): no model craze ships joins it, so the agent
    # tool's description names only the run's models and the prompt stays frozen.
    models = {**models, "catalog": False}
    native = crazedir / "native"
    native.mkdir(parents=True, exist_ok=True)
    os.chmod(crazedir, 0o700)
    os.chmod(native, 0o700)
    _write(native / "models.toml", tomli_w.dumps(models))
    _write(native / "providers.toml", tomli_w.dumps(providers), mode=0o600)


def craze_home(home: Path, em: EvalModel, snap: Snapshot, base_url: str) -> dict[str, str]:
    m = snap.model(em.key)
    entry = _craze_model(m, em)
    if entry.get("efforts") and em.effort not in entry["efforts"]:
        raise ValueError(f"{em.key}: pinned effort {em.effort!r} not in craze's efforts {entry['efforts']}")
    models = {
        "version": 1,
        "default_model": em.craze,
        "subagents": {"model": em.craze},
        "models": {em.craze: entry},
    }
    providers = {"version": 1, "providers": {em.provider: _craze_provider(m, base_url)}}
    _write_craze_native(home / ".craze", models, providers)
    return {"CRAZE_HOME": f"{H}/.craze"}


def craze_home_multi(crazedir: Path, ems: list[EvalModel], snap: Snapshot, base_urls: dict[str, str]) -> None:
    """A CRAZE_HOME for the standalone proxy (the live smoke): several models, the
    first the default, each provider pointing at its proxy route."""
    models = {"version": 1, "default_model": ems[0].craze, "models": {}}
    providers: dict = {"version": 1, "providers": {}}
    for em in ems:
        m = snap.model(em.key)
        models["models"][em.craze] = _craze_model(m, em)
        providers["providers"][em.provider] = _craze_provider(m, base_urls[em.provider])
    _write_craze_native(Path(crazedir), models, providers)


# -- gx ---------------------------------------------------------------------------


def gx_home(home: Path, em: EvalModel, snap: Snapshot, base_url: str) -> dict[str, str]:
    m = snap.model(em.key)
    model = dict(m["gx"])
    model["reasoning_effort"] = em.effort
    pid = m["gx_provider_id"]
    provider = {
        "base_url": base_url,
        "api_key": paths.DUMMY_KEY,
        "api_backend": m["gx_provider"].get("api_backend", "chat_completions"),
    }
    doc = {
        # Web tools off (plan 029 §3.1.4): web_search and web_fetch, and the hosted
        # search lane; image/video tools would call api.x.ai.
        "disable_web_search": True,
        "cli": {"auto_update": False},
        "models": {
            "default": em.gx,
            # Title and image-description calls default to grok-4.6; pin them to the target.
            "session_summary": em.gx,
            "image_description": em.gx,
        },
        "features": {
            "web_fetch": False,
            "backend_tools": False,
            "image_gen": False,
            "video_gen": False,
            "telemetry": False,
            "remote_fetch": False,
            # Per-turn summary and title refresh replay the whole conversation to the
            # model and never reach the answer: off, to spend the budget on answers.
            "turn_summary": False,
            "title_refresh": False,
        },
        # Pre-set so gx does not rewrite the generated file on first run.
        "marketplace": {"official_marketplace_auto_installed": True, "default_skills_installs_purged": True},
        "model_providers": {pid: provider},
        "model": {em.gx: model},
    }
    grok = home / ".grok"
    grok.mkdir(parents=True, exist_ok=True)
    _write(grok / "config.toml", tomli_w.dumps(doc), mode=0o600)
    return {"GROK_HOME": f"{H}/.grok", "GROK_IMAGE_EDIT": "0"}


# -- opencode ---------------------------------------------------------------------

OPENCODE_MODELS_INSIDE = f"{paths.SANDBOX_CONFIG}/opencode-models.json"
XDG_DIRS = ("config", "data", "cache", "state")


def opencode_env(models_path: str) -> dict[str, str]:
    """opencode's variables: config and XDG dirs in the sandbox home, a saved models.dev
    catalog at ``models_path``, and no fetch, update or share."""
    return {
        "OPENCODE_CONFIG": f"{H}/opencode.json",
        **{f"XDG_{d.upper()}_HOME": f"{H}/.xdg/{d}" for d in XDG_DIRS},
        "OPENCODE_MODELS_PATH": models_path,
        "OPENCODE_DISABLE_MODELS_FETCH": "1",
        "OPENCODE_DISABLE_AUTOUPDATE": "1",
        "OPENCODE_DISABLE_SHARE": "1",
    }


def opencode_home(home: Path, em: EvalModel, snap: Snapshot, base_url: str) -> dict[str, str]:
    pid, _, _ = em.opencode.partition("/")
    doc = {
        "$schema": "https://opencode.ai/config.json",
        "model": em.opencode,
        "small_model": em.opencode,
        "enabled_providers": [pid],
        "autoupdate": False,
        "share": "disabled",
        "provider": {pid: {"options": {"baseURL": base_url, "apiKey": paths.DUMMY_KEY}}},
        # Web tools off (plan 029 §3.1.4): a deny removes them from the offered tools.
        "permission": {"webfetch": "deny", "websearch": "deny"},
    }
    for d in XDG_DIRS:
        (home / ".xdg" / d).mkdir(parents=True, exist_ok=True)
    _write(home / "opencode.json", json.dumps(doc, indent=2) + "\n", mode=0o600)
    return opencode_env(OPENCODE_MODELS_INSIDE)


OPENCODE_CONFIG_INSIDE = f"{H}/.xdg/config/opencode"
SEED_FILES = ("package.json", "package-lock.json")


def opencode_seed_binds(home: Path, seed: Path | None) -> list[tuple[Path, str]]:
    """Offline packages for an agent run (review r1-c1 finding 12). opencode installs
    ``@opencode-ai/plugin`` into its config directory at start-up and skips it when
    ``node_modules`` exists and ``package-lock.json`` lists every declared package. So
    the run's config directory gets the seed's package files and its ``node_modules``
    bound read-only: nothing is fetched, and nothing needs to be."""
    if seed is None:
        return []
    cfg = home / ".xdg" / "config" / "opencode"
    cfg.mkdir(parents=True, exist_ok=True)
    for name in SEED_FILES:
        _write(cfg / name, (seed / name).read_text())
    return [(seed / "node_modules", f"{OPENCODE_CONFIG_INSIDE}/node_modules")]


# -- codex ------------------------------------------------------------------------


def codex_home(home: Path, em: EvalModel, snap: Snapshot, base_url: str) -> dict[str, str]:
    if not em.supports("codex"):
        raise ValueError(f"{em.key}: codex does not run on this model")
    pid = em.codex_provider
    doc = {
        "model": em.codex,
        "model_provider": pid,
        "model_reasoning_effort": em.effort,
        # Web tools off; the hosted web_search tool is otherwise offered to every
        # custom provider (and upgraded to live under the bypass flag).
        "web_search": "disabled",
        "analytics": {"enabled": False},
        "features": {
            # exec would fetch github.com/openai/plugins.git with no login.
            "plugins": False,
            "unbounded_connection_retries": False,
        },
        "model_providers": {
            pid: {"name": pid, "base_url": base_url, "env_key": paths.DUMMY_KEY_VAR, "wire_api": "responses"}
        },
    }
    ch = home / ".codex"
    ch.mkdir(parents=True, exist_ok=True)
    _write(ch / "config.toml", tomli_w.dumps(doc), mode=0o600)
    return {"CODEX_HOME": f"{H}/.codex", paths.DUMMY_KEY_VAR: paths.DUMMY_KEY}


HOMES = {"craze": craze_home, "gx": gx_home, "opencode": opencode_home, "codex": codex_home}


def generated_files(home: Path) -> list[Path]:
    return sorted(p for p in Path(home).rglob("*") if p.is_file())
