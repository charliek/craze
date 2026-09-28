"""Objective checks (plan 029 §3.1.5).

Check types: ``facts`` (regexes over the answer), ``no_writes`` (final state only:
no start file modified, deleted or changed in mode, no new file outside the ignore
list), ``tests`` (hidden tests added and trusted tests restored over the agent's
copies, then a trusted runner -- ``pytest`` or ``go`` -- in a network-less sandbox on
a scoring copy, which passes only when every expected test id reports a pass),
``diff_scope``, ``structural_count``, ``test_discrimination`` (the agent's tests
fail on the original implementation and pass on the agent's) and ``executed_code``
(the capture shows a code-running tool call). A contamination scan runs over every
run's tool calls.

Every host-side read, copy and restore of the agent's tree goes through safefs: no
link is followed, no special file opened, no write lands through a symlink.
"""

from __future__ import annotations

import fnmatch
import json
import re
import shutil
import xml.etree.ElementTree as ET
from dataclasses import dataclass, field
from pathlib import Path

from crazeeval import capture as cap
from crazeeval import safefs
from crazeeval.sandbox import SandboxSpec, Toolchains, run_sandboxed
from crazeeval.tasks import Task
from crazeeval.workspace import Start, build_manifest, changed_paths, compare, materialise

CHECK_TIMEOUT_S = 600


@dataclass
class ScoreContext:
    task: Task
    ws: Path  # the final workspace (host path); never modified by scoring
    start: Start
    answer: str
    records: list[dict]
    scoring_dir: Path
    tc: Toolchains | None
    ignores: list[str]
    ws_inside: str
    cache: Path | None = None
    keyring: object = None
    _diff: dict | None = field(default=None, repr=False)
    _final: dict | None = field(default=None, repr=False)
    _pristine: Path | None = field(default=None, repr=False)

    def final_manifest(self) -> dict:
        if self._final is None:
            self._final = build_manifest(self.ws)
        return self._final

    @property
    def diff(self) -> dict:
        if self._diff is None:
            self._diff = compare(self.start.manifest, self.final_manifest(), self.ignores)
        return self._diff

    def pristine(self) -> Path:
        """A fresh materialisation of the start state (for restores and discrimination)."""
        if self._pristine is None:
            dst = self.scoring_dir / "pristine"
            shutil.rmtree(dst, ignore_errors=True)
            dst.parent.mkdir(parents=True, exist_ok=True)
            st = materialise(self.task, dst, cache=self.cache)
            if st.commit != self.start.commit:
                raise RuntimeError(f"{self.task.id}: pristine start commit differs from the run's")
            self._pristine = dst
        return self._pristine


def result(check: dict, passed: bool, **details) -> dict:
    return {"name": check.get("name", check["type"]), "type": check["type"], "passed": bool(passed), "details": details}


def _patterns(v: str | list[str]) -> list[str]:
    return [v] if isinstance(v, str) else list(v)


def _timeout(check: dict) -> int:
    return int(check.get("timeout_s", CHECK_TIMEOUT_S))


# -- answer checks -----------------------------------------------------------------


def check_facts(check: dict, answer: str) -> dict:
    items = check.get("items") or []
    per = []
    for it in items:
        flags = re.I if it.get("ignore_case", True) else 0
        if it.get("dotall"):
            flags |= re.S
        found = re.search(it["regex"], answer or "", flags) is not None
        want = not it.get("absent", False)
        per.append({"id": it.get("id", it["regex"][:40]), "matched": found, "ok": found == want})
    need = check.get("min", len(per))
    ok_n = sum(1 for p in per if p["ok"])
    return result(check, ok_n >= need and bool(per), items=per, required=need, satisfied=ok_n)


def check_no_writes(check: dict, ctx: ScoreContext) -> dict:
    d = ctx.diff
    changed = changed_paths(d)
    return result(check, not changed, added=d["added"][:50], modified=d["modified"][:50], deleted=d["deleted"][:50])


