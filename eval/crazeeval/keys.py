"""Provider keys and the route table, read from the owner's craze providers.toml.

The proxy is the only thing that ever holds a key (plan 029 §3.1.2). Keys are
resolved with craze's own rule -- the first non-empty ``env_keys`` variable, else the
file's ``api_key`` -- and live only in a :class:`KeyRing`, whose ``repr`` and ``str``
never show them. Nothing here prints, logs or writes a key.
"""

from __future__ import annotations

import gzip
import json
import os
import tomllib
from dataclasses import dataclass
from pathlib import Path
from typing import Mapping
from urllib.parse import urlsplit

from crazeeval import paths

OPENROUTER_BASE = "https://openrouter.ai/api/v1"

# The upstreams the eval may reach (plan 029 §3.1.2); openrouter is here for
# completeness only -- no eval target is served there.
EVAL_PROVIDERS = ("meta", "zai-coding-plan", "fireworks", "openrouter")


@dataclass(frozen=True)
class Provider:
    """One upstream: its craze provider name, the base URL, and the route parts."""

    name: str
    driver: str
    base_url: str  # e.g. https://api.z.ai/api/coding/paas/v4
    host: str  # e.g. api.z.ai
    prefix: str  # e.g. api/coding/paas/v4 (no leading or trailing slash)

    @staticmethod
    def from_url(name: str, driver: str, base_url: str) -> "Provider":
        u = urlsplit(base_url.rstrip("/"))
        if u.scheme != "https" or not u.hostname:
            raise ValueError(f"provider {name!r}: base_url must be an https URL")
        return Provider(
            name=name,
            driver=driver,
            base_url=f"https://{u.netloc}{u.path}".rstrip("/"),
            host=u.netloc,
            prefix=u.path.strip("/"),
        )


class KeyRing:
    """Holds the keys in memory. Never printable."""

    __slots__ = ("_by_provider", "_all", "_forms", "_bforms")

    def __init__(self, by_provider: Mapping[str, str], extra: tuple[str, ...] = ()):
        self._by_provider = {k: v for k, v in by_provider.items() if v}
        seen = set(self._by_provider.values()) | {v for v in extra if v}
        # Longest first, so a key that contains another is scrubbed whole.
        self._all = tuple(sorted(seen, key=len, reverse=True))
        # Each key as written, then JSON-escaped when that differs.
        forms = []
        for s in self._all:
            forms.append(s)
            esc = json.dumps(s)[1:-1]
            if esc != s:
                forms.append(esc)
        self._forms = tuple(forms)
        self._bforms = tuple(f.encode() for f in forms)

    def __repr__(self) -> str:  # never reveal a key
        return f"<KeyRing providers={sorted(self._by_provider)} secrets={len(self._all)}>"

    __str__ = __repr__

    def providers(self) -> list[str]:
        return sorted(self._by_provider)

    def key_for(self, provider: str) -> str | None:
        return self._by_provider.get(provider)

    def secrets(self) -> tuple[str, ...]:
        """Every key the ring knows (including env/file candidates not chosen)."""
        return self._all

    def scrub(self, text: str) -> str:
        for form in self._forms:
            if form in text:
                text = text.replace(form, "<scrubbed-key>")
        return text

    def contains_key(self, data: bytes | str) -> bool:
        forms = self._bforms if isinstance(data, bytes) else self._forms
        return any(form in data for form in forms)

    def byte_forms(self) -> tuple[bytes, ...]:
        """Every byte form a key takes on the wire (for the response redactor)."""
        return self._bforms

    def json_contains_key(self, obj) -> bool:
        """Whether any string (key or value) in decoded JSON holds a key: catches a key
        the raw body only carried escaped (``\\u0073k-…``)."""
        stack = [obj]
        while stack:
            o = stack.pop()
            if isinstance(o, str):
                if self.contains_key(o):
                    return True
            elif isinstance(o, dict):
                for k, v in o.items():
                    stack.append(k)
                    stack.append(v)
            elif isinstance(o, list):
                stack.extend(o)
        return False


