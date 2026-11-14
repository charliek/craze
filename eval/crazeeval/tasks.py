"""The task format (plan 029 §3.1.5).

Each task is ``eval/tasks/<id>/task.toml`` plus ``testdata/`` (hidden tests, trusted
test copies, ``reference.patch``, reference and decoy answers). Nothing under a task
directory is ever reachable from inside a sandbox.
"""

from __future__ import annotations

import re
import tomllib
from dataclasses import dataclass, field, replace
from pathlib import Path

from crazeeval import paths, webcorpus

IMPLEMENTATION = {"bugfix", "feature", "refactor", "multi-step"}
ANSWER_LIKE = {"explain", "answer", "plan"}
CATEGORIES = IMPLEMENTATION | ANSWER_LIKE | {"investigate", "verify"}
# web (craze plan 051 E4): tasks whose decisive facts are only in their corpus, run on
# craze and craze-web; never in the default selection, nor in split:all.
WEB_SPLIT = "web"
SPLITS = {"dev", "heldout", "smoke", WEB_SPLIT}
MODES = {"build", "plan"}
CHECK_TYPES = {
    "facts",
    "no_writes",
    "tests",
    "diff_scope",
    "structural_count",
    "test_discrimination",
    "executed_code",
    "shared_helper",
}


TEST_RUNNERS = ("pytest", "go")
# Where an executed_code check's evidence may show (its ``evidence_in``; checks.py):
# the call's command, the result it returned, or a file an earlier call wrote that the
# command names.
EVIDENCE_PLACES = ("command", "result", "file")
FULL_SHA = re.compile(r"^[0-9a-f]{40}$")


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
    name: str = ""  # a short slug (e.g. explain-prompt-prefix) for reports
    false_claims: list[str] = field(default_factory=list)  # claims the judge must treat as false
    # A craze-repo task's commit: ``[repo] commit`` when the task pins one, else
    # paths.CRAZE_TEMPLATE_COMMIT (resolved at load). None for a fixture task.
    commit: str | None = None
    # A web task's corpus (its directory under testdata/ and its pages, parsed at load),
    # the corpus files holding its decisive facts, and the strings a solution must
    # contain (validate.web_lint). None and empty for every other task.
    corpus: Path | None = None
    web_corpus: webcorpus.Corpus | None = None
    relevant: list[str] = field(default_factory=list)
    needs: list[str] = field(default_factory=list)
    # A variant's base task directory (``variant_of``): where every testdata path but the
    # variant's own setup patch resolves -- its corpus, hidden tests and reference patch
    # are the base's. None for every other task.
    base_dir: Path | None = None

    @property
    def repo_name(self) -> str:
        return "craze" if self.repo_kind == "craze" else (self.fixture or "repo")

    @property
    def craze_commit(self) -> str | None:
        """The craze commit this task materialises (None for a fixture task)."""
        if self.repo_kind != "craze":
            return None
        return self.commit or paths.CRAZE_TEMPLATE_COMMIT

    @property
    def is_plan(self) -> bool:
        return self.mode == "plan"

    def testdata(self, rel: str) -> Path:
        return _inside(self.id, self.base_dir or self.dir, rel)


def _inside(tid: str, root: Path, rel: str) -> Path:
    p = (root / rel).resolve()
    if not str(p).startswith(str(root.resolve()) + "/"):
        raise TaskError(f"{tid}: {rel!r} escapes the task directory")
    return p


# What a variant's task.toml may say (``variant_of``): everything else is its base's.
VARIANT_KEYS = {"id", "name", "variant_of", "prompt", "setup_patch"}


