"""opencode: ``opencode run --format json`` on its own built-in providers (plan 029 §3.1.4)."""

from __future__ import annotations

from crazeeval import homes
from crazeeval.runners.base import Extract, Runner, after_last, read_ndjson


def extract_opencode(events: list[dict]) -> Extract:
    """The text parts of the last step that finished; failing that, text after the
    last ``tool_use``."""
    notes = []
    finishes = [e for e in events if e.get("type") == "step_finish"]
    texts = [e for e in events if e.get("type") == "text"]
    answer = ""
    if finishes:
        last = finishes[-1].get("part") or {}
        mid = last.get("messageID")
        notes.append(f"finish={last.get('reason')}")
        seen = set()
        parts = []
        for e in texts:
            p = e.get("part") or {}
            if p.get("messageID") == mid and p.get("id") not in seen and not p.get("synthetic"):
                seen.add(p.get("id"))
                parts.append(p.get("text") or "")
        answer = "".join(parts).strip()
    if not answer:
        tail = after_last(events, "tool_use")
        answer = "".join((e.get("part") or {}).get("text") or "" for e in tail if e.get("type") == "text").strip()
    errors = []
    for e in events:
        if e.get("type") == "error":
            err = e.get("error") or {}
            msg = (err.get("data") or {}).get("message") or err.get("name") or ""
            errors.append(str(msg)[:300])
            notes.append(f"error: {str(msg)[:300]}")
    tools = sum(1 for e in events if e.get("type") == "tool_use")
    # Success: the last step finished with "stop" and no error event came at all
    # (an error after partial text is a failure).
    reason = (finishes[-1].get("part") or {}).get("reason") if finishes else None
    failure = None
    if errors:
        failure = f"error event: {errors[-1][:200]}"
    elif reason != "stop":
        failure = f"last step finish {reason}"
    return Extract(answer=answer, notes=notes, extra={"tool_events": tools, "finish": reason},
                   completed=failure is None, failure=failure)


class OpencodeRunner(Runner):
    name = "opencode"
    tools = {"opencode"}
    # opencode writes its project id to .git/opencode (inside .git, outside every
    # manifest); nothing in the worktree itself.
    workspace_state: list[str] = []
    # The config dir's @opencode-ai/plugin comes read-only from the seed (no install,
    # no network); an npm cache is pruned should one appear anyway.
    prune_paths = [".npm"]

    def home(self, home, em, snap, base_url):
        return homes.opencode_home(home, em, snap, base_url)

    def extra_ro(self, snap):
        return [(snap.opencode_models_path, homes.OPENCODE_MODELS_INSIDE)]

    def command(self, em, task, ws_inside, snap):
        cmd = ["opencode", "run", "-m", em.opencode, "--format", "json", "--dir", ws_inside, "--auto"]
        variants = snap.model(em.key).get("opencode_variants")
        if variants is None or em.effort in variants:
            cmd += ["--variant", em.effort]
        if task.is_plan:
            cmd += ["--agent", "plan"]
        return cmd + [task.prompt]

    def extract(self, stdout, home, task):
        return extract_opencode(read_ndjson(stdout))

    def plan_mode(self, task):
        return "--agent plan" if task.is_plan else None

    def seed_binds(self, home, seed):
        return homes.opencode_seed_binds(home, seed)
