"""``crazeeval pack``: an optional, local tarball of a campaign's raw runs.

Raw runs stay local, in the campaign directory. ``pack`` compacts them into a tarball
kept on the owner's disk, without the harness homes and caches (plan
029: 3.0 GB of batches select to 0.8 GB before compression). What is kept is an
**allowlist**, per batch (paths relative to the batch):

- batch level: ``batch.json``, ``manifest.json``, ``summary.json`` (and the
  ``*.resume.json`` pair a ``--resume`` writes), ``results.jsonl``, ``validation.json``,
  ``rescore.jsonl``, ``best-open-harness.json``; everything under ``judging/``,
  ``calibration/`` and ``captures/`` (verdict and calibration files, their ``.pre-*``
  originals, the capture report) but the ``*.lock`` files beside them;
- per rep (``runs/<harness>/<model>/<task>/rep<N>/``): ``result.json`` and
  ``result.pre-rescore.json``;
- per attempt (``.../rep<N>/attempt-<n>/``): ``result.json`` (and its pre-rescore
  original), ``stdout.jsonl``, ``stderr.txt``, ``diff.patch``, ``start-manifest.json``,
  ``final-manifest.json``, ``capture.jsonl.gz`` (or a plain ``capture.jsonl``), the
  files of ``gitpost/``, and the scoring results: ``scoring/<label>.stdout`` and
  ``.stderr`` (``go test -json`` is the stdout) and any ``scoring/<label>.out/`` report
  file (JUnit) -- never a scoring workspace copy;
- held-out only with ``unseal``: ``heldout/results.jsonl``, ``heldout/rescore.jsonl``,
  the ``heldout/`` runs as above, and ``judging/heldout/``.

Everything else stays out: ``home/`` (harness state, vendored binaries, caches --
regenerable; the capture holds the wire exchange and ``stdout.jsonl`` the harness's own
stream), ``ws/`` workspaces, Go caches, lock files, probe and proxy leftovers.

A batch is named by its resolved directory (``eval-runs/x/runs/..`` is ``x``). The batch
is walked by ``lstat`` without following a link (``safefs.walk``), entering only
allowlisted directories; a symlink or special file met there refuses the pack. Every
selected file is then read (``safefs``, ``O_NOFOLLOW``) and grepped for every loaded key
with the batch runner's scan (``keys.data_has_key``: gzip members decompressed to the
end); any hit fails the pack before anything is written. Members are ``<batch
name>/<relative path>``, each checked to be a plain relative path (no ``..``, no empty
component, no leading ``/``), with no owner names. The tarball is written from a second
read of each file, checked against the scanned bytes' sha256; the gzip header has no
name or time, so the same batches pack to the same bytes. A sidecar
``<FILE>.manifest.json`` records the batch names, file count, bytes before compression
and the tarball's sha256. Both are staged as temporaries beside ``FILE`` and then
published (the sidecar, then the tarball), neither ever replacing a file; on a failure
whatever this call staged or published is removed. ``dry_run`` selects and scans and
writes nothing.
"""

from __future__ import annotations

import gzip
import hashlib
import io
import json
import os
import re
import tarfile
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

from crazeeval import keys as keymod
from crazeeval import safefs

SUFFIX = ".tar.gz"
MANIFEST_SUFFIX = ".manifest.json"
MAX_FILE = keymod.SCAN_MAX_BYTES  # a file the key scan cannot read whole is never packed
BATCH_FILES = frozenset({
    "batch.json", "manifest.json", "manifest.resume.json", "summary.json", "summary.resume.json", "results.jsonl",
    "validation.json", "rescore.jsonl", "best-open-harness.json",
})
BATCH_TREES = ("judging", "calibration", "captures")
HELDOUT_FILES = frozenset({"results.jsonl", "rescore.jsonl"})
REP_FILES = frozenset({"result.json", "result.pre-rescore.json"})
ATTEMPT_FILES = frozenset({
    "result.json", "result.pre-rescore.json", "stdout.jsonl", "stderr.txt", "diff.patch", "start-manifest.json",
    "final-manifest.json", "capture.jsonl.gz", "capture.jsonl",
})
REP_RE = re.compile(r"^rep\d+$")
ATTEMPT_RE = re.compile(r"^attempt-\d+$")
SCORING_LOG_RE = re.compile(r"^[^/]+\.(?:stdout|stderr)$")
REPORT_DIR_SUFFIX = ".out"  # scoring/<label>.out/: a runner's report directory (junit.xml)