def check_diff_scope(check: dict, ctx: ScoreContext) -> dict:
    changed = changed_paths(ctx.diff)
    allow = check.get("allow") or ["*"]
    deny = check.get("deny") or []
    outside = [p for p in changed if not any(fnmatch.fnmatch(p, a) for a in allow)]
    denied = [p for p in changed if any(fnmatch.fnmatch(p, x) for x in deny)]
    need_change = check.get("require_change", True)
    ok = not outside and not denied and (bool(changed) or not need_change)
    return result(check, ok, changed=changed[:100], outside=outside[:50], denied=denied[:50])


OPS = {
    "==": lambda a, b: a == b,
    "<=": lambda a, b: a <= b,
    ">=": lambda a, b: a >= b,
    "<": lambda a, b: a < b,
    ">": lambda a, b: a > b,
}


def check_structural_count(check: dict, ws: Path) -> dict:
    globs = _patterns(check.get("glob") or "**/*")
    rx = re.compile(check["regex"], re.M)
    total = 0
    per_file = {}
    for rel, v in build_manifest(ws).items():
        if v.startswith("f:") and any(fnmatch.fnmatch(rel, g) for g in globs):
            text = safefs.read_text(ws, rel)
            if text is None:
                continue
            n = len(rx.findall(text))
            if n:
                per_file[rel] = n
                total += n
    op = check.get("op", "==")
    value = int(check["value"])
    return result(check, OPS[op](total, value), count=total, op=op, value=value, files=per_file)


def check_executed_code(check: dict, records: list[dict]) -> dict:
    pats = [re.compile(p) for p in (check.get("patterns") or [r"\S"])]
    hits = []
    for c in cap.all_tool_calls(records):
        if not cap.is_exec(c):
            continue
        cmd = cap.call_command(c)
        if any(p.search(cmd) for p in pats):
            hits.append({"tool": c.get("name"), "command": cmd[:300]})
    return result(check, bool(hits), hits=hits[:20])


# -- sandboxed commands ---------------------------------------------------------------

OUT_INSIDE = "/sandbox/out"
# pytest reads no configuration file ("-c /dev/null"), so pyproject.toml, setup.cfg,
# tox.ini and pytest.ini cannot change it; these import-time hooks are restored to
# their trusted start version or removed (review r1-c1 finding 14).
PY_HOOK_NAMES = {"conftest.py", "pytest.py", "sitecustomize.py", "usercustomize.py", "pytest.ini"}
GO_TESTMAIN = re.compile(rb"^\s*func\s+TestMain\s*\(", re.M)
RUNNERS = ("pytest", "go")
MAX_REPORT = 32 * 1024 * 1024


def scoring_copy(ctx: ScoreContext, src: Path, label: str) -> Path:
    """A fresh copy of ``src`` in the scoring directory, for :func:`exec_scoring`:
    regular files, directories and symlinks (as links); special files skipped."""
    if ctx.tc is None:
        raise RuntimeError("scoring commands need toolchains (bubblewrap)")
    dst = ctx.scoring_dir / label
    shutil.rmtree(dst, ignore_errors=True)
    dst.parent.mkdir(parents=True, exist_ok=True)
    safefs.copy_tree(src, dst)
    return dst


async def exec_scoring(ctx: ScoreContext, dst: Path, label: str, cmd: list[str], timeout: int) -> dict:
    """Run ``cmd`` in the copy ``dst``: sandboxed, offline, with a writable report
    directory at /sandbox/out and a Go cache of its own, empty and deleted afterwards
    -- the code under test runs here, so nothing writable is shared with another
    scoring run or with validation (review r2-c1 item 12)."""
    out = ctx.scoring_dir / f"{label}.stdout"
    err = ctx.scoring_dir / f"{label}.stderr"
    report = ctx.scoring_dir / f"{label}.out"
    gocache = ctx.scoring_dir / f"{label}.gocache"
    for d in (report, gocache):
        shutil.rmtree(d, ignore_errors=True)
        d.mkdir(parents=True)
    spec = SandboxSpec(
        workspace=dst,
        ws_inside=ctx.ws_inside,
        home=None,
        network=False,
        goflags=ctx.task.goflags,
        gocache=gocache,
        extra_rw=[(report, OUT_INSIDE)],
        env={"PYTEST_DISABLE_PLUGIN_AUTOLOAD": "1"},
    )
    try:
        r = await run_sandboxed(spec, ctx.tc, cmd, stdout=out, stderr=err, timeout=timeout, keyring=ctx.keyring,
                                sample_env=False)
    finally:
        shutil.rmtree(gocache, ignore_errors=True)
    tail = (safefs.tail_text(out, 3000) + "\n" + safefs.tail_text(err, 2000)).strip()
    return {"exit": r.exit_code, "timed_out": r.timed_out, "tail": tail, "stdout": out, "report": report,
            "gocache": str(gocache)}