SCAN_MAX_BYTES = 256 * 1024 * 1024  # a file (as stored) larger than this is not scanned: skipped
GZ_CHUNK = 8 * 1024 * 1024  # decompressed bytes held per step of a gzip scan (read at call time)


def data_has_key(name: str, data: bytes, ring: KeyRing) -> bool:
    """Whether a file's bytes hold a loaded key -- the test ``scan_tree`` applies to each
    file. A ``.gz`` file's raw bytes are scanned, then its members decompressed **to the
    end**, ``GZ_CHUNK`` bytes at a time, each chunk scanned with the previous one's last
    (longest key form - 1) bytes, so a key split across two chunks is still found. A
    member that stops decompressing (corrupt or truncated) ends the scan there: what
    decompressed before it was scanned, and so were the raw bytes."""
    import io

    if ring.contains_key(data):
        return True
    if not name.endswith(".gz"):
        return False
    keep = max((len(f) for f in ring.byte_forms()), default=1) - 1
    tail = b""
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(data)) as g:
            while chunk := g.read(GZ_CHUNK):
                buf = tail + chunk
                if ring.contains_key(buf):
                    return True
                tail = buf[-keep:] if keep else b""
    except (OSError, EOFError):
        pass
    return False


def scan_tree(root: Path, ring: KeyRing, max_bytes: int = SCAN_MAX_BYTES) -> dict:
    """Grep every file under ``root`` (gzip members decompressed to the end:
    ``data_has_key``) for every loaded key. A file over ``max_bytes`` as stored is
    skipped, and listed.

    Returns counts and the relative paths of files with a hit -- never a key.
    """
    from crazeeval import safefs

    root = Path(root)
    hits: list[str] = []
    scanned = 0
    skipped: list[str] = []
    # lstat-classified, no link followed, no FIFO or device opened (review r1-c1 1, 16).
    for e in sorted(safefs.walk(root), key=lambda e: e.rel):
        if e.kind != "file":
            if e.kind not in ("dir", "link"):
                skipped.append(e.rel)
            continue
        if e.size > max_bytes:
            skipped.append(e.rel)
            continue
        try:
            data = safefs.read_bytes(root, e.rel, limit=max_bytes)
        except OSError:
            skipped.append(e.rel)
            continue
        scanned += 1
        if data_has_key(e.rel, data, ring):
            hits.append(e.rel)
    return {"files_scanned": scanned, "files_with_key": hits, "skipped": skipped, "keys_checked": len(ring.secrets())}


def resolve_key(entry: Mapping, env: Mapping[str, str]) -> tuple[str | None, list[str]]:
    """craze's rule: first non-empty env_keys variable, else api_key.

    Returns the chosen key and every candidate (for scrubbing), never printing either.
    """
    candidates: list[str] = []
    chosen = None
    for var in entry.get("env_keys") or []:
        v = (env.get(var) or "").strip()
        if v:
            candidates.append(v)
            if chosen is None:
                chosen = v
    file_key = (entry.get("api_key") or "").strip()
    if file_key:
        candidates.append(file_key)
        if chosen is None:
            chosen = file_key
    return chosen, candidates


def load_providers(
    path: Path | None = None,
    env: Mapping[str, str] | None = None,
    only: tuple[str, ...] = EVAL_PROVIDERS,
) -> tuple[dict[str, Provider], KeyRing]:
    """Read the owner's providers.toml (read-only) into a route table and a key ring."""
    path = path or paths.OWNER_CRAZE_NATIVE / "providers.toml"
    env = os.environ if env is None else env
    with open(path, "rb") as f:
        doc = tomllib.load(f)
    table: dict[str, Provider] = {}
    chosen: dict[str, str] = {}
    extra: list[str] = []
    for name, entry in (doc.get("providers") or {}).items():
        key, candidates = resolve_key(entry, env)
        extra.extend(candidates)
        if name not in only:
            continue
        driver = entry.get("driver", "")
        base = entry.get("base_url") or (OPENROUTER_BASE if driver == "openrouter" else "")
        if not base:
            continue
        table[name] = Provider.from_url(name, driver, base)
        if key:
            chosen[name] = key
    return table, KeyRing(chosen, tuple(extra))
