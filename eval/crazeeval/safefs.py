"""Host-side access to agent-controlled trees without following the agent's links.

A run's workspace and home are written by the agent inside the sandbox; the host
later reads, copies, hashes, restores and prunes them. Every such access goes
through here (review r1-c1 findings 1, 3, 16):

- paths are walked one component at a time from a directory fd, each opened with
  ``O_NOFOLLOW`` -- a symlinked component anywhere is refused, never followed;
- only regular files are opened for reading, and with ``O_NONBLOCK`` so a FIFO can
  never block (it is classified by ``lstat`` first and skipped);
- writes replace whatever is at the destination (unlink first, then
  ``O_CREAT|O_EXCL|O_NOFOLLOW``), so nothing is ever written through a symlink;
- work is bounded (bytes read per file, files walked); a post-run copy refuses a
  file over ``FILE_LIMIT`` or a tree over ``TREE_LIMIT`` (review r2-c1 item 14), and a
  manifest never fingerprints a file it could not read whole.
"""

from __future__ import annotations

import errno
import hashlib
import os
import shutil
import stat
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
FILE_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC
READ_LIMIT = 64 * 1024 * 1024
WALK_LIMIT = 500_000
# Post-run limits on an agent's tree: one file, and everything copied.
FILE_LIMIT = 50 * 1024 * 1024
TREE_LIMIT = 500 * 1024 * 1024


class UnsafePath(OSError):
    """A path component was a symlink, not a directory, or not a regular file."""


class Oversize(OSError):
    """An agent's file or tree is over the post-run limits: the run fails as oversize."""


def _parts(rel: str) -> list[str]:
    parts = [p for p in str(rel).replace("\\", "/").split("/") if p not in ("", ".")]
    if not parts or any(p == ".." for p in parts):
        raise UnsafePath(errno.EINVAL, f"not a confined relative path: {rel!r}")
    return parts


@contextmanager
def _dirfd(root: Path) -> Iterator[int]:
    fd = os.open(str(root), DIR_FLAGS)
    try:
        yield fd
    finally:
        os.close(fd)


def _walk_dirs(rootfd: int, parts: list[str], create: bool = False, replace: bool = False) -> int:
    """An fd for the directory ``parts`` under ``rootfd``, never through a symlink.

    ``create`` makes missing directories; ``replace`` also removes a non-directory
    (a symlink, a file, a FIFO) standing where a directory belongs -- the trusted
    layout wins. Without either, a missing or wrong component raises.
    """
    fd = os.dup(rootfd)
    try:
        for name in parts:
            try:
                nfd = os.open(name, DIR_FLAGS, dir_fd=fd)
            except OSError as e:
                if e.errno not in (errno.ENOENT, errno.ELOOP, errno.ENOTDIR) or not (create or replace):
                    raise UnsafePath(e.errno, f"unsafe path component {name!r}") from e
                if e.errno != errno.ENOENT:
                    if not replace:
                        raise UnsafePath(e.errno, f"unsafe path component {name!r}") from e
                    _remove_entry(fd, name)
                os.mkdir(name, 0o755, dir_fd=fd)
                nfd = os.open(name, DIR_FLAGS, dir_fd=fd)
            os.close(fd)
            fd = nfd
        return fd
    except BaseException:
        os.close(fd)
        raise


def _remove_entry(dirfd: int, name: str) -> None:
    """Remove ``name`` in ``dirfd`` whatever it is, never following a link."""
    try:
        st = os.stat(name, dir_fd=dirfd, follow_symlinks=False)
    except FileNotFoundError:
        return
    if stat.S_ISDIR(st.st_mode):
        shutil.rmtree(name, dir_fd=dirfd)
    else:
        os.unlink(name, dir_fd=dirfd)


# -- reads -----------------------------------------------------------------------------


def open_regular(root: Path, rel: str) -> int:
    """A read fd for the regular file ``rel`` under ``root``; raises UnsafePath otherwise."""
    parts = _parts(rel)
    with _dirfd(root) as rfd:
        dfd = _walk_dirs(rfd, parts[:-1])
        try:
            try:
                fd = os.open(parts[-1], FILE_FLAGS, dir_fd=dfd)
            except OSError as e:
                raise UnsafePath(e.errno, f"cannot open {rel!r} safely") from e
            st = os.fstat(fd)
            if not stat.S_ISREG(st.st_mode):
                os.close(fd)
                raise UnsafePath(errno.EINVAL, f"{rel!r} is not a regular file")
            return fd
        finally:
            os.close(dfd)