def _copy_in(task: Task, dst: Path, mapping: dict) -> list[str]:
    """Trusted files from the task's testdata, written over whatever the agent left."""
    placed = []
    for ws_rel, td_rel in (mapping or {}).items():
        safefs.write_bytes(dst, ws_rel, task.testdata(td_rel).read_bytes())
        placed.append(ws_rel)
    return placed


def _restore_from(pristine: Path, dst: Path, rels: list[str]) -> list[str]:
    """The start version of each file (from a fresh, trusted materialisation)."""
    done = []
    for rel in rels:
        try:
            data = safefs.read_bytes(pristine, rel)
        except OSError:
            safefs.remove(dst, rel)
        else:
            safefs.write_bytes(dst, rel, data)
        done.append(rel)
    return done


def _basename(rel: str) -> str:
    return rel.rsplit("/", 1)[-1]


def _not_git(rel: str) -> bool:
    return _basename(rel) == ".git"


def sanitize_python(dst: Path, pristine: Path) -> list[str]:
    """Restore each import-time pytest hook to its trusted start version, remove the
    ones the agent added, and drop compiled bytecode (sources are what run)."""
    actions = []
    trusted = {e.rel for e in safefs.walk(pristine, skip_dir=_not_git) if e.kind == "file" and _basename(e.rel) in PY_HOOK_NAMES}
    for e in list(safefs.walk(dst, skip_dir=_not_git)):
        name = _basename(e.rel)
        if e.kind == "dir" and name == "__pycache__":
            safefs.remove(dst, e.rel)
        elif e.kind != "dir" and (name.endswith(".pyc") or name.endswith(".pth")):
            safefs.remove(dst, e.rel)
        elif e.kind != "dir" and name in PY_HOOK_NAMES and e.rel not in trusted:
            safefs.remove(dst, e.rel)
            actions.append(f"removed {e.rel}")
    for rel in sorted(trusted):
        good = safefs.read_bytes(pristine, rel)
        try:
            cur = safefs.read_bytes(dst, rel)
        except OSError:
            cur = None
        if cur != good:
            safefs.write_bytes(dst, rel, good)
            actions.append(f"restored {rel}")
    return actions


TEST_FILE_PATTERNS = ("test_*.py", "*_test.py", "*_test.go")


def trusted_dirs(check: dict) -> list[str]:
    """The directories whose contents are the trusted test set: ``trusted_dirs`` when
    the task names them, else the (non-root) directories of the test files it adds or
    restores. The workspace root never is -- it holds the implementation."""
    if check.get("trusted_dirs") is not None:
        return sorted({d.strip("/") for d in check["trusted_dirs"] if d.strip("/")})
    rels = list((check.get("add") or {}).keys()) + list((check.get("restore") or {}).keys()) + list(
        check.get("restore_from_start") or [])
    out = set()
    for rel in rels:
        if "/" in rel and any(fnmatch.fnmatch(_basename(rel), p) for p in TEST_FILE_PATTERNS):
            out.add(rel.rsplit("/", 1)[0])
    return sorted(out)


