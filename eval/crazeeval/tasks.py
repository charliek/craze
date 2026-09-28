"""The task format (plan 029 §3.1.5).

Each task is ``eval/tasks/<id>/task.toml`` plus ``testdata/`` (hidden tests, trusted
test copies, ``reference.patch``, reference and decoy answers). Nothing under a task
directory is ever reachable from inside a sandbox.
"""

from __future__ import annotations

import tomllib
from dataclasses import dataclass, field
from pathlib import Path

from crazeeval import paths

IMPLEMENTATION = {"bugfix", "feature", "refactor", "multi-step"}
ANSWER_LIKE = {"explain", "answer", "plan"}
CATEGORIES = IMPLEMENTATION | ANSWER_LIKE | {"investigate", "verify"}
SPLITS = {"dev", "heldout", "smoke"}
MODES = {"build", "plan"}
CHECK_TYPES = {
    "facts",
    "no_writes",
    "tests",
    "diff_scope",
    "structural_count",
    "test_discrimination",
    "executed_code",
}


TEST_RUNNERS = ("pytest", "go")


class TaskError(ValueError):
    pass


@dataclass
class Task:
    id: str
    dir: Path
    category: str
    split: str
    mode: str
    prompt: str
    repo_kind: str  # "craze" | "fixture"
    fixture: str | None
    setup_patch: Path | None
    timeout_s: int
    rubric: list[str]
    checks: list[dict]
    validate: dict
    ignore: list[str] = field(default_factory=list)
    goflags: str | None = None
    harnesses: list[str] | None = None

    @property
    def repo_name(self) -> str:
        return "craze" if self.repo_kind == "craze" else (self.fixture or "repo")

    @property
    def is_plan(self) -> bool:
        return self.mode == "plan"

    def testdata(self, rel: str) -> Path:
        p = (self.dir / rel).resolve()
        if not str(p).startswith(str(self.dir.resolve()) + "/"):
            raise TaskError(f"{self.id}: {rel!r} escapes the task directory")
        return p


def load_task(d: Path) -> Task:
    d = Path(d)
    with open(d / "task.toml", "rb") as f:
        doc = tomllib.load(f)
    tid = doc.get("id") or d.name
    if tid != d.name:
        raise TaskError(f"task id {tid!r} does not match its directory {d.name!r}")
    cat = doc.get("category")
    if cat not in CATEGORIES:
        raise TaskError(f"{tid}: unknown category {cat!r}")
    split = doc.get("split", "dev")
    if split not in SPLITS:
        raise TaskError(f"{tid}: unknown split {split!r}")
    mode = doc.get("mode", "build")
    if mode not in MODES:
        raise TaskError(f"{tid}: unknown mode {mode!r}")
    repo = doc.get("repo") or {}
    kind = repo.get("kind")
    if kind not in ("craze", "fixture"):
        raise TaskError(f"{tid}: repo.kind must be craze or fixture")
    fixture = repo.get("name") if kind == "fixture" else None
    if kind == "fixture":
        if not fixture or not (paths.FIXTURES_DIR / fixture).is_dir():
            raise TaskError(f"{tid}: fixture {fixture!r} not found under eval/fixtures")
    setup = repo.get("setup_patch")
    checks = list(doc.get("checks") or [])
    for c in checks:
        if c.get("type") not in CHECK_TYPES:
            raise TaskError(f"{tid}: unknown check type {c.get('type')!r}")
        c.setdefault("name", c["type"])
        if c["type"] in ("tests", "test_discrimination"):
            # Trusted runners only (review r1-c1 finding 14): no free-form command.
            if "command" in c:
                raise TaskError(f"{tid}: {c['name']}: use runner/args, not command")
            if c.get("runner") not in TEST_RUNNERS:
                raise TaskError(f"{tid}: {c['name']}: runner must be one of {TEST_RUNNERS}")
        if c["type"] == "tests":
            expect = c.get("expect")
            if not isinstance(expect, list) or not expect or not all(isinstance(x, str) and x for x in expect):
                raise TaskError(f"{tid}: {c['name']}: expect must name the test ids that must run and pass")
    prompt = (doc.get("prompt") or "").strip()
    if not prompt:
        raise TaskError(f"{tid}: empty prompt")
    t = Task(
        id=tid,
        dir=d,
        category=cat,
        split=split,
        mode=mode,
        prompt=prompt,
        repo_kind=kind,
        fixture=fixture,
        setup_patch=None,
        timeout_s=int(doc.get("timeout_s", 1200)),
        rubric=list(doc.get("rubric") or []),
        checks=checks,
        validate=dict(doc.get("validate") or {}),
        ignore=list(doc.get("ignore") or []),
        goflags=doc.get("goflags"),
        harnesses=doc.get("harnesses"),
    )
    if setup:
        t.setup_patch = t.testdata(setup)
        if not t.setup_patch.exists():
            raise TaskError(f"{tid}: setup patch {setup!r} missing")
    return t


def load_tasks(root: Path | None = None) -> dict[str, Task]:
    root = Path(root or paths.TASKS_DIR)
    out = {}
    for d in sorted(root.iterdir()) if root.exists() else []:
        if d.is_dir() and (d / "task.toml").exists():
            t = load_task(d)
            out[t.id] = t
    return out


def select_tasks(all_tasks: dict[str, Task], spec: str | None) -> list[Task]:
    """``spec``: comma-separated ids and/or ``split:<name>`` terms; default = dev+heldout."""
    if not spec:
        return [t for t in all_tasks.values() if t.split in ("dev", "heldout")]
    out: list[Task] = []
    for term in [s.strip() for s in spec.split(",") if s.strip()]:
        if term.startswith("split:"):
            name = term.split(":", 1)[1]
            out += [t for t in all_tasks.values() if t.split == name or name == "all"]
        elif term in all_tasks:
            out.append(all_tasks[term])
        else:
            raise TaskError(f"unknown task {term!r}")
    return list({t.id: t for t in out}.values())  # first mention order, no repeats
