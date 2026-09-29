"""Workspaces: the craze template, fixture materialisation, manifests (plan 029 §3.1.5).

Every workspace has an independent object store with no remote and no alternates.
craze's template is ``git init`` + ``git fetch --no-tags <repo> <commit>`` (only that
commit's ancestry), where the commit is the task's own (``[repo] commit``, default
3eabb31); templates are cached per commit. A commit whose history ever held the eval
itself (``eval/``: every task's hidden tests and reference answers) -- in its own tree
or any ancestor's -- is never materialised with it: its template is a *fresh* object
store holding one parentless commit of the tree without ``eval/``, and no history. A craze
task with a ``setup.patch`` likewise gets a fresh object store holding one parentless
commit of the patched tree (plan 029 X7, review r2-c1 item 17): no history, so nothing in
git shows what was planted or what it replaced. Fixtures are copied and committed with a
fixed author and date.
Host-side git runs only on trusted repositories (templates, fresh materialisations, the
craze repository they are made from) and never reads the owner's git configuration; git
on an agent's repository runs in a sandbox (gitpost.py).
"""

from __future__ import annotations

import fcntl
import fnmatch
import hashlib
import io
import json
import os
import shutil
import stat
import subprocess
import tarfile
import tomllib
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from crazeeval import paths, safefs
from crazeeval.tasks import Task

FIXTURE_NAME = "Eval Fixture"
FIXTURE_EMAIL = "fixture@craze.invalid"
FIXTURE_DATE = "2026-01-01T12:00:00+00:00"
IMPORT_MESSAGE = "Import workspace"

# Generated artefacts never count as writes (plan 029 §3.1.5).
GENERATED_IGNORES = ["__pycache__/", "*.pyc", ".pytest_cache/", "*.test"]

# The craze repository's own .gitignore at the template commit (paths.CRAZE_TEMPLATE_COMMIT),
# one entry per line, comments and blank lines dropped (plan 029 X15). It stays this
# hard-coded list: tests/test_workspace_checks.py checks that the .gitignore at every commit a
# task under eval/tasks/ uses parses to exactly it, so a task pinned to a commit whose
# .gitignore differs fails that test until this list is updated. A craze-repo task's
# own ignored build outputs -- bin/craze from `make build`, tests/cli/.venv from `make
# test-cli` -- are what verifying leaves behind, and git (so the run's diff.patch) never
# shows them; they are no change. Each line already means here what it means to git: a
# leading "/" anchors at the root, a "/" inside a pattern matches from the root, a bare
# name matches at any depth. This list is kept as the file's own text -- git's extra rule
# that a pattern without a trailing "/" also matches a directory of that name (and
# everything under it) is layered on at the one place these patterns are added
# (ignores_for, via _dir_and_file_forms), not baked into the list itself (review r1-c11/c12
# P3). The file has no "!" negations, which this language does not support.
CRAZE_REPO_IGNORES = [
    "/bin/",
    "/scratch/",
    "/dist/",
    ".idea/",
    ".vscode/",
    "*.swp",
    "*.swo",
    "coverage.out",
    "*.test",
    "tests/cli/.venv/",
    "__pycache__/",
    "*.pyc",
    ".DS_Store",
    ".venv/",
    "site-build/",
    "/smoke-captures/",
    "/.cache/",
    ".claude/settings.local.json",
    ".claude/projects/",
    ".claude/worktrees/",
]


class WorkspaceError(RuntimeError):
    pass


def git_env(date: str | None = None) -> dict[str, str]:
    env = {
        "PATH": "/usr/bin:/bin",
        "HOME": str(paths.CACHE_DIR),
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_AUTHOR_NAME": FIXTURE_NAME,
        "GIT_AUTHOR_EMAIL": FIXTURE_EMAIL,
        "GIT_COMMITTER_NAME": FIXTURE_NAME,
        "GIT_COMMITTER_EMAIL": FIXTURE_EMAIL,
        "LANG": "C.UTF-8",
    }
    if date:
        env["GIT_AUTHOR_DATE"] = date
        env["GIT_COMMITTER_DATE"] = date
    return env