def sanitize_trusted_dirs(dst: Path, pristine: Path, dirs: list[str], placed: set[str], runner: str) -> list[str]:
    """In each trusted test directory, remove every file that is not trusted -- an
    agent's ``__init__.py``, support module or extra test -- and put the start version
    of every trusted one back (review r2-c1 item 13). ``placed`` are the paths the
    check itself writes next (hidden and trusted copies). For Go only ``_test.go``
    files are in scope: the package's other files are the implementation."""
    actions = []

    def in_scope(rel: str) -> bool:
        return runner != "go" or rel.endswith("_test.go")

    for d in dirs:
        def off_path(rel: str, d=d) -> bool:
            return _not_git(rel) or not (rel == d or rel.startswith(d + "/") or d.startswith(rel + "/"))

        under = d + "/"
        trusted = {e.rel for e in safefs.walk(pristine, skip_dir=off_path)
                   if e.kind == "file" and e.rel.startswith(under) and in_scope(e.rel)}
        for e in list(safefs.walk(dst, skip_dir=off_path)):
            if e.kind == "dir" or not e.rel.startswith(under) or not in_scope(e.rel):
                continue
            if e.rel not in trusted and e.rel not in placed:
                safefs.remove(dst, e.rel)
                actions.append(f"removed {e.rel}")
        for rel in sorted(trusted):
            good = safefs.read_bytes(pristine, rel)
            try:
                cur = safefs.read_bytes(dst, rel)
            except OSError:
                cur = None
            if cur != good:
                safefs.write_bytes(dst, rel, good)
                actions.append(f"restored {rel}")
    return actions


def agent_testmains(dst: Path, pristine: Path) -> list[str]:
    """Go test files defining TestMain that are not the trusted start version."""
    bad = []
    for e in safefs.walk(dst, skip_dir=_not_git):
        if e.kind != "file" or not e.rel.endswith("_test.go"):
            continue
        data = safefs.read_bytes(dst, e.rel)
        if not GO_TESTMAIN.search(data):
            continue
        try:
            if safefs.read_bytes(pristine, e.rel) == data:
                continue
        except OSError:
            pass
        bad.append(e.rel)
    return bad


def runner_cmd(runner: str, ws_inside: str, args: list[str]) -> list[str]:
    if runner == "pytest":
        # -I: the workspace is not on sys.path at startup (a pytest.py cannot shadow
        # pytest); -B: no bytecode; -c /dev/null: no config file from the tree.
        return ["python", "-I", "-B", "-m", "pytest", "-c", "/dev/null", "--rootdir", ws_inside, "-p", "no:cacheprovider",
                "-q", "--junitxml", f"{OUT_INSIDE}/junit.xml", *args]
    if runner == "go":
        return ["go", "test", "-json", "-count=1", *args]
    raise ValueError(f"unknown test runner {runner!r}")


def _junit_results(report: Path) -> dict[str, str]:
    """test id -> passed/failed/error/skipped from pytest's JUnit XML. Each case is
    known by several spellings (``classname::name`` and the file-path form)."""
    raw = safefs.read_bytes(report, "junit.xml", limit=MAX_REPORT)
    out: dict[str, str] = {}
    for tc in ET.fromstring(raw).iter("testcase"):
        classname, name = tc.get("classname") or "", tc.get("name") or ""
        status = "passed"
        for child in tc:
            if child.tag in ("failure", "error", "skipped"):
                status = "failed" if child.tag == "failure" else child.tag
                break
        parts = classname.split(".") if classname else []
        ids = {f"{classname}::{name}"}
        for k in range(len(parts), 0, -1):
            ids.add("::".join(["/".join(parts[:k]) + ".py", *parts[k:], name]))
        for i in ids:
            out[i] = status
    return out


def _go_results(stdout: Path) -> dict[str, str]:
    """``<package>::<Test>`` -> pass/fail/skip from ``go test -json``."""
    out: dict[str, str] = {}
    text = safefs.read_text(stdout.parent, stdout.name, limit=MAX_REPORT) or ""
    for line in text.splitlines():
        if not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        if ev.get("Test") and ev.get("Action") in ("pass", "fail", "skip"):
            out[f"{ev.get('Package', '')}::{ev['Test']}"] = {"pass": "passed", "fail": "failed", "skip": "skipped"}[ev["Action"]]
    return out


def match_expected(runner: str, results: dict[str, str], expected: list[str]) -> dict[str, str]:
    """Each expected id's status ("missing" when it never ran)."""
    got = {}
    for want in expected:
        if runner == "go":
            pkg, _, test = want.rpartition("::")
            hits = [st for k, st in results.items() if k.rpartition("::")[2] == test
                    and (not pkg or k.rpartition("::")[0] == pkg or k.rpartition("::")[0].endswith("/" + pkg.lstrip("./")))]
            if not hits:
                got[want] = "missing"
            else:
                got[want] = "passed" if all(h == "passed" for h in hits) else next(h for h in hits if h != "passed")
        else:
            got[want] = results.get(want, "missing")
    return got


