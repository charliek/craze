"""``crazeeval pack``: only the allowlisted files of a batch reach the tarball -- never
home/, a workspace, a scoring copy or a lock -- held-out only with unseal; a planted key
in a selected file (a gzipped capture too) fails before anything is written; a symlink
is refused; a dry run writes nothing; members are ``<batch>/<relative path>``."""

from __future__ import annotations

import gzip
import hashlib
import io
import json
import os
import random
import tarfile
from pathlib import Path

import pytest
from fakes import ZAI_KEY, keyring

from crazeeval import keys as keymod
from crazeeval import pack as pk
from crazeeval.cli import main

REP = "runs/craze/m-1/D1/rep1"
ATT = f"{REP}/attempt-1"
HO_REP = "heldout/craze/m-1/H1/rep1"
HO_ATT = f"{HO_REP}/attempt-1"

# What a sealed pack keeps.
KEPT = {
    "batch.json", "manifest.json", "manifest.resume.json", "results.jsonl", "summary.json", "validation.json",
    "rescore.jsonl", "best-open-harness.json",
    "judging/verdicts.jsonl", "judging/verdicts.pre-stamp.jsonl",
    "calibration/calibration.json", "calibration/sol.jsonl",
    "captures/captures.md", "captures/prompts/m-1/craze.system.txt",
    f"{REP}/result.json", f"{REP}/result.pre-rescore.json",
    f"{ATT}/result.json", f"{ATT}/result.pre-rescore.json", f"{ATT}/stdout.jsonl", f"{ATT}/stderr.txt",
    f"{ATT}/diff.patch", f"{ATT}/start-manifest.json", f"{ATT}/final-manifest.json", f"{ATT}/capture.jsonl.gz",
    f"{ATT}/gitpost/diff.patch", f"{ATT}/gitpost/head.txt", f"{ATT}/gitpost/add.rc",
    f"{ATT}/scoring/tests-hidden.stdout", f"{ATT}/scoring/tests-hidden.stderr",
    f"{ATT}/scoring/tests-hidden.out/junit.xml",
    f"{REP}/attempt-2/result.json", f"{REP}/attempt-2/capture.jsonl",
}
# ... and what only an unsealed one adds.
HELD_OUT = {
    "heldout/results.jsonl", "heldout/rescore.jsonl", "judging/heldout/verdicts.jsonl",
    f"{HO_REP}/result.json", f"{HO_ATT}/result.json", f"{HO_ATT}/stdout.jsonl", f"{HO_ATT}/capture.jsonl.gz",
}
# Never packed.
LEFT_OUT = {
    ".lock", "proxy-refusals.jsonl", "probe.json",
    "judging/verdicts.jsonl.lock", "calibration/sol.jsonl.lock", "judging/heldout/verdicts.jsonl.lock",
    f"{ATT}/home/.craze/native/providers.toml", f"{ATT}/home/.cache/go-build/ab/cd", f"{ATT}/home/opencode.json",
    f"{ATT}/ws/app.py", f"{ATT}/ws/.git/HEAD", f"{ATT}/gocache/x", f"{ATT}/notes.log",
    f"{ATT}/scoring/tests-hidden/test_app.py", f"{ATT}/scoring/pristine/app.py", f"{ATT}/scoring/notes.txt",
    f"{ATT}/gitpost/sub/x", f"{REP}/stray.txt", "runs/craze/m-1/D1/notarep/result.json",
    f"{REP}/attempt-x/result.json", "heldout/other.txt", f"{HO_ATT}/home/state.json",
}


def _gz(text: str) -> bytes:
    return gzip.compress(text.encode())


def _batch(root: Path, name: str = "final-x", files: dict[str, bytes] | None = None) -> Path:
    b = root / "camp" / "eval-runs" / name
    for rel in sorted(KEPT | HELD_OUT | LEFT_OUT):
        p = b / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        data = _gz(f'{{"line": "{rel}"}}\n') if rel.endswith(".gz") else f"{name}:{rel}\n".encode()
        p.write_bytes((files or {}).get(rel, data))
    (b / "batch.json").write_text(json.dumps({"label": name}))
    return b