def git(cwd: Path, *args: str, date: str | None = None, check: bool = True, input: bytes | None = None) -> str:
    r = subprocess.run(
        ["git", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", *args],
        cwd=cwd,
        env=git_env(date),
        capture_output=True,
        input=input,
    )
    if check and r.returncode != 0:
        raise WorkspaceError(f"git {' '.join(args[:3])} failed: {r.stderr.decode(errors='replace').strip()[:400]}")
    return r.stdout.decode(errors="replace")


def _locked(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True)
    fh = open(path, "w")
    fcntl.flock(fh.fileno(), fcntl.LOCK_EX)
    return fh


# -- the craze template -----------------------------------------------------------


def verify_craze_repo(ws: Path, base: str, source_repo: Path | None = None) -> dict:
    """The isolation checks: only HEAD's branch, nothing after ``base``, and newer
    source heads absent from the object store."""
    refs = [r for r in git(ws, "for-each-ref", "--format=%(refname)").split() if r]
    head_ref = git(ws, "symbolic-ref", "-q", "HEAD", check=False).strip()
    after = [c for c in git(ws, "rev-list", "--all", f"^{base}").split() if c]
    expect_after = 0
    newer: list[str] = []
    present: list[str] = []
    if source_repo is not None:
        # Each distinct source head once (branches and tags often share a commit).
        for c in dict.fromkeys(git(source_repo, "for-each-ref", "--format=%(objectname)").split()):
            r = subprocess.run(
                ["git", "merge-base", "--is-ancestor", c, base], cwd=source_repo, env=git_env(), capture_output=True
            )
            if r.returncode == 1:
                newer.append(c)
        newer = newer[:200]
        for c in newer:
            ok = subprocess.run(["git", "cat-file", "-e", f"{c}^{{commit}}"], cwd=ws, env=git_env(), capture_output=True)
            if ok.returncode == 0:
                present.append(c)
    remotes = [r for r in git(ws, "remote").split() if r]
    alternates = (ws / ".git/objects/info/alternates").exists()
    result = {
        "refs": refs,
        "head_ref": head_ref,
        "commits_after_base": len(after),
        "newer_source_commits_checked": len(newer),
        "newer_source_commits_present": present,
        "remotes": remotes,
        "alternates": alternates,
    }
    result["ok"] = (
        refs == [head_ref]
        and len(after) == expect_after
        and not present
        and not remotes
        and not alternates
    )
    return result


def _build_once(dest: Path, build: Callable[[Path], dict]) -> Path:
    """``dest``, built under a lock unless a ready one exists. ``build`` fills a scratch
    tree and returns its isolation check, which must be ok; the tree then replaces
    ``dest`` and the check is kept in the ready marker."""
    lock = _locked(dest.parent / f".{dest.name}.lock")
    try:
        if _is_ready(dest):
            return dest
        tmp = dest.parent / f"{dest.name}.building"
        shutil.rmtree(tmp, ignore_errors=True)
        check = build(tmp)
        shutil.rmtree(dest, ignore_errors=True)
        os.replace(tmp, dest)
        _mark_ready(dest, check)
        return dest
    finally:
        lock.close()


# Top-level paths of the craze repository a workspace never holds: the eval itself (every
# task's hidden tests, trusted copies and reference answers). A commit with one anywhere
# in its history -- its own tree, or any ancestor's, even if a later commit removed it --
# gets a history-free template without it (hidden_paths, _orphan_template).
HIDDEN_CRAZE_PATHS = ("eval",)


def hidden_paths(commit: str, source_repo: Path | None = None) -> list[str]:
    """The HIDDEN_CRAZE_PATHS that ``commit`` or any of its ancestors ever held: some
    commit in its history touched the path (``git rev-list --full-history``, so a side
    branch that added and removed it before a merge counts too). None for 3eabb31, whose
    whole history predates eval/."""
    source_repo = Path(source_repo or paths.REPO_ROOT)
    git(source_repo, "cat-file", "-e", f"{commit}^{{commit}}")  # a missing commit fails loudly here
    return [h for h in HIDDEN_CRAZE_PATHS
            if git(source_repo, "rev-list", "-1", "--full-history", commit, "--", h).strip()]


def hidden_absent(ws: Path, hidden: list[str]) -> bool:
    """None of ``hidden`` is in the workspace's files or in its HEAD tree."""
    return all(not os.path.lexists(ws / h) and not git(ws, "ls-tree", "--name-only", "HEAD", "--", h, check=False).strip()
               for h in hidden)


def craze_template(
    commit: str = paths.CRAZE_TEMPLATE_COMMIT,
    source_repo: Path | None = None,
    cache: Path | None = None,
) -> Path:
    """The cached template for a craze commit (``<cache>/templates/craze-<sha12>``):
    the commit and its ancestry -- or, for a commit whose history ever held ``eval/``,
    one parentless commit of its tree without it (``craze-<sha12>-noeval``). The history
    is checked on every call, so no template built with history is used for such a
    commit."""
    source_repo = Path(source_repo or paths.REPO_ROOT)
    root = Path(cache or paths.CACHE_DIR) / "templates"
    hidden = hidden_paths(commit, source_repo)
    if hidden:
        return _orphan_template(commit, hidden, source_repo, root / f"craze-{commit[:12]}-no{'-'.join(hidden)}")

    def build(tmp: Path) -> dict:
        tmp.mkdir(parents=True)
        git(tmp, "init", "-q", "-b", "main")
        git(tmp, "fetch", "-q", "--no-tags", str(source_repo), commit)
        git(tmp, "update-ref", "refs/heads/main", commit)
        git(tmp, "reset", "-q", "--hard", "main")
        for leftover in ("FETCH_HEAD", "ORIG_HEAD"):
            (tmp / ".git" / leftover).unlink(missing_ok=True)
        git(tmp, "reflog", "expire", "--expire=now", "--all")
        check = verify_craze_repo(tmp, commit, source_repo)
        if not check["ok"]:
            raise WorkspaceError(f"craze template failed its isolation checks: {check}")
        return check

    return _build_once(root / f"craze-{commit[:12]}", build)


def _orphan_template(commit: str, hidden: list[str], source_repo: Path, dest: Path) -> Path:
    """``commit``'s tree without ``hidden``, as one parentless commit in a fresh object
    store: neither the hidden paths nor any history (which holds them) is readable."""

    def build(tmp: Path) -> dict:
        tmp.mkdir(parents=True)
        tar = subprocess.run(["git", "archive", "--format=tar", commit], cwd=source_repo, env=git_env(),
                             capture_output=True, check=True).stdout
        with tarfile.open(fileobj=io.BytesIO(tar)) as tf:
            tf.extractall(tmp, filter="data")
        for h in hidden:  # in the tree still, or only in the history
            p = tmp / h
            if p.is_dir() and not p.is_symlink():
                shutil.rmtree(p)
            elif os.path.lexists(p):
                p.unlink()
        git(tmp, "init", "-q", "-b", "main")
        # Every extracted file was tracked at the commit: add it even where a
        # .gitignore pattern matches it.
        git(tmp, "add", "-A", "-f")
        git(tmp, "commit", "-q", "-m", IMPORT_MESSAGE, date=FIXTURE_DATE)
        git(tmp, "reflog", "expire", "--expire=now", "--all")
        if git(tmp, "status", "--porcelain").strip():
            raise WorkspaceError(f"craze {commit[:12]}: the import commit left a dirty tree")
        check = verify_orphan_repo(tmp, commit, source_repo)
        check["hidden"] = hidden
        check["hidden_absent"] = hidden_absent(tmp, hidden)
        check["ok"] = check["ok"] and check["hidden_absent"]
        if not check["ok"]:
            raise WorkspaceError(f"craze {commit[:12]}: the template failed its isolation checks: {check}")
        return check

    return _build_once(dest, build)


def _mark_ready(dest: Path, check: dict | None = None) -> None:
    # The ready marker sits beside the tree, not in it, so no manifest ever sees it.
    marker = dest.parent / f"{dest.name}.ready"
    marker.write_text(json.dumps(check or {"ok": True}, sort_keys=True) + "\n")


def _is_ready(dest: Path) -> bool:
    return (dest.parent / f"{dest.name}.ready").exists() and dest.is_dir()


def template_check(dest: Path) -> dict | None:
    """The isolation check recorded when a template was built."""
    try:
        return json.loads((dest.parent / f"{dest.name}.ready").read_text())
    except (OSError, ValueError):
        return None


def verify_orphan_repo(ws: Path, base: str, base_repo: Path) -> dict:
    """A planted task's isolation: one parentless commit on HEAD's branch, every object
    in the store reachable from it, and ``base`` and its whole history absent."""
    refs = [r for r in git(ws, "for-each-ref", "--format=%(refname)").split() if r]
    head_ref = git(ws, "symbolic-ref", "-q", "HEAD", check=False).strip()
    commits = [c for c in git(ws, "rev-list", "--all").split() if c]
    roots = [c for c in git(ws, "rev-list", "--max-parents=0", "--all").split() if c]
    reachable = len([x for x in git(ws, "rev-list", "--objects", "--all").splitlines() if x.strip()])
    counts = dict(line.split(": ", 1) for line in git(ws, "count-objects", "-v").splitlines() if ": " in line)
    stored = int(counts.get("count", 0)) + int(counts.get("in-pack", 0))
    history = git(base_repo, "rev-list", base).split()
    batch = git(ws, "cat-file", "--batch-check", input="\n".join(history).encode() + b"\n")
    present = [line.split()[0] for line in batch.splitlines() if line.strip() and not line.endswith(" missing")]
    result = {
        "refs": refs,
        "head_ref": head_ref,
        "commits": len(commits),
        "parentless": commits == roots,
        "objects_stored": stored,
        "objects_reachable": reachable,
        "history_commits_checked": len(history),
        "history_commits_present": present[:10],
        "remotes": [r for r in git(ws, "remote").split() if r],
        "alternates": (ws / ".git/objects/info/alternates").exists(),
    }
    result["ok"] = (
        refs == [head_ref] and len(commits) == 1 and result["parentless"] and stored == reachable
        and not present and not result["remotes"] and not result["alternates"]
    )
    return result


def template_hidden(base: Path) -> list[str]:
    """The paths a template was built without (a history-free ``eval/``-less template);
    [] for a template with its commit's history."""
    return list((template_check(base) or {}).get("hidden") or [])


def _history_repo(base: Path, source_repo: Path | None) -> Path:
    """Where to list a commit's ancestry for an isolation check: the template itself
    when it holds that history, else the craze repository it was made from."""
    return Path(source_repo or paths.REPO_ROOT) if template_hidden(base) else base


def craze_task_template(task: Task, cache: Path | None = None, source_repo: Path | None = None) -> Path:
    commit = task.craze_commit
    base = craze_template(commit, cache=cache, source_repo=source_repo)
    if task.setup_patch is None:
        return base
    patch = task.setup_patch.read_bytes()
    h = hashlib.sha256(patch).hexdigest()[:12]
    hidden = template_hidden(base)

    def build(tmp: Path) -> dict:
        # The base tree exactly as committed -- none of its history -- into a fresh
        # repository, the plant applied, one parentless commit (plan 029 X7). The
        # base's HEAD is the task's commit (or, for an eval/-less template, its one
        # commit of that tree).
        tmp.mkdir(parents=True)
        tar = subprocess.run(["git", "archive", "--format=tar", "HEAD"], cwd=base, env=git_env(),
                             capture_output=True, check=True).stdout
        with tarfile.open(fileobj=io.BytesIO(tar)) as tf:
            tf.extractall(tmp, filter="data")
        git(tmp, "init", "-q", "-b", "main")
        git(tmp, "apply", "--whitespace=nowarn", "-", input=patch)
        git(tmp, "add", "-A")
        git(tmp, "commit", "-q", "-m", IMPORT_MESSAGE, date=FIXTURE_DATE)
        git(tmp, "reflog", "expire", "--expire=now", "--all")
        if git(tmp, "status", "--porcelain").strip():
            raise WorkspaceError(f"{task.id}: import commit left a dirty tree")
        check = verify_orphan_repo(tmp, commit, _history_repo(base, source_repo))
        if hidden:
            check["hidden"] = hidden
            check["hidden_absent"] = hidden_absent(tmp, hidden)
            check["ok"] = check["ok"] and check["hidden_absent"]
        if not check["ok"]:
            raise WorkspaceError(f"{task.id}: task template failed its isolation checks: {check}")
        return check

    return _build_once(base.parent / f"{base.name}-{task.id}-{h}-orphan", build)


# -- fixtures -----------------------------------------------------------------------


def fixture_dir(name: str) -> Path:
    return paths.FIXTURES_DIR / name


def fixture_hash(name: str) -> str:
    """sha256 over a fixture's files (for the provenance manifest)."""
    root = fixture_dir(name)
    h = hashlib.sha256()
    for p in sorted(root.rglob("*")):
        if p.is_file():
            h.update(p.relative_to(root).as_posix().encode() + b"\0")
            h.update(p.read_bytes())
    return h.hexdigest()


def materialise_fixture(name: str, dest: Path) -> None:
    src = fixture_dir(name)
    meta = {}
    if (src / "fixture.toml").exists():
        with open(src / "fixture.toml", "rb") as f:
            meta = tomllib.load(f)
    files = src / "files"
    if not files.is_dir():
        raise WorkspaceError(f"fixture {name}: no files/ directory")
    shutil.copytree(files, dest, symlinks=True, dirs_exist_ok=False)
    git(dest, "init", "-q", "-b", "main")
    git(dest, "add", "-A")
    git(dest, "commit", "-q", "-m", meta.get("initial_message", "Initial commit"), date=meta.get("initial_date", FIXTURE_DATE))
    for step in meta.get("history") or []:
        patch = (src / step["patch"]).read_bytes()
        git(dest, "apply", "--whitespace=nowarn", "-", input=patch)
        git(dest, "add", "-A")
        git(dest, "commit", "-q", "-m", step["message"], date=step.get("date", FIXTURE_DATE))
        if step.get("tag"):
            git(dest, "tag", step["tag"])
    git(dest, "reflog", "expire", "--expire=now", "--all")


# -- materialisation -------------------------------------------------------------------


@dataclass
class Start:
    commit: str
    manifest: dict[str, str]
    isolation: dict | None


def materialise(task: Task, dest: Path, cache: Path | None = None) -> Start:
    dest = Path(dest)
    if dest.exists():
        raise WorkspaceError(f"{dest} already exists")
    isolation = None
    if task.repo_kind == "craze":
        commit = task.craze_commit
        tmpl = craze_task_template(task, cache=cache)
        shutil.copytree(tmpl, dest, symlinks=True)
        base = craze_template(commit, cache=cache)
        hidden = template_hidden(base)
        if task.setup_patch is not None or hidden:
            isolation = verify_orphan_repo(dest, commit, _history_repo(base, None))
        else:
            isolation = verify_craze_repo(dest, commit, None)
        if hidden:
            isolation["hidden"] = hidden
            isolation["hidden_absent"] = hidden_absent(dest, hidden)
            isolation["ok"] = isolation["ok"] and isolation["hidden_absent"]
        isolation["craze_commit"] = commit
        if not isolation["ok"]:
            raise WorkspaceError(f"{task.id}: workspace failed its isolation checks")
    else:
        materialise_fixture(task.fixture, dest)
    commit = git(dest, "rev-parse", "HEAD").strip()
    return Start(commit=commit, manifest=build_manifest(dest), isolation=isolation)


def _dir_and_file_forms(patterns: list[str]) -> list[str]:
    """Git's directory semantics for a bare pattern (review r1-c11/c12 P3): a pattern
    with no trailing "/" -- "coverage.out", "*.swp", ".DS_Store" -- matches a *file* of
    that name here (:func:`is_ignored`), but in git it also matches a *directory* of that
    name, and so everything under it. Each such pattern is added a second time with a
    trailing "/", so :func:`dir_ignored` catches the directory case too; a pattern that
    already ends in "/" is directory-only already and is passed through once. This is
    only for a caller that wants git's fuller meaning (``CRAZE_REPO_IGNORES``, whose
    patterns are real gitignore lines) -- ``GENERATED_IGNORES``, a task's own ``ignore``
    list and a runner's ``workspace_state`` keep meaning exactly what this module's
    pattern language says (file-only unless written with a trailing "/")."""
    out = []
    for p in patterns:
        out.append(p)
        if not p.endswith("/"):
            out.append(p + "/")
    return out


def ignores_for(task: Task | None, extra: list[str] | None = None) -> list[str]:
    out = list(GENERATED_IGNORES)
    if task is not None:
        if task.repo_kind == "craze":
            # The task repository's own ignored build outputs (X15), with git's directory
            # semantics (P3): a bare pattern also hides a directory of that name.
            out += _dir_and_file_forms(CRAZE_REPO_IGNORES)
        out += task.ignore
    out += extra or []
    return out


# -- manifests ---------------------------------------------------------------------------


def _anchored(rel: str, pat: str, prefix: bool) -> bool:
    """``rel`` against a root-anchored pattern (its leading "/" removed), one path
    component per pattern component, so "*" never crosses a "/" (as in gitignore). With
    ``prefix``, ``rel`` may continue below the match (a directory's contents)."""
    want, got = pat.split("/"), rel.split("/")
    if len(got) < len(want) or (not prefix and len(got) != len(want)):
        return False
    return all(fnmatch.fnmatch(g, w) for g, w in zip(got, want))


def dir_ignored(rel_dir: str, patterns: list[str]) -> bool:
    """A directory pattern ends in "/": a bare name matches that directory anywhere,
    a path with "/" matches from the workspace root, and a leading "/" anchors it at the
    root as in gitignore ("/bin/" is the root's bin directory, never internal/bin)."""
    base = rel_dir.rsplit("/", 1)[-1]
    for pat in patterns:
        if not pat.endswith("/"):
            continue
        name = pat.rstrip("/")
        if name.startswith("/"):
            if _anchored(rel_dir, name[1:], prefix=True):
                return True
        elif "/" in name:
            if rel_dir == name or rel_dir.startswith(name + "/") or fnmatch.fnmatch(rel_dir, name):
                return True
        elif fnmatch.fnmatch(base, name):
            return True
    return False


def is_ignored(rel: str, patterns: list[str]) -> bool:
    """A file pattern (no trailing "/"): a bare name matches the file's name at any
    depth, a path with "/" is matched against the whole path, and a leading "/" anchors
    it at the root ("/x.txt" is the root's x.txt only)."""
    parts = rel.split("/")
    for i in range(1, len(parts)):
        if dir_ignored("/".join(parts[:i]), patterns):
            return True
    for pat in patterns:
        if pat.endswith("/"):
            continue
        if pat.startswith("/"):
            if _anchored(rel, pat[1:], prefix=False):
                return True
        elif "/" in pat:
            if fnmatch.fnmatch(rel, pat):
                return True
        elif fnmatch.fnmatch(parts[-1], pat):
            return True
    return False


def build_manifest(root: Path) -> dict[str, str]:
    """Every entry under ``root`` except ``.git/`` directories, classified by ``lstat``
    and never followed (review r1-c1 findings 15, 16):

    - ``f:<mode>:<sha256>`` -- a regular file with its full permission bits (``0644``;
      any mode change is a change);
    - ``o:<mode>:<bytes>`` -- a regular file over ``safefs.FILE_LIMIT``: never
      fingerprinted from a part, and it fails the run as oversize;
    - ``l:<target>`` -- a symlink;
    - ``s:<kind>`` -- a FIFO, socket or device: recorded, never opened.

    Directories are implied by their contents. Nothing is ignored here: ignore
    patterns apply to additions only (:func:`compare`).
    """
    root = Path(root)
    out: dict[str, str] = {}
    for e in safefs.walk(root, skip_dir=lambda rel: rel.rsplit("/", 1)[-1] == ".git"):
        if e.kind == "dir":
            continue
        if e.kind == "file":
            mode = f"{stat.S_IMODE(e.mode):04o}"
            if e.size > safefs.FILE_LIMIT:
                out[e.rel] = f"o:{mode}:{e.size}"
                continue
            h = safefs.sha256_regular(root, e.rel)
            out[e.rel] = f"f:{mode}:{h}" if h else f"o:{mode}:{e.size}"
        elif e.kind == "link":
            out[e.rel] = "l:" + (safefs.readlink(root, e.rel) or "?")
        else:
            out[e.rel] = f"s:{e.kind}"
    return dict(sorted(out.items()))


def oversize(manifest: dict[str, str]) -> list[str]:
    """Paths the manifest could not fingerprint because they are over the limit."""
    return [p for p, v in manifest.items() if v.startswith("o:")]


def compare(start: dict[str, str], final: dict[str, str], ignores: list[str] | None = None) -> dict[str, list[str]]:
    """Changes from ``start`` to ``final``. Every path present at the start is always
    compared (a tracked file matching an ignore pattern still counts); the ignores --
    generated files, harness state, a craze-repo task's own .gitignore -- apply only to
    additions."""
    ignores = ignores or []
    added = sorted(p for p in set(final) - set(start) if not is_ignored(p, ignores))
    deleted = sorted(set(start) - set(final))
    modified = sorted(p for p in set(start) & set(final) if start[p] != final[p])
    return {"added": added, "modified": modified, "deleted": deleted}


def sha256_file(p: Path) -> str | None:
    """sha256 of a trusted host file (a binary under test, a toolchain)."""
    try:
        h = hashlib.sha256()
        with open(p, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                h.update(chunk)
        return h.hexdigest()
    except OSError:
        return None


def changed_paths(diff: dict[str, list[str]]) -> list[str]:
    return sorted(set(diff["added"]) | set(diff["modified"]) | set(diff["deleted"]))
