"""gx (grok-build fork): ``gx -p … --output-format streaming-messages-json`` (plan 029 §3.1.4)."""

from __future__ import annotations

import json

from crazeeval import homes
from crazeeval.runners.base import Extract, Runner, lead_with, newest_text, read_ndjson


def extract_gx(events: list[dict], plan_text: str | None = None) -> Extract:
    """The terminal ``result`` line's ``result`` (the last assistant message's text)."""
    notes = []
    results = [e for e in events if e.get("type") == "result"]
    answer = ""
    extra: dict = {}
    failure = None
    if results:
        r = results[-1]
        extra = {k: r.get(k) for k in ("subtype", "is_error", "num_turns", "stop_reason", "duration_ms")}
        if not r.get("is_error"):
            answer = (r.get("result") or "").strip()
        for e in r.get("errors") or []:
            notes.append(f"error: {str(e)[:300]}")
        notes.append(f"result={r.get('subtype')} stop={r.get('stop_reason')}")
        # Success is the terminal result line: subtype success, not an error.
        if r.get("is_error") or r.get("subtype") != "success":
            failure = f"result {r.get('subtype')} is_error={r.get('is_error')}"
    else:
        notes.append("no result line")
        failure = "no result line"
    # A plan presented through exit_plan_mode arrives as that tool's result.
    presented = plan_text
    if presented is None:
        for e in events:
            if e.get("type") != "user":
                continue
            for c in (e.get("message") or {}).get("content") or []:
                if isinstance(c, dict) and c.get("type") == "tool_result":
                    content = c.get("content")
                    if isinstance(content, str) and "plan_content" in content:
                        try:
                            presented = json.loads(content).get("plan_content") or presented
                        except ValueError:
                            pass
    if presented:
        answer = lead_with(presented, answer)
    init = next((e for e in events if e.get("type") == "system" and e.get("subtype") == "init"), None)
    if init:
        extra.update({k: init.get(k) for k in ("tools", "model", "permissionMode")})
    return Extract(answer=answer, notes=notes, extra=extra, completed=failure is None, failure=failure)


class GxRunner(Runner):
    name = "gx"
    tools = {"grok"}

    def home(self, home, em, snap, base_url):
        return homes.gx_home(home, em, snap, base_url)

    def command(self, em, task, ws_inside, snap):
        cmd = ["gx", "-m", em.gx, "-p", task.prompt, "--output-format", "streaming-messages-json", "--cwd", ws_inside]
        # --always-approve overrides --permission-mode plan (plan 029 X8): a plan task
        # asks for plan mode alone; whether gx plans headless is C2's question.
        cmd += ["--permission-mode", "plan"] if task.is_plan else ["--always-approve"]
        return cmd

    def extract(self, stdout, home, task):
        plan = newest_text(home, ".grok/sessions", "plan.md") if task.is_plan else None
        return extract_gx(read_ndjson(stdout), plan)

    def plan_mode(self, task):
        return "--permission-mode plan (without --always-approve; unverified, plan X8)" if task.is_plan else None