class PackError(RuntimeError):
    """The pack cannot be made as asked (nothing is written)."""


def _runs_root(parts: list[str], unseal: bool) -> bool:
    return parts[0] == "runs" or (parts[0] == "heldout" and unseal)


def wanted_dir(rel: str, unseal: bool = False) -> bool:
    """Whether the walk enters the batch directory ``rel``: the allowlist's directories."""
    parts = rel.split("/")
    if parts[0] in BATCH_TREES:
        return unseal or "heldout" not in parts
    if not _runs_root(parts, unseal):
        return False
    run = parts[1:]  # <harness>/<model>/<task>/rep<N>/attempt-<n>/<dir>/<dir>
    if len(run) <= 3:
        return True
    if not REP_RE.match(run[3]):
        return False
    if len(run) == 4:
        return True
    if not ATTEMPT_RE.match(run[4]):
        return False
    if len(run) == 5:
        return True
    if run[5] == "gitpost":
        return len(run) == 6
    if run[5] == "scoring":
        return len(run) == 6 or (len(run) == 7 and run[6].endswith(REPORT_DIR_SUFFIX))
    return False


def wanted_file(rel: str, unseal: bool = False) -> bool:
    """Whether the batch file ``rel`` is packed (the allowlist; see the module doc)."""
    parts = rel.split("/")
    name = parts[-1]
    if len(parts) == 1:
        return name in BATCH_FILES
    if parts[0] in BATCH_TREES:
        return not name.endswith(".lock") and (unseal or "heldout" not in parts)
    if parts[0] == "heldout" and len(parts) == 2:
        return unseal and name in HELDOUT_FILES
    if not _runs_root(parts, unseal):
        return False
    run = parts[1:]
    if len(run) < 5 or not REP_RE.match(run[3]):
        return False
    if len(run) == 5:
        return name in REP_FILES
    if not ATTEMPT_RE.match(run[4]):
        return False
    if len(run) == 6:
        return name in ATTEMPT_FILES
    if run[5] == "gitpost":
        return len(run) == 7
    if run[5] == "scoring":
        return (len(run) == 7 and bool(SCORING_LOG_RE.match(name))) or (
            len(run) == 8 and run[6].endswith(REPORT_DIR_SUFFIX))
    return False


@dataclass(frozen=True)
class Picked:
    rel: str
    size: int
    mtime: float


def select(batch: Path, unseal: bool = False) -> list[Picked]:
    """The allowlisted files of ``batch``, sorted by path. PackError when a symlink or
    special file sits in the walked part of the batch (never followed, never packed)."""
    batch = Path(batch)
    refused, picked = [], []
    try:
        for e in safefs.walk(batch, skip_dir=lambda rel: not wanted_dir(rel, unseal)):
            if e.kind == "dir":
                continue
            if e.kind != "file":
                refused.append(f"{e.rel} ({e.kind})")
            elif wanted_file(e.rel, unseal):
                picked.append(Picked(e.rel, e.size, e.mtime))
    except OSError as e:
        raise PackError(f"{batch.name}: cannot be walked safely ({e})") from e
    if refused:
        raise PackError(f"{batch.name}: refusing to pack a batch with a symlink or special file: {sorted(refused)[:10]}")
    return sorted(picked, key=lambda p: p.rel)


