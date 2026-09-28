"""``crazeeval snapshot-config``: a versioned, secret-free copy of the target
definitions (plan 029 §3.1.4).

Reads the owner's craze and gx model tables (read-only) and keeps, per eval model,
only allowlisted fields -- never a key, an env-key name, an auth helper or a header.
Saves a models.dev catalog for opencode (``OPENCODE_MODELS_PATH``). Every run uses a
snapshot and records its hash; runs never read the owner's live files.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
import tomllib
import urllib.request
from pathlib import Path

from crazeeval import paths
from crazeeval.config import EvalModel, snapshot_hash

MODELS_DEV_URL = "https://models.dev/api.json"

CRAZE_MODEL_FIELDS = (
    "provider", "wire_model", "name", "context_window", "max_output_tokens", "efforts", "default_effort", "vision",
    "tool_profile",
)
CRAZE_PROVIDER_FIELDS = ("driver", "base_url")
GX_MODEL_FIELDS = (
    "model", "name", "description", "model_provider", "model_family", "context_window", "max_completion_tokens",
    "stream_tool_calls", "supports_reasoning_effort", "reasoning_effort", "reasoning_efforts", "system_prompt_label",
    "supports_vision", "temperature", "top_p",
)
GX_PROVIDER_FIELDS = ("base_url", "api_backend")


def take(src: dict, fields: tuple[str, ...]) -> dict:
    return {k: src[k] for k in fields if k in src}


def _load(p: Path) -> dict:
    with open(p, "rb") as f:
        return tomllib.load(f)


def build_snapshot(
    models: dict[str, EvalModel],
    craze_dir: Path | None = None,
    grok_dir: Path | None = None,
) -> dict:
    craze_dir = Path(craze_dir or paths.OWNER_CRAZE_NATIVE)
    grok_dir = Path(grok_dir or paths.OWNER_GROK)
    cm = _load(craze_dir / "models.toml").get("models") or {}
    cp = _load(craze_dir / "providers.toml").get("providers") or {}
    gx_models: dict = {}
    gx_providers: dict = {}
    # providers.toml deep-merges over config.toml (gx's user layer).
    for name in ("config.toml", "providers.toml"):
        p = grok_dir / name
        if p.exists():
            d = _load(p)
            for k, v in (d.get("model") or {}).items():
                gx_models.setdefault(k, {}).update(v)
            for k, v in (d.get("model_providers") or {}).items():
                gx_providers.setdefault(k, {}).update(v)
    out = {"version": 1, "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "models": {}}
    for key, em in models.items():
        if em.craze not in cm:
            raise KeyError(f"{key}: craze alias {em.craze!r} not in the owner's models.toml")
        centry = take(cm[em.craze], CRAZE_MODEL_FIELDS)
        if centry.get("provider") != em.provider:
            raise ValueError(f"{key}: craze provider {centry.get('provider')!r} != eval provider {em.provider!r}")
        if centry.get("wire_model") != em.wire_model:
            raise ValueError(f"{key}: craze wire model {centry.get('wire_model')!r} != {em.wire_model!r}")
        if em.provider not in cp:
            raise KeyError(f"{key}: provider {em.provider!r} not in the owner's providers.toml")
        cprov = take(cp[em.provider], CRAZE_PROVIDER_FIELDS)
        if em.gx not in gx_models:
            raise KeyError(f"{key}: gx alias {em.gx!r} not in the owner's gx model tables")
        gentry = take(gx_models[em.gx], GX_MODEL_FIELDS)
        gpid = gentry.get("model_provider")
        if not gpid or gpid not in gx_providers:
            raise KeyError(f"{key}: gx provider {gpid!r} not found")
        if gentry.get("model", em.gx) != em.wire_model:
            raise ValueError(f"{key}: gx wire model {gentry.get('model')!r} != {em.wire_model!r}")
        out["models"][key] = {
            "eval": {
                "provider": em.provider,
                "wire_model": em.wire_model,
                "effort": em.effort,
                "craze": em.craze,
                "gx": em.gx,
                "opencode": em.opencode,
                "codex": em.codex,
                "codex_provider": em.codex_provider,
            },
            "craze": centry,
            "craze_provider": cprov,
            "gx": gentry,
            "gx_provider_id": gpid,
            "gx_provider": take(gx_providers[gpid], GX_PROVIDER_FIELDS),
        }
    return out


def fetch_models_dev(url: str = MODELS_DEV_URL) -> bytes:
    req = urllib.request.Request(url, headers={"User-Agent": "crazeeval"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return r.read()


def check_opencode_ids(models: dict[str, EvalModel], catalog: dict) -> dict:
    out = {}
    for key, em in models.items():
        pid, _, mid = em.opencode.partition("/")
        prov = catalog.get(pid) or {}
        out[key] = {"provider": pid, "model": mid, "present": mid in (prov.get("models") or {})}
    return out


def write_snapshot(snap: dict, models_dev: bytes, root: Path | None = None) -> Path:
    root = Path(root or paths.DEFAULT_CONFIG_ROOT)
    root.mkdir(parents=True, exist_ok=True)
    body = json.dumps(snap, indent=2, sort_keys=True).encode() + b"\n"
    h = hashlib.sha256(body + models_dev).hexdigest()[:10]
    d = root / f"{time.strftime('%Y%m%d-%H%M%S')}-{h}"
    d.mkdir()
    (d / "config.json").write_bytes(body)
    (d / "opencode-models.json").write_bytes(models_dev)
    (d / "HASH").write_text(snapshot_hash(d) + "\n")
    tmp = root / "CURRENT.tmp"
    tmp.write_text(d.name + "\n")
    os.replace(tmp, root / "CURRENT")
    return d