def load_task(d: Path) -> Task:
    d = Path(d)
    with open(d / "task.toml", "rb") as f:
        doc = tomllib.load(f)
    tid = doc.get("id") or d.name
    if tid != d.name:
        raise TaskError(f"task id {tid!r} does not match its directory {d.name!r}")
    if "variant_of" in doc:
        return _load_variant(d, tid, doc)
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
    commit = repo.get("commit")
    if commit is not None:
        # A full sha only: it keys the template cache and is what every isolation check
        # verifies against, so an abbreviation or a ref name (which could move) is refused.
        if kind != "craze":
            raise TaskError(f"{tid}: repo.commit applies to a craze-repo task only")
        if not isinstance(commit, str) or not FULL_SHA.match(commit):
            raise TaskError(f"{tid}: repo.commit must be a full 40-character lowercase sha, got {commit!r}")
    elif kind == "craze":
        commit = paths.CRAZE_TEMPLATE_COMMIT
    checks = list(doc.get("checks") or [])
    seen_names: set[str] = set()
    for c in checks:
        if c.get("type") not in CHECK_TYPES:
            raise TaskError(f"{tid}: unknown check type {c.get('type')!r}")
        c.setdefault("name", c["type"])
        # The name checks.result() records (review c11/c12 P2): a task with two checks
        # sharing a name would let one silently stand in for the other at rescore time.
        if c["name"] in seen_names:
            raise TaskError(f"{tid}: duplicate check name {c['name']!r}")
        seen_names.add(c["name"])
        if c["type"] in ("tests", "test_discrimination"):
            # Trusted runners only (review r1-c1 finding 14): no free-form command.
            if "command" in c:
                raise TaskError(f"{tid}: {c['name']}: use runner/args, not command")
            if c.get("runner") not in TEST_RUNNERS:
                raise TaskError(f"{tid}: {c['name']}: runner must be one of {TEST_RUNNERS}")
        if c["type"] == "structural_count" and c.get("target", "content") not in ("content", "path"):
            raise TaskError(f"{tid}: {c['name']}: target must be content or path")
        if c["type"] == "executed_code" and not c.get("evidence"):
            # Without evidence any code-running call passes, `python --version` included
            # (review r1-c2 §4).
            raise TaskError(f"{tid}: {c['name']}: executed_code needs evidence regexes")
        if c["type"] == "executed_code" and "evidence_in" in c:
            where = c["evidence_in"]
            # Each element is checked as one of the place names before anything hashes it
            # (plan 033 C11r3, review r10 P3): a nested array or table is valid TOML, and
            # set() over it raised TypeError instead of this error.
            places_ok = isinstance(where, list) and all(isinstance(x, str) and x in EVIDENCE_PLACES for x in where)
            if not places_ok or not where:
                raise TaskError(f"{tid}: {c['name']}: evidence_in must list some of {EVIDENCE_PLACES}")
        if c["type"] == "shared_helper" and not (c.get("callers") and c.get("markers")):
            raise TaskError(f"{tid}: {c['name']}: shared_helper needs callers and markers")
        if c["type"] == "tests":
            expect = c.get("expect")
            if not _nonempty_strings(expect):
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
        name=str(doc.get("name") or ""),
        false_claims=list(doc.get("false_claims") or []),
        commit=commit,
    )
    if setup:
        t.setup_patch = t.testdata(setup)
        if not t.setup_patch.exists():
            raise TaskError(f"{tid}: setup patch {setup!r} missing")
    _load_web(t, doc)
    return t