def _members(out: Path) -> dict[str, bytes]:
    with tarfile.open(out, "r:gz") as tar:
        return {m.name: tar.extractfile(m).read() for m in tar.getmembers()}


def _pack(batches, out, **kw) -> dict:
    return pk.pack(list(batches), out, keyring(), **kw)


def test_only_the_allowlist_is_packed(tmp_path):
    b = _batch(tmp_path)
    out = tmp_path / "packs" / "raw.tar.gz"
    s = _pack([b], out)
    assert s["ok"] and s["errors"] == [] and not s["unsealed"]
    got = _members(out)
    assert set(got) == {f"final-x/{rel}" for rel in KEPT}
    for rel in KEPT:
        assert got[f"final-x/{rel}"] == (b / rel).read_bytes(), rel
    assert s["files"] == len(KEPT) and s["bytes"] == sum((b / r).stat().st_size for r in KEPT)
    assert s["batches"] == [{"name": "final-x", "files": len(KEPT), "bytes": s["bytes"]}]
    man = json.loads((tmp_path / "packs" / "raw.tar.gz.manifest.json").read_text())
    assert man["sha256"] == hashlib.sha256(out.read_bytes()).hexdigest() == s["sha256"]
    assert (man["tarball"], man["files"], man["bytes"], man["unsealed"]) == ("raw.tar.gz", len(KEPT), s["bytes"], False)
    assert man["compressed_bytes"] == out.stat().st_size and [x["name"] for x in man["batches"]] == ["final-x"]
    assert man["key_scan"] == {"files_scanned": len(KEPT), "keys_checked": len(keyring().secrets())}
    assert sorted(p.name for p in (tmp_path / "packs").iterdir()) == ["raw.tar.gz", "raw.tar.gz.manifest.json"]


def test_held_out_only_with_unseal(tmp_path):
    b = _batch(tmp_path)
    sealed = _pack([b], tmp_path / "sealed.tar.gz")
    assert not any("heldout" in m for m in _members(tmp_path / "sealed.tar.gz"))
    opened = _pack([b], tmp_path / "open.tar.gz", unseal=True)
    assert opened["unsealed"] and set(_members(tmp_path / "open.tar.gz")) == {f"final-x/{r}" for r in KEPT | HELD_OUT}
    assert opened["files"] == sealed["files"] + len(HELD_OUT)
    assert json.loads((tmp_path / "open.tar.gz.manifest.json").read_text())["unsealed"] is True


@pytest.mark.parametrize("rel,unseal", [
    (f"{ATT}/stdout.jsonl", False),
    (f"{ATT}/capture.jsonl.gz", False),  # inside the gzip member
    ("judging/verdicts.jsonl", False),
    (f"{HO_ATT}/stdout.jsonl", True),
])
def test_a_planted_key_in_a_selected_file_fails_and_writes_nothing(tmp_path, rel, unseal):
    text = f'{{"reasons": "the answer printed {ZAI_KEY}"}}\n'
    b = _batch(tmp_path, files={rel: _gz(text) if rel.endswith(".gz") else text.encode()})
    out = tmp_path / "packs" / "raw.tar.gz"
    s = _pack([b], out, unseal=unseal)
    assert not s["ok"] and s["key_scan"]["files_with_key"] == [f"final-x/{rel}"]
    assert "nothing written" in s["errors"][0] and ZAI_KEY not in json.dumps(s)
    assert not (tmp_path / "packs").exists()


def test_the_scan_covers_exactly_the_selected_files(tmp_path):
    """A key where nothing is packed (a harness home, a workspace, a sealed held-out
    file) does not fail the pack: the scan is over the selection, which leaves them out."""
    key = f"{ZAI_KEY}\n".encode()
    b = _batch(tmp_path, files={f"{ATT}/home/.craze/native/providers.toml": key, f"{ATT}/ws/app.py": key,
                                f"{HO_ATT}/stdout.jsonl": key})
    s = _pack([b], tmp_path / "raw.tar.gz")
    assert s["ok"] and s["key_scan"]["files_with_key"] == []
    assert not any(ZAI_KEY.encode() in data for data in _members(tmp_path / "raw.tar.gz").values())