def _read_whole(batch: Path, p: Picked) -> bytes:
    """The file's bytes through ``safefs`` (no link followed), exactly ``p.size`` of them."""
    if p.size > MAX_FILE:
        raise PackError(f"{Path(batch).name}/{p.rel}: {p.size} bytes, over the key scan's {MAX_FILE}-byte limit")
    try:
        data = safefs.read_bytes(batch, p.rel, limit=p.size + 1)
    except OSError as e:
        raise PackError(f"{Path(batch).name}/{p.rel}: cannot be read safely ({type(e).__name__})") from e
    if len(data) != p.size:
        raise PackError(f"{Path(batch).name}/{p.rel}: changed while being packed ({len(data)} bytes, not {p.size})")
    return data


def member_name(prefix: str, rel: str) -> str:
    """The tarball member for ``rel`` in the batch named ``prefix``: ``<prefix>/<rel>``,
    refused unless it is relative with no empty, ``.`` or ``..`` component (a member can
    never land outside the directory it is extracted into)."""
    name = f"{prefix}/{rel}"
    if name.startswith("/") or any(p in ("", ".", "..") for p in name.split("/")):
        raise PackError(f"refusing the tarball member name {name!r}: not a plain relative path")
    return name


def _check_batches(batches: list[Path]) -> list[Path]:
    """The batch directories, resolved and each read once: real directories (the given
    path not a link) holding a ``batch.json``, with distinct names. A batch is named by
    its resolved directory (``runs/..`` is its parent's name, never ``..``): the
    tarball's top-level directory."""
    seen, out, names = set(), [], {}
    for b in (Path(x) for x in batches):
        if b.is_symlink() or not b.is_dir():
            raise PackError(f"{b}: not a batch directory (a symlink, or not a directory)")
        r = b.resolve()
        bj = r / "batch.json"
        if bj.is_symlink() or not bj.is_file():
            raise PackError(f"{b}: no batch.json (not a batch directory)")
        member_name(r.name, "batch.json")  # a usable top-level name ("/" resolves to none)
        if r in seen:
            continue
        seen.add(r)
        if r.name in names:
            raise PackError(f"two batches named {r.name!r} ({names[r.name]} and {r}): the tarball's "
                            "top-level directories would collide")
        names[r.name] = r
        out.append(r)
    if not out:
        raise PackError("no batch given")
    return out


def _check_out(out: Path, batches: list[Path]) -> None:
    if not out.name.endswith(SUFFIX) or out.name == SUFFIX:
        raise PackError(f"--out {out}: the tarball's name must end in {SUFFIX}")
    sidecar = out.with_name(out.name + MANIFEST_SUFFIX)
    for p in (out, sidecar):
        if os.path.lexists(p):
            raise PackError(f"--out {out}: {p} exists; pack never replaces a tarball or its manifest")
    o = out.resolve()
    for b in batches:
        rb = Path(b).resolve()
        if rb in o.parents:
            raise PackError(f"--out {out} is inside the batch {b}: batches are read-only inputs")


def _sha256_file(p: Path) -> str:
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


@contextmanager
def _create(p: Path, ours: list[Path]):
    """``p`` opened for writing, created exclusively -- and only then recorded in
    ``ours`` (a file that was already there is never ours to remove)."""
    with open(p, "xb") as f:
        ours.append(p)
        yield f


def _write_tarball(raw, selected: list[tuple[Path, Picked, str, str]]) -> None:
    """The tarball, written to ``raw`` from a second read of each selected file, checked
    against the sha256 of the bytes the key scan read. The gzip header carries no name
    (not the temporary file's) and no time: the same batches pack to the same bytes
    (members keep their files' mtimes)."""
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=6, mtime=0) as gz, \
            tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for b, p, member, sha in selected:
            data = _read_whole(b, p)
            if hashlib.sha256(data).hexdigest() != sha:
                raise PackError(f"{b.name}/{p.rel}: changed since the key scan")
            ti = tarfile.TarInfo(member)
            ti.size, ti.mtime, ti.mode = p.size, int(p.mtime), 0o644
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = ""
            tar.addfile(ti, io.BytesIO(data))