def _load_variant(d: Path, tid: str, doc: dict) -> Task:
    """A web task's variant (``variant_of``, plan 051 follow-up): the base web task with
    its own id, name and prompt, and optionally a ``setup_patch`` of its own (in its own
    directory) applied to the fixture before its initial commit -- so the variant can
    take out what the base's workspace says that its prompt must not, the docs site's
    address, with no trace in the history. Everything else -- the fixture, checks,
    corpus, needs, hidden tests and reference patch -- is the base's, read from the
    base's directory. The base is a web task that is not itself a variant."""
    if extra := sorted(set(doc) - VARIANT_KEYS):
        raise TaskError(f"{tid}: a variant sets only {sorted(VARIANT_KEYS)}, not {extra}")
    base_id = doc["variant_of"]
    if not isinstance(base_id, str) or not base_id or "/" in base_id or base_id in (".", ".."):
        raise TaskError(f"{tid}: variant_of must name a task directory")
    base_dir = d.parent / base_id
    if not (base_dir / "task.toml").exists():
        raise TaskError(f"{tid}: variant_of {base_id!r}: no such task")
    with open(base_dir / "task.toml", "rb") as f:
        if "variant_of" in tomllib.load(f):
            raise TaskError(f"{tid}: variant_of {base_id!r} is itself a variant")
    base = load_task(base_dir)
    if base.split != WEB_SPLIT:
        raise TaskError(f"{tid}: variant_of {base_id!r}: only a web task has variants")
    prompt = (doc.get("prompt") or "").strip()
    if not prompt:
        raise TaskError(f"{tid}: empty prompt")
    setup = None
    if "setup_patch" in doc:
        setup = _inside(tid, d, doc["setup_patch"])
        if not setup.exists():
            raise TaskError(f"{tid}: setup patch {doc['setup_patch']!r} missing")
    return replace(base, id=tid, dir=d, base_dir=base_dir, name=str(doc.get("name") or ""), prompt=prompt,
                   setup_patch=setup)


def _nonempty_strings(v) -> bool:
    return isinstance(v, list) and bool(v) and all(isinstance(x, str) and x for x in v)


def _load_web(t: Task, doc: dict) -> None:
    """A web task's ``corpus``, ``relevant`` and ``needs`` (craze plan 051 E4): all three
    required on the web split, none allowed elsewhere."""
    keys = [k for k in ("corpus", "relevant", "needs") if k in doc]
    if t.split != WEB_SPLIT:
        if keys:
            raise TaskError(f"{t.id}: {', '.join(keys)} belong(s) to a web-split task only")
        return
    if t.repo_kind != "fixture":
        raise TaskError(f"{t.id}: a web task runs on a fixture")
    if not isinstance(doc.get("corpus"), str) or not doc["corpus"]:
        raise TaskError(f"{t.id}: a web task names its corpus directory")
    if not _nonempty_strings(doc.get("relevant")) or not _nonempty_strings(doc.get("needs")):
        raise TaskError(f"{t.id}: a web task lists its relevant corpus files and its needs")
    if not t.harnesses or "craze-web" not in t.harnesses:
        raise TaskError(f"{t.id}: a web task's harnesses include craze-web")
    t.corpus = t.testdata(doc["corpus"])
    try:
        t.web_corpus = webcorpus.load_corpus(t.corpus)
    except (webcorpus.CorpusError, OSError, UnicodeDecodeError) as e:
        raise TaskError(f"{t.id}: corpus: {e}") from None
    files = {p.file for p in t.web_corpus.pages}
    if missing := [f for f in doc["relevant"] if f not in files]:
        raise TaskError(f"{t.id}: relevant files not in the corpus: {missing}")
    t.relevant = list(doc["relevant"])
    t.needs = list(doc["needs"])


def load_tasks(root: Path | None = None) -> dict[str, Task]:
    root = Path(root or paths.TASKS_DIR)
    out = {}
    for d in sorted(root.iterdir()) if root.exists() else []:
        if d.is_dir() and (d / "task.toml").exists():
            t = load_task(d)
            out[t.id] = t
    return out


def select_tasks(all_tasks: dict[str, Task], spec: str | None) -> list[Task]:
    """``spec``: comma-separated ids and/or ``split:<name>`` terms; default = dev+heldout.
    ``split:all`` is every split but web: a web task runs only when named, by its id or
    ``split:web``, so no existing campaign's selection gains one."""
    if not spec:
        return [t for t in all_tasks.values() if t.split in ("dev", "heldout")]
    out: list[Task] = []
    for term in [s.strip() for s in spec.split(",") if s.strip()]:
        if term.startswith("split:"):
            name = term.split(":", 1)[1]
            out += [t for t in all_tasks.values() if t.split == name or (name == "all" and t.split != WEB_SPLIT)]
        elif term in all_tasks:
            out.append(all_tasks[term])
        else:
            raise TaskError(f"unknown task {term!r}")
    return list({t.id: t for t in out}.values())  # first mention order, no repeats
