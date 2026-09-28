"""craze native: ``craze prompt --provider native --json`` (plan 029 §3.1.4)."""

from __future__ import annotations

from crazeeval import homes
from crazeeval.runners.base import Extract, Runner, after_last, lead_with, newest_text, read_ndjson
from crazeeval.sandbox import TOOL_INSIDE


def extract_craze(events: list[dict], plan_text: str | None = None) -> Extract:
    """Text events after the last tool event, main agent only (sub-agent events carry
    an ``agent`` field). In plan mode the presented plan leads the answer."""
    main = [e for e in events if not e.get("agent")]
    text = "".join(e.get("text", "") for e in after_last(main, "tool") if e.get("type") == "text").strip()
    notes = []
    stop = [e.get("stopReason") for e in events if e.get("type") == "done"]
    if stop:
        notes.append(f"stop={stop[-1]}")
    errs = [e.get("message", "") for e in events if e.get("type") == "error"]
    notes += [f"error: {m[:300]}" for m in errs]
    tools = sum(1 for e in main if e.get("type") == "tool" and e.get("status") in ("completed", "failed"))
    answer = lead_with(plan_text, text) if plan_text else text
    # Success is a final ``done`` with end_turn and no error event.
    last_stop = stop[-1] if stop else None
    failure = None
    if errs:
        failure = f"error event: {errs[-1][:200]}"
    elif last_stop is None:
        failure = "no done event"
    elif last_stop != "end_turn":
        failure = f"stopReason {last_stop}"
    return Extract(answer=answer, notes=notes, extra={"tool_events": tools, "stop_reason": last_stop},
                   completed=failure is None, failure=failure)


class CrazeRunner(Runner):
    name = "craze"
    tools = {"craze"}

    def home(self, home, em, snap, base_url):
        return homes.craze_home(home, em, snap, base_url)

    def command(self, em, task, ws_inside, snap):
        cmd = [TOOL_INSIDE["craze"], "prompt", "--provider", "native", "--model", em.craze, "--workspace", ws_inside, "--json"]
        if task.is_plan:
            cmd.append("--plan")
        return cmd + ["--", task.prompt]

    def extract(self, stdout, home, task):
        plan = newest_text(home, ".craze", "*.plan.md") if task.is_plan else None
        return extract_craze(read_ndjson(stdout), plan)

    def plan_mode(self, task):
        return "--plan" if task.is_plan else None