def read_bytes(root: Path, rel: str, limit: int = READ_LIMIT) -> bytes:
    fd = open_regular(root, rel)
    try:
        chunks, n = [], 0
        while n < limit:
            b = os.read(fd, min(1 << 20, limit - n))
            if not b:
                break
            chunks.append(b)
            n += len(b)
        return b"".join(chunks)
    finally:
        os.close(fd)


def read_text(root: Path, rel: str, limit: int = READ_LIMIT) -> str | None:
    """The file's text, or None when it is missing, not regular, or behind a link."""
    try:
        return read_bytes(root, rel, limit).decode("utf-8", errors="replace")
    except OSError:
        return None


# -- walking -----------------------------------------------------------------------------


@dataclass(frozen=True)
class Entry:
    rel: str
    kind: str  # "file", "link", "dir", or "fifo" / "socket" / "char" / "block" / "other"
    mode: int
    size: int
    mtime: float


def _kind(mode: int) -> str:
    if stat.S_ISREG(mode):
        return "file"
    if stat.S_ISLNK(mode):
        return "link"
    if stat.S_ISDIR(mode):
        return "dir"
    if stat.S_ISFIFO(mode):
        return "fifo"
    if stat.S_ISSOCK(mode):
        return "socket"
    if stat.S_ISCHR(mode):
        return "char"
    if stat.S_ISBLK(mode):
        return "block"
    return "other"


def walk(root: Path, skip_dir=None, limit: int = WALK_LIMIT) -> Iterator[Entry]:
    """Every entry under ``root`` by ``lstat``, never descending through a symlink.

    ``skip_dir(rel)`` prunes directories (the entry is not yielded either). Stops
    after ``limit`` entries (raises).
    """
    root = Path(root)
    n = 0
    for dirpath, dirnames, filenames, dfd in os.fwalk(root, follow_symlinks=False):
        rel_dir = os.path.relpath(dirpath, root)
        rel_dir = "" if rel_dir == "." else rel_dir.replace(os.sep, "/")
        keep = []
        for name in dirnames + filenames:
            rel = f"{rel_dir}/{name}" if rel_dir else name
            try:
                st = os.stat(name, dir_fd=dfd, follow_symlinks=False)
            except OSError:
                continue
            kind = _kind(st.st_mode)
            if kind == "dir":
                if skip_dir is not None and skip_dir(rel):
                    continue
                keep.append(name)
            n += 1
            if n > limit:
                raise OSError(errno.EFBIG, f"more than {limit} entries under {root}")
            yield Entry(rel, kind, st.st_mode, st.st_size, st.st_mtime)
        dirnames[:] = [d for d in dirnames if d in keep]


def sha256_regular(root: Path, rel: str, limit: int = FILE_LIMIT) -> str | None:
    """sha256 of a whole regular file; None if unsafe or over ``limit`` (never a
    partial fingerprint)."""
    try:
        fd = open_regular(root, rel)
    except OSError:
        return None
    try:
        if os.fstat(fd).st_size > limit:
            return None
        h = hashlib.sha256()
        n = 0
        while True:
            b = os.read(fd, 1 << 20)
            if not b:
                break
            n += len(b)
            if n > limit:
                return None
            h.update(b)
        return h.hexdigest()
    finally:
        os.close(fd)


def tail_text(path: Path, n: int) -> str:
    """The last ``n`` bytes of a host-written file, by seeking (never the whole file)."""
    try:
        with open(path, "rb") as f:
            size = f.seek(0, os.SEEK_END)
            f.seek(max(0, size - n))
            return f.read(n).decode("utf-8", errors="replace")
    except OSError:
        return ""


def tree_stats(root: Path, file_limit: int = FILE_LIMIT) -> dict:
    """Apparent bytes of regular files under ``root`` (lstat), and those over the limit."""
    total, over = 0, []
    for e in walk(root):
        if e.kind == "file":
            total += e.size
            if e.size > file_limit:
                over.append({"path": e.rel, "bytes": e.size})
    return {"bytes": total, "over_file_limit": over[:20]}