async def run_tests(ctx: ScoreContext, dst: Path, label: str, runner: str, args: list[str], timeout: int) -> dict:
    """Prepare ``dst`` for ``runner`` and run it; returns exit, results and actions."""
    actions: list[str] = []
    if runner == "pytest":
        actions = sanitize_python(dst, ctx.pristine())
    elif runner == "go":
        bad = agent_testmains(dst, ctx.pristine())
        if bad:
            return {"exit": None, "timed_out": False, "tail": "", "results": {}, "actions": actions,
                    "rejected": f"agent-added TestMain in {bad}"}
    r = await exec_scoring(ctx, dst, label, runner_cmd(runner, ctx.ws_inside, args), timeout)
    try:
        results = _junit_results(r["report"]) if runner == "pytest" else _go_results(r["stdout"])
    except (OSError, ET.ParseError):
        results = {}
    return {**r, "results": results, "actions": actions, "rejected": None}


async def check_tests(check: dict, ctx: ScoreContext, src: Path | None = None, label: str | None = None) -> dict:
    """Hidden tests added, trusted tests restored, then a trusted runner; passes only
    when it exits 0 and every expected test id reports a pass."""
    label = label or f"tests-{check.get('name', 'tests')}"
    dst = scoring_copy(ctx, src or ctx.ws, label)
    placed = set((check.get("add") or {}).keys()) | set((check.get("restore") or {}).keys()) | set(
        check.get("restore_from_start") or [])
    cleaned = sanitize_trusted_dirs(dst, ctx.pristine(), trusted_dirs(check), placed, check["runner"])
    restored = _copy_in(ctx.task, dst, check.get("restore") or {})
    if check.get("restore_from_start"):
        restored += _restore_from(ctx.pristine(), dst, list(check["restore_from_start"]))
    added = _copy_in(ctx.task, dst, check.get("add") or {})
    runner = check["runner"]
    r = await run_tests(ctx, dst, label, runner, list(check.get("args") or []), _timeout(check))
    expected = match_expected(runner, r["results"], list(check["expect"]))
    not_passed = {k: v for k, v in expected.items() if v != "passed"}
    passed = r["rejected"] is None and r["exit"] == 0 and not r["timed_out"] and not not_passed
    return result(check, passed, exit=r["exit"], timed_out=r["timed_out"], restored=restored, added=added,
                  sanitized=cleaned + r["actions"], rejected=r["rejected"], expected=expected, tail=r["tail"])


async def check_test_discrimination(check: dict, ctx: ScoreContext) -> dict:
    globs = _patterns(check.get("tests_glob") or ["test_*.py", "*_test.go", "tests/*.py"])
    d = ctx.diff
    tests = [p for p in d["added"] + d["modified"] if any(fnmatch.fnmatch(p, g) for g in globs)]
    tests = [p for p in tests if ctx.final_manifest().get(p, "").startswith("f:")]  # regular, within limits
    if not tests:
        return result(check, False, reason="no added or changed test files", tests=[])
    runner = check["runner"]
    if runner == "pytest":
        args = tests
    else:
        args = sorted({"./" + (p.rsplit("/", 1)[0] if "/" in p else ".") for p in tests})
    timeout = _timeout(check)

    # Against the original implementation: the pristine start plus the agent's tests.
    orig = scoring_copy(ctx, ctx.pristine(), "discrim-original")
    for rel in tests:
        safefs.write_bytes(orig, rel, safefs.read_bytes(ctx.ws, rel))
    r_orig = await run_tests(ctx, orig, "discrim-original", runner, args, timeout)
    agent = scoring_copy(ctx, ctx.ws, "discrim-agent")
    r_agent = await run_tests(ctx, agent, "discrim-agent", runner, args, timeout)
    ran = r_agent["results"]
    fails_on_original = r_orig["rejected"] is None and r_orig["exit"] not in (0, None) and not r_orig["timed_out"]
    passes_on_agent = (r_agent["rejected"] is None and r_agent["exit"] == 0 and not r_agent["timed_out"]
                       and bool(ran) and all(v in ("passed", "skipped") for v in ran.values())
                       and any(v == "passed" for v in ran.values()))
    return result(
        check,
        fails_on_original and passes_on_agent,
        tests=tests,
        original_exit=r_orig["exit"],
        agent_exit=r_agent["exit"],
        rejected=r_orig["rejected"] or r_agent["rejected"],
        agent_results=dict(list(ran.items())[:50]),
        original_tail=r_orig["tail"][-1500:],
        agent_tail=r_agent["tail"][-1500:],
    )


