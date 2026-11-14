"""The eval's model table (eval/models.toml) and the versioned config snapshot."""

from __future__ import annotations

import hashlib
import json
import tomllib
from dataclasses import dataclass
from pathlib import Path

from crazeeval import paths

# What a run selects when it names no harness, and every harness it may name: craze-web
# (craze with web search answered from a web task's corpus, craze plan 051 E2) runs only
# when named.
DEFAULT_HARNESSES = ("craze", "gx", "opencode", "codex")
SUPPORTED_HARNESSES = DEFAULT_HARNESSES + ("craze-web",)
HARNESSES = DEFAULT_HARNESSES
# An arm that runs another harness's executable, by the name batch identity and the
# manifest record it under (manifest.executables): craze-web runs the craze binary.
ARM_EXECUTABLE = {"craze-web": "craze"}


def harness_executable(harness: str) -> str:
    """The executable ``harness`` runs, as manifests and batch identity name it."""
    return ARM_EXECUTABLE.get(harness, harness)

# The source craze's former gx importer wrote on every entry it made (craze's
# modeltable legacySource): such an entry for a provider craze ships yields to the
# catalog, lending only its key, so its driver and base_url are not craze's.
LEGACY_SOURCE = "gx"
# craze's Anthropic provider id (keys.ANTHROPIC; keys imports this module).
ANTHROPIC_PROVIDER = "anthropic"


def shipped_providers(path: Path | None = None) -> dict[str, dict]:
    """craze's shipped catalog's ``[providers]`` (``paths.CRAZE_CATALOG``), each entry
    as craze ships it: its ``driver``, ``base_url``, ``env_keys`` and the rest."""
    with open(path or paths.CRAZE_CATALOG, "rb") as f:
        return dict(tomllib.load(f).get("providers") or {})


def catalog_on(craze_dir: Path) -> bool:
    """Whether craze merges its shipped catalog under the files in ``craze_dir``: unless
    its ``models.toml`` says ``catalog = false`` (craze's modeltable LoadWith), when the two
    files are the whole table and every entry, an old gx import's too, is the owner's own."""
    p = Path(craze_dir) / "models.toml"
    if not p.exists():
        return True
    with open(p, "rb") as f:
        return tomllib.load(f).get("catalog") is not False


def craze_models(craze_dir: Path, catalog_path: Path | None = None) -> dict[str, dict]:
    """craze's model table as craze builds it from ``craze_dir`` (the owner's native
    directory), each entry by alias.

    With the catalog off (``catalog_on`` is False: the owner's ``models.toml`` says
    ``catalog = false``), the owner's ``[models]`` alone. With it on, craze's shipped
    catalog's ``[models]`` (``paths.CRAZE_CATALOG``, or ``catalog_path``) with the owner's
    ``models.toml`` -- optional; absent means no entries -- laid over it by craze's rule
    (internal/harness/modeltable/overlay.go: ``merger.model`` and ``overlayModel``):

    - an entry marked ``source = "gx"`` (an old gx import's copy) for an alias the
      catalog ships is ignored: the catalog's entry stands;
    - any other entry for a shipped alias overrides it key by key, only the keys it
      writes. One that writes ``efforts`` keeps the shipped ``default_effort`` only if the
      new list has it, unless it writes its own; an empty ``efforts`` list means none;
    - an entry for an alias the catalog does not ship is taken whole.

    A ``default_effort`` the resulting model does not offer is ignored, as craze ignores
    it. The shipped entries are read as craze reads them (``_shipped_models``).

    Not mirrored: craze also drops an entry that points at a retired wire id (or an old
    gx import's copy of a retired alias), falls back to the shipped entry when an
    override fails any other of its validation rules (a bad ``cost`` table, an unknown
    provider, a ``default_context`` over the window), merges a ``cost`` table key by key
    (here it is replaced whole; no snapshot keeps it) and forgets the shipped
    ``deprecated`` / ``replaced_by`` / ``maker`` when an entry repoints the alias. So an
    override craze rejects is still taken here: ``snapshot-config`` names every override
    it used (``owner_overrides``) for the owner to compare with ``craze models <alias>``."""
    p = Path(craze_dir) / "models.toml"
    own: dict = {}
    if p.exists():
        with open(p, "rb") as f:
            own = dict(tomllib.load(f).get("models") or {})
    if not catalog_on(craze_dir):
        return {alias: dict(e) for alias, e in own.items()}
    shipped = _shipped_models(catalog_path)
    out = {alias: dict(e) for alias, e in shipped.items()}
    for alias, o in own.items():
        base = shipped.get(alias)
        if base is None:
            out[alias] = dict(o)
        elif o.get("source") != LEGACY_SOURCE:
            out[alias] = _overlay_model(base, o)
    return out


def _shipped_models(catalog_path: Path | None = None) -> dict[str, dict]:
    """The shipped catalog's ``[models]`` as craze reads them before any user file:
    ``[anthropic_defaults] default_effort`` is the default effort of each Anthropic model
    that offers it and has none of its own (craze's withAnthropicList)."""
    with open(catalog_path or paths.CRAZE_CATALOG, "rb") as f:
        doc = tomllib.load(f)
    shipped = {alias: dict(e) for alias, e in (doc.get("models") or {}).items()}
    effort = (doc.get("anthropic_defaults") or {}).get("default_effort")
    for e in shipped.values():
        if e.get("provider") == ANTHROPIC_PROVIDER and not e.get("default_effort") and effort in (e.get("efforts") or []):
            e["default_effort"] = effort
    return shipped