def check_limits(root: Path, file_limit: int = FILE_LIMIT, tree_limit: int = TREE_LIMIT) -> None:
    st = tree_stats(root, file_limit)
    if st["over_file_limit"]:
        raise Oversize(errno.EFBIG, f"files over {file_limit} bytes: {st['over_file_limit']}")
    if st["bytes"] > tree_limit:
        raise Oversize(errno.EFBIG, f"tree of {st['bytes']} bytes is over {tree_limit}")


def readlink(root: Path, rel: str) -> str | None:
    parts = _parts(rel)
    try:
        with _dirfd(root) as rfd:
            dfd = _walk_dirs(rfd, parts[:-1])
            try:
                return os.readlink(parts[-1], dir_fd=dfd)
            finally:
                os.close(dfd)
    except OSError:
        return None


# -- writes ---------------------------------------------------------------------------------


def write_bytes(root: Path, rel: str, data: bytes, mode: int = 0o644) -> None:
    """Put ``data`` at ``rel`` under ``root``, replacing whatever is there.

    Intermediate components that are not real directories (a planted symlink, a
    file) are removed and recreated as directories; the destination entry is
    unlinked first and the file created with O_EXCL|O_NOFOLLOW.
    """
    parts = _parts(rel)
    with _dirfd(root) as rfd:
        dfd = _walk_dirs(rfd, parts[:-1], create=True, replace=True)
        try:
            _remove_entry(dfd, parts[-1])
            fd = os.open(parts[-1], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, mode, dir_fd=dfd)
            with os.fdopen(fd, "wb") as f:
                f.write(data)
        finally:
            os.close(dfd)


def remove(root: Path, rel: str) -> bool:
    """Remove ``rel`` under ``root`` (a file, a link or a whole tree) without ever
    following a link. A symlinked or missing parent means there is nothing of ours
    to remove: returns False."""
    parts = _parts(rel)
    try:
        with _dirfd(root) as rfd:
            dfd = _walk_dirs(rfd, parts[:-1])
    except OSError:
        return False
    try:
        try:
            os.stat(parts[-1], dir_fd=dfd, follow_symlinks=False)
        except FileNotFoundError:
            return False
        _remove_entry(dfd, parts[-1])
        return True
    finally:
        os.close(dfd)


def tree_size(root: Path, rel: str) -> int:
    """Bytes of regular files under ``rel`` (lstat, no links followed); 0 if unsafe."""
    parts = _parts(rel)
    try:
        with _dirfd(root) as rfd:
            dfd = _walk_dirs(rfd, parts[:-1])
            try:
                st = os.stat(parts[-1], dir_fd=dfd, follow_symlinks=False)
            finally:
                os.close(dfd)
    except OSError:
        return 0
    if stat.S_ISREG(st.st_mode):
        return st.st_size
    if not stat.S_ISDIR(st.st_mode):
        return 0
    base = Path(root)
    for p in parts:
        base = base / p
    return sum(e.size for e in walk(base) if e.kind == "file")


def copy_tree(src: Path, dst: Path, file_limit: int = FILE_LIMIT, tree_limit: int = TREE_LIMIT) -> dict:
    """Copy ``src`` to a new ``dst``: directories, regular files (bytes and mode) and
    symlinks (as links, never followed); FIFOs, sockets and devices are skipped and
    counted. Reads go through :func:`open_regular`. A file over ``file_limit`` or a
    tree over ``tree_limit`` (apparent sizes, checked before anything is copied, and
    again while copying) raises :class:`Oversize`."""
    check_limits(src, file_limit, tree_limit)
    dst = Path(dst)
    dst.mkdir(parents=True, exist_ok=False)
    skipped: list[str] = []
    copied = 0
    for e in walk(src):
        target = dst / e.rel
        if e.kind == "dir":
            target.mkdir(exist_ok=True)
        elif e.kind == "file":
            fd = open_regular(src, e.rel)
            try:
                with os.fdopen(os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as out:
                    n = 0
                    while True:
                        b = os.read(fd, 1 << 20)
                        if not b:
                            break
                        n += len(b)
                        copied += len(b)
                        if n > file_limit or copied > tree_limit:
                            raise Oversize(errno.EFBIG, f"{e.rel} grew past the copy limits")
                        out.write(b)
                    os.fchmod(out.fileno(), stat.S_IMODE(e.mode) & 0o777)
            finally:
                os.close(fd)
        elif e.kind == "link":
            link = readlink(src, e.rel)
            if link is not None:
                os.symlink(link, target)
        else:
            skipped.append(e.rel)
    return {"skipped_special": skipped}
