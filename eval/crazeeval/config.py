"""The eval's model table (eval/models.toml) and the versioned config snapshot."""

from __future__ import annotations

import hashlib
import json
import tomllib
from dataclasses import dataclass
from pathlib import Path

from crazeeval import paths

HARNESSES = ("craze", "gx", "opencode", "codex")


@dataclass(frozen=True)
class EvalModel:
    key: str
    provider: str
    wire_model: str
    effort: str
    craze: str
    gx: str
    opencode: str
    codex: str | None = None
    codex_provider: str | None = None
    last: bool = False

    @property
    def slug(self) -> str:
        return self.key.replace("/", "_")

    def supports(self, harness: str) -> bool:
        if harness == "codex":
            return bool(self.codex and self.codex_provider)
        return harness in HARNESSES


def load_models(path: Path | None = None) -> dict[str, EvalModel]:
    path = path or paths.MODELS_FILE
    with open(path, "rb") as f:
        doc = tomllib.load(f)
    out = {}
    for key, e in (doc.get("models") or {}).items():
        out[key] = EvalModel(
            key=key,
            provider=e["provider"],
            wire_model=e["wire_model"],
            effort=e["effort"],
            craze=e["craze"],
            gx=e["gx"],
            opencode=e["opencode"],
            codex=e.get("codex"),
            codex_provider=e.get("codex_provider"),
            last=bool(e.get("last", False)),
        )
    return out


@dataclass(frozen=True)
class Snapshot:
    """A versioned, secret-free copy of the target definitions every run uses."""

    dir: Path
    config: dict
    hash: str

    @property
    def opencode_models_path(self) -> Path:
        return self.dir / "opencode-models.json"

    def max_output(self, models) -> dict[str, int]:
        """Each eval model's maximum output tokens by wire model: the owner's craze
        ``max_output_tokens`` or gx ``max_completion_tokens`` from the snapshot, else
        pricing.MODEL_MAX_OUTPUT_DEFAULT. What the proxy reserves for a request that
        names no output limit (review r2-c1 item 10)."""
        from crazeeval.pricing import MODEL_MAX_OUTPUT_DEFAULT

        out = {}
        for em in models:
            try:
                m = self.model(em.key)
            except KeyError:
                m = {}
            found = [v for v in ((m.get("craze") or {}).get("max_output_tokens"), (m.get("gx") or {}).get("max_completion_tokens"))
                     if isinstance(v, int) and v > 0]
            out[em.wire_model] = max(found) if found else MODEL_MAX_OUTPUT_DEFAULT
        return out

    def model(self, key: str) -> dict:
        try:
            return self.config["models"][key]
        except KeyError:
            raise KeyError(f"model {key!r} is not in config snapshot {self.dir.name}") from None


def snapshot_hash(d: Path) -> str:
    h = hashlib.sha256()
    for name in ("config.json", "opencode-models.json"):
        p = d / name
        h.update(name.encode() + b"\0")
        h.update(p.read_bytes() if p.exists() else b"")
        h.update(b"\0")
    return h.hexdigest()


def load_snapshot(d: Path | None = None, root: Path | None = None) -> Snapshot:
    root = Path(root or paths.DEFAULT_CONFIG_ROOT)
    if d is None:
        cur = root / "CURRENT"
        if not cur.exists():
            raise FileNotFoundError(f"no config snapshot: run `crazeeval snapshot-config` (looked in {root})")
        d = root / cur.read_text().strip()
    d = Path(d)
    with open(d / "config.json") as f:
        config = json.load(f)
    return Snapshot(dir=d, config=config, hash=snapshot_hash(d))