@pytest.mark.parametrize("where", [f"{ATT}/stdout.jsonl", "judging", f"{ATT}/gitpost", f"{ATT}/ws"])
def test_a_symlink_is_refused(tmp_path, where):
    b = _batch(tmp_path)
    secret = tmp_path / "outside"
    secret.mkdir()
    (secret / "stdout.jsonl").write_text("owner's file\n")
    target = b / where
    if target.is_dir():
        import shutil

        shutil.rmtree(target)
        target.symlink_to(secret, target_is_directory=True)
    else:
        target.unlink()
        target.symlink_to(secret / "stdout.jsonl")
    out = tmp_path / "packs" / "raw.tar.gz"
    with pytest.raises(pk.PackError, match="symlink"):
        _pack([b], out)
    assert not (tmp_path / "packs").exists()


def test_a_symlinked_batch_or_a_link_in_an_unwalked_tree(tmp_path):
    b = _batch(tmp_path)
    link = tmp_path / "linked-batch"
    link.symlink_to(b, target_is_directory=True)
    with pytest.raises(pk.PackError, match="not a batch directory"):
        _pack([link], tmp_path / "raw.tar.gz")
    # home/ is never walked: a link the harness left there is neither followed nor refused.
    (b / ATT / "home" / "auth.json").symlink_to(tmp_path / "anything")
    assert _pack([b], tmp_path / "raw.tar.gz")["ok"]


def test_a_dry_run_writes_nothing(tmp_path, monkeypatch, capsys):
    b = _batch(tmp_path)
    out = tmp_path / "packs" / "raw.tar.gz"
    s = _pack([b], out, unseal=True, dry_run=True)
    assert s["ok"] and s["dry_run"] and s["files"] == len(KEPT | HELD_OUT) and "out" not in s
    assert not (tmp_path / "packs").exists()
    monkeypatch.setattr(keymod, "load_providers", lambda *a, **k: ({}, keyring()))
    assert main(["pack", "--dry-run", "--batch", str(b), "--out", str(out)]) == 0
    printed = capsys.readouterr().out
    assert f"  final-x: {len(KEPT)} files, {sum((b / r).stat().st_size for r in KEPT)} bytes" in printed
    assert "held-out left out (sealed)" in printed and "files with a key: none" in printed
    assert "dry run: nothing written" in printed and not (tmp_path / "packs").exists()


def test_members_are_relative_to_their_batch(tmp_path):
    a = _batch(tmp_path, "final-a")
    b = _batch(tmp_path / "other", "final-b")
    out = tmp_path / "raw.tar.gz"
    s = _pack([a, b, a], out)  # a batch given twice is packed once
    assert s["ok"] and [x["name"] for x in s["batches"]] == ["final-a", "final-b"]
    with tarfile.open(out, "r:gz") as tar:
        members = tar.getmembers()
    assert len(members) == 2 * len(KEPT)
    for m in members:
        assert m.isfile() and not m.name.startswith("/") and ".." not in m.name.split("/"), m.name
        assert m.name.split("/", 1)[0] in ("final-a", "final-b") and str(tmp_path) not in m.name
        assert (m.uid, m.gid, m.uname, m.gname, m.mode) == (0, 0, "", "", 0o644)
    raw = gzip.GzipFile(fileobj=io.BytesIO(out.read_bytes()))
    raw.read()
    assert raw.mtime == 0  # no time in the gzip header


def test_the_same_batches_pack_to_the_same_bytes(tmp_path):
    b = _batch(tmp_path)
    one, two = tmp_path / "one.tar.gz", tmp_path / "two.tar.gz"
    assert _pack([b], one)["sha256"] == _pack([b], two)["sha256"]
    assert one.read_bytes() == two.read_bytes()