def _write_manifest(f, manifest: dict) -> None:
    f.write((json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8"))


def _publish(tmp: Path, final: Path, ours: list[Path]) -> None:
    """Give the staged ``tmp`` its final name without ever replacing a file there: a
    hard link (it fails if ``final`` exists; ``final`` is ours from then on), then the
    temporary name dropped."""
    try:
        os.link(tmp, final)
    except FileExistsError as e:
        raise PackError(f"{final} appeared while packing; not replaced") from e
    ours.append(final)
    os.unlink(tmp)


def pack(batches: list[Path], out: Path, keyring: keymod.KeyRing, *, unseal: bool = False,
         dry_run: bool = False) -> dict:
    """Select, key-scan and (unless ``dry_run``) write the tarball of ``batches`` and its
    sidecar manifest. PackError for a refusal (a symlink, a name collision, an existing
    ``out``, a member name that is not a plain relative path...) or a failed write; a
    key hit returns ``ok`` false with ``errors`` and writes nothing.

    Writing stages both files as temporaries beside ``out`` -- the tarball, then the
    manifest carrying its sha256 -- and only then publishes the sidecar and the tarball,
    neither ever replacing a file. On any failure every file this call staged or
    published is removed: a tarball never stays without its manifest, so a retry is
    never refused by a half-written pack."""
    batches = _check_batches(batches)
    out = Path(out)
    _check_out(out, batches)
    # (batch, file, member name, sha256 of the scanned bytes)
    selected: list[tuple[Path, Picked, str, str]] = []
    per_batch, hits = [], []
    for b in batches:
        picked = select(b, unseal)
        for p in picked:
            member = member_name(b.name, p.rel)
            data = _read_whole(b, p)
            if keymod.data_has_key(p.rel, data, keyring):
                hits.append(member)
            selected.append((b, p, member, hashlib.sha256(data).hexdigest()))
        per_batch.append({"name": b.name, "files": len(picked), "bytes": sum(p.size for p in picked)})
    errors = []
    if hits:
        more = f" and {len(hits) - 10} more" if len(hits) > 10 else ""
        errors.append(f"key scan: a loaded key in {hits[:10]}{more} -- nothing written")
    summary = {
        "batches": per_batch,
        "files": len(selected),
        "bytes": sum(p.size for _, p, _, _ in selected),
        "unsealed": unseal,
        "dry_run": dry_run,
        "key_scan": {"files_scanned": len(selected), "keys_checked": len(keyring.secrets()), "files_with_key": hits},
        "errors": errors,
        "ok": not hits,
    }
    if hits or dry_run:
        return summary
    out.parent.mkdir(parents=True, exist_ok=True)
    sidecar = out.with_name(out.name + MANIFEST_SUFFIX)
    tmp_tar = out.with_name(f".{out.name}.tmp-{os.getpid()}")
    tmp_side = out.with_name(f".{sidecar.name}.tmp-{os.getpid()}")
    ours: list[Path] = []  # every file this call created: removed again on failure
    try:
        with _create(tmp_tar, ours) as raw:
            _write_tarball(raw, selected)
        compressed, sha = tmp_tar.stat().st_size, _sha256_file(tmp_tar)
        manifest = {
            "tarball": out.name,
            "sha256": sha,
            "compressed_bytes": compressed,
            "files": summary["files"],
            "bytes": summary["bytes"],
            "unsealed": unseal,
            "batches": per_batch,
            "key_scan": {k: v for k, v in summary["key_scan"].items() if k != "files_with_key"},
        }
        with _create(tmp_side, ours) as f:
            _write_manifest(f, manifest)
        for tmp, final in ((tmp_side, sidecar), (tmp_tar, out)):
            _publish(tmp, final, ours)
    except BaseException as e:
        for p in reversed(ours):
            if os.path.lexists(p):
                os.unlink(p)
        if isinstance(e, OSError):
            raise PackError(f"--out {out}: the tarball or its manifest could not be written ({e}); "
                            "nothing kept") from e
        raise
    summary.update({"out": str(out), "compressed_bytes": compressed, "sha256": sha, "manifest": str(sidecar)})
    return summary


