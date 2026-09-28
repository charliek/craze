"""codex: ``codex exec --json`` against a Responses-API provider (plan 029 §3.1.4).

bwrap is the sandbox, so codex runs with ``--dangerously-bypass-approvals-and-sandbox``
(its own workspace-write sandbox would block the Go cache and cannot nest).
"""

from __future__ import annotations

from collections import Counter
from pathlib import Path

from crazeeval import homes, paths, safefs
from crazeeval.runners.base import Extract, Runner, read_ndjson

LAST_MESSAGE = "last-message.txt"


def extract_codex(events: list[dict], last_message: str | None) -> Extract:
    notes = []
    items = [e.get("item") or {} for e in events if e.get("type") == "item.completed"]
    answer = (last_message or "").strip()
    if not answer:
        msgs = [i.get("text") or "" for i in items if i.get("type") == "agent_message"]
        if msgs:
            answer = msgs[-1].strip()
            notes.append("answer from the last agent_message (no -o file)")
    for e in events:
        t = e.get("type")
        if t == "turn.failed":
            notes.append(f"turn.failed: {str((e.get('error') or {}).get('message'))[:300]}")
        elif t == "error":
            notes.append(f"error: {str(e.get('message'))[:300]}")
        elif t == "item.completed" and (e.get("item") or {}).get("type") == "error":
            notes.append(f"error item: {str(e['item'].get('message'))[:300]}")
    usage = next((e.get("usage") for e in reversed(events) if e.get("type") == "turn.completed"), None)
    counts = Counter(i.get("type") for i in items if i.get("type"))
    # Success: the last turn event is turn.completed ("error" events are retries).
    turns = [e.get("type") for e in events if e.get("type") in ("turn.completed", "turn.failed")]
    failure = None
    if not turns:
        failure = "no turn.completed"
    elif turns[-1] != "turn.completed":
        failure = "turn.failed"
    return Extract(answer=answer, notes=notes, extra={"usage": usage, "items": dict(counts)},
                   completed=failure is None, failure=failure)


class CodexRunner(Runner):
    name = "codex"
    tools = {"node"}
    supports_plan = False

    def home(self, home, em, snap, base_url):
        return homes.codex_home(home, em, snap, base_url)

    def command(self, em, task, ws_inside, snap):
        return [
            "codex", "exec", "--json",
            "-m", em.codex,
            "-c", f"model_provider={em.codex_provider}",
            "-c", f"model_reasoning_effort={em.effort}",
            "--skip-git-repo-check",
            "--dangerously-bypass-approvals-and-sandbox",
            "-C", ws_inside,
            "-o", f"{paths.SANDBOX_HOME}/{LAST_MESSAGE}",
            task.prompt,
        ]

    def extract(self, stdout, home: Path, task):
        # The agent can write its home: read the -o file without following a link
        # and only if it is a regular file (review r1-c1 finding 1).
        last = safefs.read_text(home, LAST_MESSAGE, limit=4 * 1024 * 1024)
        return extract_codex(read_ndjson(stdout), last)