async def run_checks(ctx: ScoreContext) -> list[dict]:
    out = []
    for c in ctx.task.checks:
        t = c["type"]
        if t == "facts":
            out.append(check_facts(c, ctx.answer))
        elif t == "no_writes":
            out.append(check_no_writes(c, ctx))
        elif t == "diff_scope":
            out.append(check_diff_scope(c, ctx))
        elif t == "structural_count":
            out.append(check_structural_count(c, ctx.ws))
        elif t == "executed_code":
            out.append(check_executed_code(c, ctx.records))
        elif t == "tests":
            out.append(await check_tests(c, ctx))
        elif t == "test_discrimination":
            out.append(await check_test_discrimination(c, ctx))
    return out


# -- contamination -------------------------------------------------------------------

# Paths or URLs a run must never touch (plan 029 §3.1.5). The eval's own material is
# unreachable inside the sandbox; these catch the attempt, and a fetch of the public
# craze repository (whose eval/tasks would hold the hidden tests once pushed).
CONTAMINATION = [
    ("eval-dir", re.compile(r"(?<![\w.-])eval/")),
    ("reference-patch", re.compile(r"reference\.patch")),
    ("eval-runs", re.compile(r"eval-runs")),
    ("claude-plans", re.compile(r"\.claude/plans")),
    # The owner's craze home by absolute path. Inside the sandbox ~ is the run's own
    # temp home, whose generated .craze/native holds only the dummy key -- and craze's
    # plan mode writes its plan file there by design -- so ~ and /sandbox/home are not hits.
    ("craze-native", re.compile(r"(/home/[^/\s'\"]+|/root)/\.craze/native")),
    (
        "craze-github",
        re.compile(
            r"(https?://(www\.)?github\.com/charliek/craze|git@github\.com:charliek/craze|"
            r"raw\.githubusercontent\.com/charliek/craze|api\.github\.com/repos/charliek/craze|"
            r"codeload\.github\.com/charliek/craze|\bgh\b[^\n]*\bcharliek/craze\b|"
            r"git\s+(clone|fetch|pull|ls-remote|remote\s+add)[^\n]*github\.com[/:]charliek/craze)",
            re.I,
        ),
    ),
]
ABS_PATH = re.compile(r"(?<![\w.~$-])(/[^\s'\"`;|&()<>]+)")


def contamination_scan(records: list[dict], ws_inside: str) -> list[dict]:
    hits = []
    for i, c in enumerate(cap.all_tool_calls(records)):
        text = cap.call_text(c)
        for name, rx in CONTAMINATION:
            m = rx.search(text)
            if m:
                hits.append({"rule": name, "call": i, "tool": c.get("name"), "match": text[max(0, m.start() - 60) : m.end() + 60][:200]})
        # testdata/ and tasks/ count only outside the run's own workspace (craze keeps
        # many testdata/ directories of its own).
        for m in ABS_PATH.finditer(text):
            p = m.group(1)
            if p == ws_inside or p.startswith(ws_inside + "/"):
                continue
            if "/testdata/" in p + "/" or "/tasks/" in p + "/":
                hits.append({"rule": "outside-testdata-or-tasks", "call": i, "tool": c.get("name"), "match": p[:200]})
    return hits


def objective_pass(checks: list[dict], timed_out: bool, crashed: bool) -> bool:
    return bool(checks) and all(c["passed"] for c in checks) and not timed_out and not crashed


__all__ = [
    "ScoreContext",
    "run_checks",
    "contamination_scan",
    "objective_pass",
    "check_facts",
    "check_executed_code",
]
