"""Post-run git on an agent's repository, sandboxed (review r1-c1 finding 2).

The agent controls its workspace's ``.git/config`` and ``.gitattributes``: a clean
filter, an fsmonitor hook or a textconv driver there would run on the host under any
host-side ``git add``/``git diff``. So no git command ever touches an agent's
repository on the host. Everything runs in one bwrap sandbox with no network, no
/home, the workspace bound read-only and a writable output directory:

- **the diff** uses a fresh repository of our own in the sandbox's /tmp (the agent's
  ``.git/config``, hooks and ``info/attributes`` are never read), borrowing the start
  commit from the *trusted* pristine materialisation's objects, with the worktree's
  ``.gitattributes`` ignored (``GIT_ATTR_SOURCE`` = the empty tree) and no textconv,
  external diff or fsmonitor;
- **agent commits** are read from the agent's own repository with the same
  overrides, read-only (``rev-parse`` and a tree-to-tree ``diff --stat``).

Even a filter that did run would find nothing to reach: no network, no /home, a
read-only workspace.
"""

from __future__ import annotations

import re
from pathlib import Path

from crazeeval import safefs
from crazeeval.sandbox import SandboxSpec, Toolchains, run_sandboxed

EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
# The one case ``--ignore-errors`` exists for: a nested repository with no commit
# checked out. git leaves this on stderr, one line per skipped path, and still exits
# non-zero (review r3-c1 item J). Any other line means a real staging failure.
EMBEDDED_REPO_RE = re.compile(r"^error: '.*' does not have a commit checked out$")


def add_errors_benign(add_errors: str) -> bool:
    """True only when every line of the post-run ``git add``'s stderr is the known,
    accepted embedded-repository case. Empty/no stderr with a non-zero ``add_rc`` is
    not this case, so it is not waved through."""
    lines = [ln for ln in (add_errors or "").splitlines() if ln.strip()]
    return bool(lines) and all(EMBEDDED_REPO_RE.match(ln) for ln in lines)
DIFF_LIMIT = 2_000_000
OBJECTS_INSIDE = "/sandbox/trusted-objects"
OUT_INSIDE = "/sandbox/out"
# Generated artefacts left out of the diff (the manifest's generated ignores).
DIFF_EXCLUDES = [":(exclude,glob)**/__pycache__/**", ":(exclude,glob)**/*.pyc", ":(exclude,glob)**/.pytest_cache/**",
                 ":(exclude,glob)**/*.test"]

SCRIPT = r'''
set -u
out=/sandbox/out
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_ATTR_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
  GIT_PAGER=cat GIT_OPTIONAL_LOCKS=0 GIT_NO_REPLACE_OBJECTS=1
G() { git -c core.fsmonitor=false -c core.hooksPath=/dev/null -c core.untrackedCache=false \
  -c core.attributesFile=/dev/null -c diff.external= -c core.pager=cat "$@"; }
start="$1"; ws="$2"; limit="$3"; shift 3

# 1. The diff: our own repository; the start commit from the trusted objects.
t=/tmp/crazeeval-git
git init -q "$t" && echo /sandbox/trusted-objects > "$t/.git/objects/info/alternates"
(
  export GIT_DIR="$t/.git" GIT_WORK_TREE="$ws" GIT_INDEX_FILE="$t/index" \
    GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904
  cd "$ws" || exit 1
  G read-tree "$start" || { echo read-tree > "$out/diff.rc"; exit 1; }
  # A file git cannot index (an embedded repository with no commit, say) is skipped
  # and noted in add.rc; the diff of everything else still stands.
  G add -A -f --ignore-errors -- . "$@" 2> "$out/add.stderr"
  echo $? > "$out/add.rc"
  # Streamed: the first $limit bytes kept, the rest only counted.
  G diff --cached --binary --no-textconv --no-ext-diff "$start" |
    { head -c "$limit" > "$out/diff.patch"; wc -c > "$out/diff.rest"; }
  echo "${PIPESTATUS[0]}" > "$out/diff.rc"
)

# 2. Agent commits: the agent's own repository, read-only.
cd "$ws" || exit 0
G rev-parse -q --verify HEAD > "$out/head.txt" 2>/dev/null
head=$(cat "$out/head.txt" 2>/dev/null)
if [ -n "$head" ] && [ "$head" != "$start" ]; then
  G diff --stat --no-textconv --no-ext-diff "$start" "$head" 2>&1 | head -c 20000 > "$out/tree_diff.txt"
fi
exit 0
'''


async def git_postrun(ws: Path, ws_inside: str, start_commit: str, trusted_repo: Path, out: Path, tc: Toolchains,
                      keyring=None, timeout: int = 300) -> dict:
    """Run the sandboxed post-run git and return ``{diff, diff_bytes, head, moved, tree_diff}``.

    ``trusted_repo`` is a fresh materialisation of the task's start (never the
    agent's), whose objects hold the start commit.
    """
    out.mkdir(parents=True, exist_ok=False)
    spec = SandboxSpec(
        workspace=ws,
        ws_inside=ws_inside,
        home=None,
        network=False,
        ws_readonly=True,
        extra_ro=[(trusted_repo / ".git" / "objects", OBJECTS_INSIDE)],
        extra_rw=[(out, OUT_INSIDE)],
    )
    cmd = ["bash", "-c", SCRIPT, "gitpost", start_commit, ws_inside, str(DIFF_LIMIT), *DIFF_EXCLUDES]
    r = await run_sandboxed(spec, tc, cmd, stdout=out / "stdout", stderr=out / "stderr", timeout=timeout,
                            keyring=keyring, sample_env=False)
    head = (safefs.read_text(out, "head.txt", limit=200) or "").strip()
    diff = safefs.read_text(out, "diff.patch", limit=DIFF_LIMIT) or ""
    try:
        rest = int((safefs.read_text(out, "diff.rest", limit=100) or "0").strip() or 0)
    except ValueError:
        rest = 0
    diff_bytes = len(diff.encode()) + rest
    return {
        "diff": diff,
        "diff_bytes": diff_bytes,
        "diff_rc": (safefs.read_text(out, "diff.rc") or "").strip(),
        "add_rc": (safefs.read_text(out, "add.rc", limit=100) or "").strip(),
        "add_errors": safefs.tail_text(out / "add.stderr", 2000),
        "head": head,
        "moved": bool(head) and head != start_commit,
        "tree_diff": safefs.read_text(out, "tree_diff.txt") or "",
        "exit": r.exit_code,
        "timed_out": r.timed_out,
    }
