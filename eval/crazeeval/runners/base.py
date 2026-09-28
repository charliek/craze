"""The runner contract: how one harness is configured, launched and read back."""

from __future__ import annotations

import fnmatch
import json
from dataclasses import dataclass, field
from pathlib import Path

from crazeeval import safefs
from crazeeval.config import EvalModel, Snapshot
from crazeeval.tasks import Task

STDOUT_LIMIT = 64 * 1024 * 1024


@dataclass
class Extract:
    """What a harness's own output says: the answer, and whether the harness reported
    a successful terminal completion (review r1-c1 finding 17). A run is ``ok`` only
    when ``completed`` is true; an error after partial text is a failure."""

    answer: str
    notes: list[str] = field(default_factory=list)
    extra: dict = field(default_factory=dict)
    completed: bool = False
    failure: str | None = None


class Runner:
    name: str = ""
    # Toolchains bound for this harness beyond the base set (go, the venv, uv).
    tools: set[str] = set()
    # Paths the harness itself writes inside the workspace (manifest ignores),
    # identified by the AC-A2 smoke.
    workspace_state: list[str] = []
    # Download caches in the home, deleted after scoring (never evidence, often large).
    prune_paths: list[str] = []
    supports_plan: bool = True

    def supports(self, em: EvalModel, task: Task) -> bool:
        if task.is_plan and not self.supports_plan:
            return False
        if task.harnesses is not None and self.name not in task.harnesses:
            return False
        return em.supports(self.name)

    def home(self, home: Path, em: EvalModel, snap: Snapshot, base_url: str) -> dict[str, str]:
        raise NotImplementedError

    def command(self, em: EvalModel, task: Task, ws_inside: str, snap: Snapshot) -> list[str]:
        raise NotImplementedError

    def extra_ro(self, snap: Snapshot) -> list[tuple[Path, str]]:
        return []

    def extract(self, stdout: Path, home: Path, task: Task) -> Extract:
        raise NotImplementedError

    def plan_mode(self, task: Task) -> str | None:
        """How plan mode is requested for a plan task (recorded with each result)."""
        return None

    def seed_binds(self, home: Path, seed: Path | None) -> list[tuple[Path, str]]:
        """Read-only binds of pre-seeded, offline packages (opencode)."""
        return []


def read_ndjson(p: Path) -> list[dict]:
    """A harness's stdout (written by the host into the attempt directory, bounded)."""
    out = []
    p = Path(p)
    text = safefs.read_text(p.parent, p.name, limit=STDOUT_LIMIT)
    if text is None:
        return out
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if isinstance(obj, dict):
            out.append(obj)
    return out


def after_last(events: list[dict], kind: str) -> list[dict]:
    """The events after the last one of type ``kind`` (all of them when there is none)."""
    last = max((i for i, e in enumerate(events) if e.get("type") == kind), default=-1)
    return events[last + 1 :]


def lead_with(plan: str, answer: str) -> str:
    """A presented plan leads the answer."""
    return plan.strip() + (("\n\n" + answer) if answer else "")


def newest_text(home: Path, subdir: str, pattern: str) -> str | None:
    """The text of the most recently modified regular file under ``home/subdir`` whose
    name matches ``pattern``. The agent wrote this tree: the walk starts at ``home``,
    descends only along ``subdir``, follows no link and opens no special file."""
    prefix = subdir.strip("/")

    def off_path(rel: str) -> bool:
        return not (rel == prefix or rel.startswith(prefix + "/") or prefix.startswith(rel + "/"))

    try:
        found = [
            e for e in safefs.walk(home, skip_dir=off_path)
            if e.kind == "file" and e.rel.startswith(prefix + "/") and fnmatch.fnmatch(e.rel.rsplit("/", 1)[-1], pattern)
        ]
    except OSError:
        return None
    if not found:
        return None
    return safefs.read_text(home, max(found, key=lambda e: e.mtime).rel)


def words(text: str) -> int:
    return len((text or "").split())