def owner_overrides(craze_dir: Path, catalog_path: Path | None = None) -> dict[str, list[str]]:
    """The shipped aliases the owner's ``models.toml`` overrides, each with the keys it
    writes: the entries ``craze_models`` lays over the catalog. Empty with the catalog
    off (the owner's file is then the whole table) or with no file."""
    p = Path(craze_dir) / "models.toml"
    if not p.exists() or not catalog_on(craze_dir):
        return {}
    with open(p, "rb") as f:
        own = tomllib.load(f).get("models") or {}
    shipped = _shipped_models(catalog_path)
    return {alias: sorted(k for k in o if k != "source") for alias, o in own.items()
            if alias in shipped and o.get("source") != LEGACY_SOURCE}


def _overlay_model(base: dict, o: dict) -> dict:
    """``base`` (a shipped entry) with the keys ``o`` writes (craze's overlayModel)."""
    m = dict(base)
    m.update({k: v for k, v in o.items() if k not in ("efforts", "default_effort")})
    if "efforts" in o:
        if o["efforts"]:
            m["efforts"] = list(o["efforts"])
        else:
            m.pop("efforts", None)  # craze: an empty list is no effort control
        if m.get("default_effort") not in (m.get("efforts") or []):
            m.pop("default_effort", None)
    if "default_effort" in o:
        if not o["default_effort"]:
            m.pop("default_effort", None)
        elif o["default_effort"] in (m.get("efforts") or []):
            m["default_effort"] = o["default_effort"]
        # else craze ignores the key: the shipped default stands if the model still
        # offers it (kept or dropped above), else none.
    return m


def yields_to_catalog(provider: str, entry: dict, shipped: dict, catalog: bool) -> bool:
    """Whether the owner's provider entry is an old gx import's that craze reads through
    its catalog (its legacySource rule): marked ``source = "gx"``, for a provider craze
    ships, with the catalog on. craze then takes only its key and any ``env_keys`` names
    the catalog lacks, never its driver or ``base_url``."""
    return catalog and entry.get("source") == LEGACY_SOURCE and provider in shipped


def shipped_driver(provider: str, path: Path | None = None) -> str:
    """The driver craze ships for ``provider``: what craze uses when the owner's entry
    names none, or is an old gx import's. ValueError for a provider craze does not ship,
    which must name its own."""
    driver = (shipped_providers(path).get(provider) or {}).get("driver")
    if not driver:
        raise ValueError(f"craze ships no provider {provider!r}, so its entry must name a driver")
    return driver


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
        return harness in SUPPORTED_HARNESSES


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
        """Each eval model's maximum output tokens by wire model: the **largest** of every
        limit that applies to it -- the snapshot's craze ``max_output_tokens``, gx
        ``max_completion_tokens`` (none without a gx entry) and models.dev ``limit.output``
        (deepseek-v4p1-flash lists 384000) -- else pricing.MODEL_MAX_OUTPUT_DEFAULT. What
        the proxy reserves for a request that names no output limit: whichever harness
        sent it, the provider may generate up to the largest (review r2-c1 item 10,
        r1-c2 §10)."""
        from crazeeval.pricing import MODEL_MAX_OUTPUT_DEFAULT

        catalog = self._catalog()
        out = {}
        for em in models:
            try:
                m = self.model(em.key)
            except KeyError:
                m = {}
            limits = [(m.get("craze") or {}).get("max_output_tokens"), (m.get("gx") or {}).get("max_completion_tokens"),
                      catalog_max_output(catalog, em.opencode)]
            found = [v for v in limits if isinstance(v, int) and v > 0]
            out[em.wire_model] = max(found) if found else MODEL_MAX_OUTPUT_DEFAULT
        return out

    def _catalog(self) -> dict:
        try:
            return json.loads(self.opencode_models_path.read_bytes())
        except (OSError, ValueError):
            return {}

    def model(self, key: str) -> dict:
        try:
            return self.config["models"][key]
        except KeyError:
            raise KeyError(f"model {key!r} is not in config snapshot {self.dir.name}") from None


def gx_problems(models, snap: Snapshot, harnesses) -> list[str]:
    """Why a run cannot start the gx harness: one line per model whose config snapshot has
    no gx entry (the owner's gx tables did not name its alias at snapshot time). Only the
    harness named ``gx`` needs one; none for a run without it."""
    if "gx" not in harnesses:
        return []
    return [f"{em.key}: the config snapshot has no gx entry (the owner's gx tables do not name {em.gx}); "
            "drop gx from --harness, or add it there and re-run snapshot-config"
            for em in models if snap.model(em.key).get("gx") is None]


def catalog_max_output(catalog: dict, opencode_id: str) -> int | None:
    """A model's output limit in a models.dev catalog (``limit.output``), by its
    opencode id ``<provider>/<model>``."""
    pid, _, mid = (opencode_id or "").partition("/")
    entry = ((catalog.get(pid) or {}).get("models") or {}).get(mid) or {}
    v = (entry.get("limit") or {}).get("output")
    return v if isinstance(v, int) and v > 0 else None


def snapshot_hash(d: Path) -> str:
    h = hashlib.sha256()
    for name in ("config.json", "opencode-models.json"):
        p = d / name
        h.update(name.encode() + b"\0")
        h.update(p.read_bytes() if p.exists() else b"")
        h.update(b"\0")
    return h.hexdigest()


def load_snapshot(d: Path | None = None, root: Path | None = None) -> Snapshot:
    root = Path(root or paths.config_root())
    if d is None:
        cur = root / "CURRENT"
        if not cur.exists():
            raise FileNotFoundError(f"no config snapshot: run `crazeeval snapshot-config` (looked in {root})")
        d = root / cur.read_text().strip()
    d = Path(d)
    with open(d / "config.json") as f:
        config = json.load(f)
    return Snapshot(dir=d, config=config, hash=snapshot_hash(d))