def test_refusals(tmp_path):
    b = _batch(tmp_path)
    same_name = _batch(tmp_path / "elsewhere", "final-x")
    with pytest.raises(pk.PackError, match="two batches named 'final-x'"):
        _pack([b, same_name], tmp_path / "raw.tar.gz")
    with pytest.raises(pk.PackError, match=r"must end in \.tar\.gz"):
        _pack([b], tmp_path / "raw.tar")
    with pytest.raises(pk.PackError, match="read-only inputs"):
        _pack([b], b / "captures" / "raw.tar.gz")
    (tmp_path / "exists.tar.gz").write_bytes(b"old")
    with pytest.raises(pk.PackError, match="never replaces"):
        _pack([b], tmp_path / "exists.tar.gz")
    assert (tmp_path / "exists.tar.gz").read_bytes() == b"old"
    (tmp_path / "side.tar.gz.manifest.json").write_text("{}")
    with pytest.raises(pk.PackError, match="never replaces"):
        _pack([b], tmp_path / "side.tar.gz")
    (b / "batch.json").unlink()
    with pytest.raises(pk.PackError, match="no batch.json"):
        _pack([b], tmp_path / "raw.tar.gz")
    assert not (tmp_path / "raw.tar.gz").exists()


def test_the_cli_writes_and_reports(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(keymod, "load_providers", lambda *a, **k: ({}, keyring()))
    b = _batch(tmp_path)
    out = tmp_path / "raw.tar.gz"
    assert main(["pack", "--batch", str(b), "--out", str(out), "--unseal"]) == 0
    printed = capsys.readouterr().out
    n = len(KEPT | HELD_OUT)
    assert f"({out.stat().st_size} bytes compressed, {n} files; sha256 " in printed
    assert f"manifest: {out}.manifest.json" in printed and "held-out included (--unseal)" in printed
    assert main(["pack", "--batch", str(b), "--out", str(out)]) == 2  # never replaced
    (b / ATT / "stderr.txt").write_text(f"leak {ZAI_KEY}\n")
    assert main(["pack", "--batch", str(b), "--out", str(tmp_path / "second.tar.gz")]) == 1
    err = capsys.readouterr().err
    assert "FAILED: key scan" in err and ZAI_KEY not in err and not (tmp_path / "second.tar.gz").exists()
    monkeypatch.setattr(keymod, "load_providers", lambda *a, **k: ({}, keymod.KeyRing({})))
    assert main(["pack", "--dry-run", "--batch", str(b), "--out", str(tmp_path / "third.tar.gz")]) == 2


def test_a_batch_named_through_dotdot_packs_under_its_real_name(tmp_path):
    """``<batch>/runs/..`` is the batch itself: its members are ``final-x/...``, never
    ``../...`` (the given path's own last component)."""
    b = _batch(tmp_path)
    out = tmp_path / "raw.tar.gz"
    s = _pack([b / "runs" / ".."], out)
    assert s["ok"] and s["batches"][0]["name"] == "final-x"
    names = set(_members(out))
    assert names == {f"final-x/{rel}" for rel in KEPT}
    assert not any(n.startswith(("..", "/")) or ".." in n.split("/") for n in names)
    # The same batch through ".." and directly is one batch, not a name collision.
    assert _pack([b, b / "runs" / ".."], tmp_path / "again.tar.gz")["files"] == len(KEPT)


@pytest.mark.parametrize("prefix,rel", [
    ("..", "batch.json"), ("final-x", "../batch.json"), ("final-x", "runs/../../x"), ("", "batch.json"),
    ("final-x", "/etc/passwd"), ("final-x", "a//b"), (".", "batch.json"), ("final-x", "runs/./x"),
])
def test_a_member_name_that_is_not_a_plain_relative_path_is_refused(prefix, rel):
    with pytest.raises(pk.PackError, match="not a plain relative path"):
        pk.member_name(prefix, rel)


def test_a_plain_member_name(tmp_path):
    assert pk.member_name("final-x", f"{ATT}/stdout.jsonl") == f"final-x/{ATT}/stdout.jsonl"


KEY = ZAI_KEY.encode()


def test_a_gzip_is_scanned_to_its_end_across_chunk_boundaries(monkeypatch):
    """The scan once read 256 MiB of a gzip and stopped. With a 64-byte chunk standing in
    for that limit, a key far past the first read -- at every alignment across a chunk
    boundary, so split between two chunks -- in a second gzip member, or in the readable
    part of a truncated file, is found."""
    monkeypatch.setattr(keymod, "GZ_CHUNK", 64)
    ring = keyring()
    filler = b"x" * (64 * 40)
    for off in range(64 * 30 - len(KEY), 64 * 30 + 2):
        data = filler[:off] + KEY + filler[off:]
        assert keymod.data_has_key("capture.jsonl.gz", gzip.compress(data), ring), off
    assert not keymod.data_has_key("capture.jsonl.gz", gzip.compress(filler), ring)
    two_members = gzip.compress(filler) + gzip.compress(filler + KEY)
    assert keymod.data_has_key("capture.jsonl.gz", two_members, ring)
    # Truncated mid-stream (incompressible bytes, so half the file is half the data):
    # what decompressed before the cut is scanned.
    whole = gzip.compress(KEY + random.Random(29).randbytes(64 * 100))
    assert keymod.data_has_key("capture.jsonl.gz", whole[: len(whole) // 2], ring)
    assert keymod.data_has_key("capture.jsonl.gz", b"raw " + KEY, ring)  # not gzip at all: the raw bytes


def test_a_key_past_the_old_scan_limit_fails_the_pack(tmp_path, monkeypatch):
    monkeypatch.setattr(keymod, "GZ_CHUNK", 64)
    b = _batch(tmp_path, files={f"{ATT}/capture.jsonl.gz": gzip.compress(b"{}\n" * 5000 + KEY + b"\n")})
    s = _pack([b], tmp_path / "raw.tar.gz")
    assert not s["ok"] and s["key_scan"]["files_with_key"] == [f"final-x/{ATT}/capture.jsonl.gz"]
    assert not (tmp_path / "raw.tar.gz").exists()


def _left(d: Path) -> list[str]:
    return sorted(p.name for p in d.iterdir() if p.name != "camp")


def test_a_failed_sidecar_write_leaves_nothing_and_a_retry_succeeds(tmp_path, monkeypatch):
    b = _batch(tmp_path)
    out = tmp_path / "raw.tar.gz"

    def disk_full(f, manifest):
        f.write(b'{"partial": ')
        raise OSError(28, "No space left on device")

    with monkeypatch.context() as mp:
        mp.setattr(pk, "_write_manifest", disk_full)
        with pytest.raises(pk.PackError, match="could not be written.*nothing kept"):
            _pack([b], out)
    assert _left(tmp_path) == []  # no tarball, no sidecar, no temporaries
    s = _pack([b], out)
    assert s["ok"] and _left(tmp_path) == ["raw.tar.gz", "raw.tar.gz.manifest.json"]


def test_a_failed_publish_of_the_tarball_takes_the_published_sidecar_with_it(tmp_path, monkeypatch):
    b = _batch(tmp_path)
    real = pk._publish

    def publish(tmp, final, ours):
        if final.name == "raw.tar.gz":
            raise OSError(5, "Input/output error")
        real(tmp, final, ours)

    monkeypatch.setattr(pk, "_publish", publish)
    with pytest.raises(pk.PackError, match="could not be written"):
        _pack([b], tmp_path / "raw.tar.gz")
    assert _left(tmp_path) == []


def test_a_file_this_call_did_not_create_is_never_removed(tmp_path):
    """A stale temporary (another run's, same pid) makes the pack fail -- and stays."""
    b = _batch(tmp_path)
    stale = tmp_path / f".raw.tar.gz.tmp-{os.getpid()}"
    stale.write_bytes(b"someone else's")
    with pytest.raises(pk.PackError, match="could not be written"):
        _pack([b], tmp_path / "raw.tar.gz")
    assert _left(tmp_path) == [stale.name] and stale.read_bytes() == b"someone else's"


def test_a_file_that_changes_after_the_scan_is_not_packed(tmp_path, monkeypatch):
    """The tarball is written from a second read checked against the scanned bytes."""
    b = _batch(tmp_path)
    real = pk._read_whole
    calls = {"n": 0}

    def swap(batch, p):
        data = real(batch, p)
        if p.rel == f"{ATT}/stdout.jsonl":
            calls["n"] += 1
            if calls["n"] == 2:  # the write pass: same size, other bytes
                return bytes(reversed(data))
        return data

    monkeypatch.setattr(pk, "_read_whole", swap)
    out = tmp_path / "raw.tar.gz"
    with pytest.raises(pk.PackError, match="changed since the key scan"):
        _pack([b], out)
    assert not out.exists() and [p.name for p in tmp_path.iterdir() if p.name.startswith(".raw")] == []
    assert os.listdir(tmp_path) == ["camp"]
