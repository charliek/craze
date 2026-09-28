"""The judge's packet for one side of a pair (plan 029 §3.1.6).

A side's packet holds its final answer, the execution evidence from the capture (the
ordered tool calls with their commands or paths, and each result's exit status and
first lines -- plus its last lines when it is long; at most 12 KB), the workspace diff (the file list and stats always, the
diff itself up to 40 KB, then a truncation marker; harness state paths stripped) and
the objective results, timeouts included.

Provenance is normalised, names are not scrubbed: run-specific absolute paths become
``<workspace>``, ``<home>``, ``<tmp>`` (and ``<run>`` for the host's run directory), a
harness's plan-file location becomes ``<plan-file>``, and everything else -- task-subject
words, repository paths, harness names -- stays as it is. Tool names are the exception:
each harness names its tools differently (``bash``, ``run_terminal_command``,
``exec_command``), so the evidence shows a tool's kind (``shell``, ``read``, ``edit``,
``search``...; capture.TOOL_KINDS) and argument names are unified (``path=``,
``pattern=``); commands and results stay as they are (review r1-c2 §5).
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path

from crazeeval import capture as cap
from crazeeval import paths
from crazeeval.tasks import Task
from crazeeval.workspace import GENERATED_IGNORES, is_ignored

EVIDENCE_LIMIT = 12 * 1024
DIFF_LIMIT = 40 * 1024
RESULT_LINES = 3
RESULT_TAIL_LINES = 3
RESULT_LINE_CHARS = 200
ARG_CHARS = 300

BEGIN = "<<<BEGIN UNTRUSTED CANDIDATE {side}>>>"
END = "<<<END UNTRUSTED CANDIDATE {side}>>>"


# -- normalisation -----------------------------------------------------------------------

PLAN_FILE_RE = re.compile(
    r"<home>/\.(?:craze|grok|codex|xdg|local|config|opencode)/[^\s'\"`)\]]*?plan[^\s'\"`)\]/]*\.md"
    r"|<workspace>/\.opencode/plans?/[^\s'\"`)\]]+\.md"
)
TMP_RE = re.compile(r"(?<![\w.~<>-])/tmp/[^\s/'\"`)\]]+")


def normaliser(ws_inside: str, host_dirs: list[str] | None = None):
    """A function that normalises run-specific paths in text (§3.1.6). ``ws_inside``
    is the workspace path inside the sandbox; ``host_dirs`` are host paths of this
    run (its attempt directory) that stand for ``<run>``."""
    ws = ws_inside.rstrip("/")
    hosts = sorted({h.rstrip("/") for h in host_dirs or [] if h}, key=len, reverse=True)

    def norm(text: str) -> str:
        if not text:
            return text or ""
        out = text
        for h in hosts:
            out = out.replace(h + "/ws/" + ws.rsplit("/", 1)[-1], "<workspace>")
            out = out.replace(h + "/home", "<home>")
            out = out.replace(h, "<run>")
        out = out.replace(ws, "<workspace>")
        out = out.replace(paths.SANDBOX_HOME, "<home>")
        out = out.replace(paths.SANDBOX_WORK, "<workspace-root>")
        out = TMP_RE.sub("<tmp>", out)
        out = PLAN_FILE_RE.sub("<plan-file>", out)
        return out

    return norm


# -- loading a run ------------------------------------------------------------------------


@dataclass
class Side:
    """One judged run: its final result and the attempt directory that produced it."""

    rep_dir: Path
    result: dict
    attempt_dir: Path

    @property
    def key(self) -> str:
        return self.result.get("run_key") or str(self.rep_dir)

    def records(self) -> list[dict]:
        return cap.read_capture(self.attempt_dir)

    def diff_text(self) -> str:
        p = self.attempt_dir / "diff.patch"
        try:
            return p.read_text(errors="replace")
        except OSError:
            return ""


def attempt_dir(rep_dir: Path, result: dict) -> Path:
    """The attempt directory behind a rep's final result: its last recorded attempt,
    else the highest-numbered ``attempt-N`` on disk, else the rep directory itself."""
    attempts = result.get("attempts") or []
    adir = rep_dir / attempts[-1]["dir"] if attempts and attempts[-1].get("dir") else rep_dir
    if not (adir / "result.json").exists():
        found = sorted((p for p in rep_dir.glob("attempt-*") if p.is_dir()), key=lambda p: int(p.name[8:] or 0))
        adir = found[-1] if found else rep_dir
    return adir


def load_side(rep_dir: Path) -> Side:
    rep_dir = Path(rep_dir)
    result = json.loads((rep_dir / "result.json").read_text())
    return Side(rep_dir=rep_dir, result=result, attempt_dir=attempt_dir(rep_dir, result))


# -- execution evidence ---------------------------------------------------------------------

EXIT_RES = [
    re.compile(r"(?im)process exited with code (-?\d+)"),
    re.compile(r"(?im)^\s*exit(?:\s+code)?\s*[:=]\s*(-?\d+)"),
    re.compile(r"(?im)\bexit(?:ed)?(?: with)? (?:code|status) (-?\d+)"),
    re.compile(r'(?i)"exit_code"\s*:\s*(-?\d+)'),
    re.compile(r"(?i)<exit_code>\s*(-?\d+)"),
]
PATH_KEYS = ("filePath", "file_path", "path", "filename", "file", "notebook_path", "target_file", "target_directory",
             "dir_path", "directory")
PATTERN_KEYS = ("pattern", "query", "glob", "regex")


def exit_status(text: str) -> str | None:
    for rx in EXIT_RES:
        m = rx.search(text or "")
        if m:
            return m.group(1)
    return None


content_text = cap.content_text
tool_results = cap.tool_results
# Calls that are harness housekeeping, not work on the task: left out of the evidence
# (gx's session_title is gx's alone and would name the harness).
EVIDENCE_SKIP_KINDS = {"title"}


def call_summary(call: dict) -> str:
    """The command, path or arguments of a tool call, in one line. Argument names differ
    between harnesses (``filePath``, ``target_file``, ``file_path``...), so a path is
    shown as ``path=`` and a pattern as ``pattern=`` whatever the harness called it."""
    if cap.is_exec(call):
        return cap.call_command(call)
    args = call.get("arguments")
    if isinstance(args, dict):
        bits = [f"path={args[k]}" for k in PATH_KEYS if isinstance(args.get(k), str)]
        bits += [f"pattern={args[k]!r}" for k in PATTERN_KEYS if isinstance(args.get(k), str)]
        if bits:
            return " ".join(bits)
    return cap.call_text(call)


def _one_line(s: str, n: int) -> str:
    s = " ".join((s or "").split())
    return s if len(s) <= n else s[: n - 1] + "…"


def result_lines(text: str, head: int = RESULT_LINES, tail: int = RESULT_TAIL_LINES) -> list[str]:
    """A result's first lines -- and, when it is longer, its last lines too: a test
    run's summary and a command's error are at the end (the judge smoke showed a
    passing pytest summary hidden behind a directory listing)."""
    lines = [ln for ln in (text or "").splitlines() if ln.strip()]
    if len(lines) <= head + tail + 1:
        return lines
    return lines[:head] + [f"[… {len(lines) - head - tail} lines …]"] + lines[-tail:]


def evidence(records: list[dict], norm, limit: int = EVIDENCE_LIMIT) -> str:
    """The ordered tool calls -- each shown by its harness-neutral kind (capture.TOOL_KINDS),
    with its command or path -- and each result's exit status, first lines and, for a long
    result, last lines (§3.1.6). At most ``limit`` bytes, UTF-8 encoded."""
    results = tool_results(records)
    calls = [c for c in cap.all_tool_calls(records) if cap.tool_kind(c.get("name")) not in EVIDENCE_SKIP_KINDS]
    if not calls:
        return "(no tool calls)"
    lines: list[str] = []
    used = 0
    for i, c in enumerate(calls, 1):
        head = f"[{i}] {cap.tool_kind(c.get('name'))}: {_one_line(norm(call_summary(c)), ARG_CHARS)}"
        res = results.get(c.get("id") or "")
        if res is None:
            body = "    -> (no result recorded)"
        else:
            st = exit_status(res)
            shown = [_one_line(ln, RESULT_LINE_CHARS) for ln in result_lines(norm(res))]
            body = f"    -> exit {st}" if st is not None else "    ->"
            if shown:
                body += "\n" + "\n".join("       " + ln for ln in shown)
        block = head + "\n" + body
        size = len(block.encode()) + 1  # the cap is in bytes: multibyte output counts as such
        if used + size > limit:
            lines.append(f"[evidence truncated: {len(calls) - i + 1} more tool calls not shown]")
            break
        lines.append(block)
        used += size
    return "\n".join(lines)


# -- the diff -------------------------------------------------------------------------------

DIFF_HEADER = re.compile(r"^diff --git a/(.*?) b/(.*)$", re.M)


def split_diff(text: str) -> list[tuple[str, str]]:
    """(path, section) per file of a git diff."""
    heads = list(DIFF_HEADER.finditer(text or ""))
    out = []
    for i, m in enumerate(heads):
        end = heads[i + 1].start() if i + 1 < len(heads) else len(text)
        out.append((m.group(2), text[m.start() : end]))
    return out


def diff_stats(section: str) -> tuple[int, int, bool]:
    add = dele = 0
    binary = "Binary files" in section or "GIT binary patch" in section
    for ln in section.splitlines():
        if ln.startswith("+++") or ln.startswith("---"):
            continue
        if ln.startswith("+"):
            add += 1
        elif ln.startswith("-"):
            dele += 1
    return add, dele, binary


def diff_section(diff_text: str, norm, strip: list[str] | None = None, limit: int = DIFF_LIMIT) -> str:
    """The file list and stats always, then the diff up to ``limit`` bytes (§3.1.6)."""
    patterns = list(GENERATED_IGNORES) + list(strip or [])
    files = [(p, s) for p, s in split_diff(diff_text) if not is_ignored(p, patterns)]
    if not files:
        return "Files changed: none"
    out = ["Files changed:"]
    for p, s in files:
        a, d, b = diff_stats(s)
        out.append(f"  {p} | " + ("binary" if b else f"+{a} -{d}"))
    shown: list[str] = []
    used = 0
    hidden_bytes = hidden_files = 0
    for _, s in files:
        s = norm(s)
        size = len(s.encode())
        if used + size <= limit:
            shown.append(s)
            used += size
        else:
            hidden_bytes += size
            hidden_files += 1
    out.append("")
    out.append("".join(shown).rstrip("\n"))
    if hidden_files:
        out.append(f"[diff truncated: {hidden_bytes} bytes in {hidden_files} files not shown]")
    return "\n".join(out)


# -- objective results ------------------------------------------------------------------------


def _check_line(c: dict) -> str:
    d = c.get("details") or {}
    mark = "PASS" if c.get("passed") else "FAIL"
    t = c.get("type")
    extra = ""
    if t == "facts":
        extra = f"{d.get('satisfied')}/{len(d.get('items') or [])} items, {d.get('required')} required"
    elif t == "tests":
        bad = {k: v for k, v in (d.get("expected") or {}).items() if v != "passed"}
        if d.get("timed_out"):
            extra = "timed out"
        elif d.get("rejected"):
            extra = str(d["rejected"])[:200]
        elif bad:
            extra = "not passing: " + ", ".join(f"{k} ({v})" for k, v in list(bad.items())[:8])
        else:
            extra = f"{len(d.get('expected') or {})} expected tests passed"
    elif t == "no_writes":
        changed = (d.get("added") or []) + (d.get("modified") or []) + (d.get("deleted") or [])
        extra = "no changes" if not changed else "changed: " + ", ".join(changed[:10])
    elif t == "diff_scope":
        extra = ("outside scope: " + ", ".join((d.get("outside") or [])[:10])) if d.get("outside") else ""
        if d.get("denied"):
            extra += (" " if extra else "") + "denied: " + ", ".join(d["denied"][:10])
    elif t == "structural_count":
        extra = f"count {d.get('count')} (want {d.get('op')} {d.get('value')})"
    elif t == "test_discrimination":
        extra = d.get("reason") or f"on the original: exit {d.get('original_exit')}; on the agent's: exit {d.get('agent_exit')}"
    elif t == "executed_code":
        extra = f"{len(d.get('hits') or [])} answered code-running calls that exercised the case"
        if d.get("attempted_without_result"):
            extra += f"; {len(d['attempted_without_result'])} attempted without a result"
    elif t == "shared_helper":
        if d.get("helpers"):
            extra = "shared helper: " + ", ".join(d["helpers"][:3])
        else:
            extra = "no shared helper" + (f"; still inline in {', '.join(d['callers_with_markers'])}"
                                          if d.get("callers_with_markers") else "")
    return f"  {mark} {c.get('name')} ({t})" + (f": {extra}" if extra else "")


def objective_section(result: dict) -> str:
    status = result.get("status")
    lines = [f"Run status: {status}" + (" (timed out: killed at the time limit)" if result.get("timed_out") else "")]
    if result.get("failure"):
        lines.append(f"Harness-reported failure: {result['failure']}")
    lines.append(f"Objective result: {'PASS' if result.get('objective_pass') else 'FAIL'}")
    for c in result.get("checks") or []:
        lines.append(_check_line(c))
    if not result.get("checks"):
        lines.append("  (no checks were scored)")
    return "\n".join(lines)


# -- one side -----------------------------------------------------------------------------------


def side_block(label: str, side: Side, task: Task, strip: list[str] | None = None,
               answer_override: str | None = None) -> str:
    """One candidate's packet, framed as untrusted data."""
    ws_inside = f"{paths.SANDBOX_WORK}/{task.repo_name}"
    norm = normaliser(ws_inside, [str(side.attempt_dir)])
    answer = side.result.get("answer") if answer_override is None else answer_override
    records = side.records()
    parts = [
        BEGIN.format(side=label),
        f"## Candidate {label}: final answer",
        norm(answer or "(no final answer)"),
        "",
        f"## Candidate {label}: execution evidence (tool calls in order, each with its result's exit status, first lines and, "
        "for a long result, last lines)",
        evidence(records, norm),
        "",
        f"## Candidate {label}: workspace changes",
        diff_section(side.diff_text(), norm, strip),
        "",
        f"## Candidate {label}: objective results",
        objective_section(side.result),
        END.format(side=label),
    ]
    return "\n".join(parts)


def task_block(task: Task) -> str:
    lines = ["# The task", "", task.prompt.strip(), ""]
    if task.is_plan:
        lines += ["(A planning task: the candidate's answer is the plan it presented; it should change no files.)", ""]
    lines += ["# The rubric (facts a correct answer contains, with their sources)", ""]
    lines += [f"{i}. {item}" for i, item in enumerate(task.rubric, 1)]
    if task.false_claims:
        lines += ["", "# Known false claims (a candidate making one of these states something false about the code)", ""]
        lines += [f"- {c}" for c in task.false_claims]
    return "\n".join(lines)


def build_packet(task: Task, a: Side, b: Side, strip: list[str] | None = None,
                 answers: tuple[str | None, str | None] = (None, None)) -> str:
    """The data part of a judge prompt: the task, the rubric, then candidates A and B."""
    return "\n\n".join([
        task_block(task),
        side_block("A", a, task, strip, answers[0]),
        side_block("B", b, task, strip, answers[1]),
    ]) + "\n"
