"""``crazeeval captures``: the wire-capture report (plan 029 §3.1.10, AC-A8).

Per model, a comparison across harnesses of what each one sends: the system prompt
(bytes, sha256, the text saved beside the report), where the environment text sits
(system prompt or first user message), the offered tools with each description's
bytes, every request parameter other than the conversation and the tools, how many
replayed assistant turns carry their reasoning (counted per turn, chat and Responses
shapes), the role sequence, and the requested and served model ids. It is what
licenses lever L4.

It also runs the config-fidelity checks of AC-A8 before judging: reasoning-effort
parity across harnesses (or the recorded difference), opencode's GLM ``thinking``
flag, no web tool offered by any harness, one model per run (requested *and* served),
and no contamination hit in an accepted run. A check with no main request to read is
``no-evidence``, never true (review r1-c2 §9).

Held-out runs are read only with ``--unseal`` (their captures hold their answers).
"""

from __future__ import annotations

import hashlib
import json
import re
from collections import defaultdict
from itertools import groupby
from pathlib import Path

from crazeeval import capture as cap
from crazeeval.capture import content_text
from crazeeval.config import load_models
from crazeeval.packet import attempt_dir
from crazeeval.report import iter_results

CONVERSATION_KEYS = {"messages", "input", "tools", "instructions"}
DATE_RE = re.compile(r"\b20\d\d-\d\d-\d\d\b|\b(Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday)\b")
GIT_RE = re.compile(r"git status|current branch|\bbranch:|recent commits|## [\w./-]+\.\.\.", re.I)
OS_RE = re.compile(r"\blinux\b|\bdarwin\b|operating system|\bplatform\b", re.I)
WS_RE = re.compile(r"/sandbox/work/")
SHELL_RE = re.compile(r"/bin/(ba|z|da)?sh\b|\bshell\b", re.I)


# -- one request ------------------------------------------------------------------------------------


def _turns(req: dict) -> list[dict]:
    """A request's conversation: chat ``messages`` or Responses ``input`` items."""
    return [m for m in req.get("messages") or req.get("input") or [] if isinstance(m, dict)]


def system_text(req: dict) -> str:
    """Everything a request sends as its system prompt: ``instructions`` (Responses)
    and every system/developer message."""
    parts = []
    if isinstance(req.get("instructions"), str):
        parts.append(req["instructions"])
    parts += [content_text(m.get("content")) for m in _turns(req) if m.get("role") in ("system", "developer")]
    return "\n\n".join(p for p in parts if p)


def first_user_text(req: dict) -> str:
    return next((content_text(m.get("content")) for m in _turns(req) if m.get("role") == "user"), "")


def tool_descriptions(req: dict) -> list[dict]:
    out = []
    for t in req.get("tools") or []:
        if not isinstance(t, dict):
            continue
        fn = t.get("function") if isinstance(t.get("function"), dict) else t
        name = fn.get("name") or t.get("type") or "?"
        desc = fn.get("description") or ""
        params = fn.get("parameters") or fn.get("input_schema") or fn.get("format")
        out.append({"name": name, "description_bytes": len(desc.encode()),
                    "schema_bytes": len(json.dumps(params, sort_keys=True).encode()) if params else 0})
    return out


def params(req: dict) -> dict:
    return {k: v for k, v in req.items() if k not in CONVERSATION_KEYS}


RESPONSES_ASSISTANT_TYPES = {"reasoning", "function_call", "custom_tool_call", "local_shell_call", "web_search_call"}


def _assistant_item(it: dict) -> bool:
    """A Responses input item the model produced (part of an assistant turn)."""
    t = it.get("type")
    if t in RESPONSES_ASSISTANT_TYPES:
        return True
    return it.get("role") == "assistant" and t in (None, "message")


def replay_counts(req: dict) -> tuple[int, int]:
    """(assistant turns this request replays, how many of them carry their reasoning).

    Chat: each ``assistant`` message is a turn; it carries reasoning when it has a
    non-empty ``reasoning_content``, ``reasoning`` or ``reasoning_details``. Responses: a
    turn is a run of consecutive model-produced items (reasoning, messages, calls)
    between the user's and the tools' items; it carries reasoning when a ``reasoning``
    item is among them."""
    msgs = req.get("messages")
    if isinstance(msgs, list):
        asst = [m for m in msgs if isinstance(m, dict) and m.get("role") == "assistant"]
        return len(asst), sum(1 for m in asst if m.get("reasoning_content") or m.get("reasoning")
                              or m.get("reasoning_details"))
    items = req.get("input")
    turns = with_r = 0
    if isinstance(items, list):
        in_turn = has_r = False
        for it in (i for i in items if isinstance(i, dict)):
            if _assistant_item(it):
                if not in_turn:
                    in_turn, has_r = True, False
                    turns += 1
                if it.get("type") == "reasoning" and not has_r:
                    has_r = True
                    with_r += 1
            else:
                in_turn = False
    return turns, with_r


def role_sequence(req: dict) -> str:
    """The conversation's roles in order, a run of one role shown as ``role×n``."""
    runs = ((r, len(list(g))) for r, g in groupby(m.get("role") or m.get("type") or "?" for m in _turns(req)))
    return ", ".join(r if n == 1 else f"{r}×{n}" for r, n in runs)


def env_location(system: str, first_user: str) -> dict[str, str]:
    """Where each piece of environment text sits: system, user, both or none."""

    def where(rx) -> str:
        s, u = bool(rx.search(system or "")), bool(rx.search(first_user or ""))
        return "both" if s and u else "system" if s else "user" if u else "none"

    return {"workspace": where(WS_RE), "date": where(DATE_RE), "git": where(GIT_RE), "os": where(OS_RE),
            "shell": where(SHELL_RE)}


def effective_effort(eff: dict) -> str:
    """One comparable string from a request's reasoning controls."""
    if not eff:
        return "(none)"
    for k in ("reasoning_effort", "reasoning.effort"):
        if k in eff:
            return str(eff[k])
    if "thinking" in eff:
        return "thinking:" + json.dumps(eff["thinking"], sort_keys=True)
    return json.dumps(eff, sort_keys=True)


# -- one harness on one model -----------------------------------------------------------------------------


def harness_summary(reps: list[tuple[Path, dict]], out_dir: Path, model_key: str, harness: str) -> dict:
    """The capture comparison row for one (model, harness) over its runs (each a rep
    directory and its parsed result)."""
    prompts: dict[str, str] = {}
    first = None
    param_values: dict[str, list] = defaultdict(list)
    replay = {"requests_after_first": 0, "assistant_turns": 0, "with_reasoning": 0, "without_reasoning": 0,
              "last_request": {"assistant_turns": 0, "with_reasoning": 0}}
    served, req_models, efforts = set(), set(), []
    web = set()
    contamination = 0
    per_run: list[dict] = []
    glm_thinking = {"requests": 0, "with_flag": 0, "clear_thinking_false": 0}
    for rep, result in reps:
        records = cap.read_capture(attempt_dir(rep, result))
        main = [r for r in records if not r.get("refused") and isinstance(r.get("request"), dict)
                and cap.classify(r) == "main"]
        m = result.get("metrics") or cap.metrics(records)
        served.update(m.get("served_models") or [])
        req_models.update(m.get("request_models") or [])
        web.update(m.get("web_tools_offered") or [])
        run_efforts = []
        for e in m.get("main_efforts") or []:
            s = effective_effort(e)
            if s not in efforts:
                efforts.append(s)
            if s not in run_efforts:
                run_efforts.append(s)
        # The per-run evidence the fidelity checks read (review r1-c2 §9).
        per_run.append({
            "run": str(rep),
            "main_requests": len(main),
            "request_models": sorted(m.get("request_models") or []),
            "served_models": sorted(m.get("served_models") or []),
            "model_refusals": int((m.get("refused") or {}).get("model") or 0),
            "efforts": run_efforts,
            "web_tools_offered": sorted(m.get("web_tools_offered") or []),
        })
        if result.get("status") == "ok" and result.get("contamination"):
            contamination += 1
        for i, r in enumerate(main):
            req = r["request"]
            st = system_text(req)
            sha = hashlib.sha256(st.encode()).hexdigest()
            prompts.setdefault(sha, st)
            for k, v in params(req).items():
                if v not in param_values[k] and len(param_values[k]) < 8:
                    param_values[k].append(v)
            if i > 0:
                replay["requests_after_first"] += 1
                turns, with_r = replay_counts(req)
                replay["assistant_turns"] += turns
                replay["with_reasoning"] += with_r
                replay["without_reasoning"] += turns - with_r
                if i == len(main) - 1:
                    replay["last_request"] = {"assistant_turns": turns, "with_reasoning": with_r}
            th = req.get("thinking")
            glm_thinking["requests"] += 1
            if isinstance(th, dict):
                glm_thinking["with_flag"] += 1
                if th.get("clear_thinking") is False:
                    glm_thinking["clear_thinking_false"] += 1
            if first is None:
                first = {"run": str(rep), "request": req, "last": main[-1]["request"], "system": st, "sha": sha}
    if first is None:
        return {"harness": harness, "runs": len(reps), "main_requests": 0, "per_run": per_run,
                "contaminated_accepted_runs": contamination}
    req, st = first["request"], first["system"]
    pdir = out_dir / "prompts" / model_key.replace("/", "_")
    pdir.mkdir(parents=True, exist_ok=True)
    pfile = pdir / f"{harness}.system.txt"
    pfile.write_text(st)
    tools = tool_descriptions(req)
    return {
        "harness": harness,
        "runs": len(reps),
        "main_requests": sum(r["main_requests"] for r in per_run),
        "runs_without_main_requests": sum(1 for r in per_run if not r["main_requests"]),
        "per_run": per_run,
        "sample_run": first["run"],
        "system_prompt": {"bytes": len(st.encode()), "sha256": first["sha"], "file": str(pfile),
                          "distinct_across_runs": len(prompts)},
        "environment": env_location(st, first_user_text(req)),
        "tools": {"count": len(tools), "description_bytes_total": sum(t["description_bytes"] for t in tools),
                  "each": tools},
        "params": {k: v for k, v in sorted(param_values.items())},
        "reasoning_replay": replay,
        "role_sequence": role_sequence(first["last"]),
        "served_models": sorted(served),
        "request_models": sorted(req_models),
        "effective_efforts": efforts,
        "web_tools_offered": sorted(web),
        "thinking_flag": glm_thinking,
        "contaminated_accepted_runs": contamination,
    }


# -- the report -----------------------------------------------------------------------------------------------


def rep_results(batch: Path, unseal: bool) -> dict[tuple[str, str], list[tuple[Path, dict]]]:
    """(model slug, harness) -> its (rep directory, result) pairs."""
    out: dict[tuple[str, str], list[tuple[Path, dict]]] = defaultdict(list)
    for root, rep, result in iter_results(batch, unseal):
        harness, slug = rep.relative_to(root).parts[:2]
        out[(slug, harness)].append((rep, result))
    return out


NO_EVIDENCE = "no-evidence"


def _combine(values: list) -> bool | str:
    """False if any is False; else no-evidence if any is (or there are none); else True."""
    if any(v is False for v in values):
        return False
    if not values or any(v == NO_EVIDENCE for v in values):
        return NO_EVIDENCE
    return True


def _run_one_model(run: dict, wire_model: str | None) -> bool | str:
    """One run named only its target model and was served only that model."""
    if not run["main_requests"]:
        return NO_EVIDENCE
    if run["model_refusals"] or (wire_model and run["request_models"] != [wire_model]):
        return False
    if not run["served_models"]:
        return NO_EVIDENCE  # nothing reported which model served it
    return not wire_model or run["served_models"] == [wire_model]


def fidelity(model_key: str, rows: list[dict], pinned_effort: str | None, wire_model: str | None) -> dict:
    """AC-A8 for one model. Every check needs main-request evidence from every run of
    every harness: a run with no main request makes a check ``no-evidence`` (review
    r1-c2 §9), and a failing run makes it false."""
    runs = [(r["harness"], run) for r in rows for run in r.get("per_run") or []]
    covered = {r["harness"]: bool(r.get("per_run")) and all(x["main_requests"] for x in r["per_run"]) for r in rows}
    eff = {r["harness"]: (r.get("effective_efforts") if covered[r["harness"]] else NO_EVIDENCE) for r in rows}
    if not rows or not all(covered.values()):
        parity: bool | str = NO_EVIDENCE
    else:
        parity = len({json.dumps(v) for v in eff.values()}) <= 1
    thinking: bool | str | None = None
    oc = next((r for r in rows if r["harness"] == "opencode"), None)
    if "glm" in model_key and oc is not None:
        tf = oc.get("thinking_flag") or {"requests": 0, "with_flag": 0}
        thinking = NO_EVIDENCE if not tf["requests"] or not covered["opencode"] else tf["with_flag"] == tf["requests"]
    web = [False if run["web_tools_offered"] else (True if run["main_requests"] else NO_EVIDENCE) for _, run in runs]
    return {
        "pinned_effort": pinned_effort,
        "main_request_coverage": {h: ok for h, ok in covered.items()},
        "effort_by_harness": eff,
        "effort_parity": parity,
        "opencode_glm_thinking_flag": thinking,
        "web_tools_absent": _combine(web),
        "one_model_per_run": _combine([_run_one_model(run, wire_model) for _, run in runs]),
        "one_model_failures": [f"{h}: {run['run']}" for h, run in runs if _run_one_model(run, wire_model) is False][:10],
        "no_contamination_in_accepted_runs": all(not r.get("contaminated_accepted_runs") for r in rows),
    }


def build(batch: Path, out_dir: Path, unseal: bool = False) -> dict:
    models = load_models()
    by_slug = {em.slug: em for em in models.values()}
    by_model: dict[str, list[dict]] = defaultdict(list)
    for (slug, harness), reps in sorted(rep_results(batch, unseal).items()):
        em = by_slug.get(slug)
        key = em.key if em else slug
        by_model[key].append(harness_summary(reps, out_dir, key, harness))
    report = {"batch": str(batch), "sealed": not unseal, "models": {}}
    for key, rows in sorted(by_model.items()):
        em = models.get(key)
        report["models"][key] = {"harnesses": rows,
                                 "fidelity": fidelity(key, rows, em.effort if em else None, em.wire_model if em else None)}
    return report


def render_markdown(rep: dict) -> str:
    L = ["# Wire-capture report (§3.1.10)", "", f"Batch: {rep['batch']}" + (" (held-out runs sealed)" if rep["sealed"] else ""), ""]
    for model, block in rep["models"].items():
        L += [f"## {model}", ""]
        f = block["fidelity"]
        parity = {True: "yes", False: "**no**"}.get(f["effort_parity"], f"**{f['effort_parity']}**")
        L += [f"- effort parity: {parity} (pinned {f['pinned_effort']}; "
              f"{', '.join(f'{h}: {v}' for h, v in f['effort_by_harness'].items())})",
              f"- web tools absent: {f['web_tools_absent']}; one model per run: {f['one_model_per_run']}; "
              f"no contamination in accepted runs: {f['no_contamination_in_accepted_runs']}"]
        if f["opencode_glm_thinking_flag"] is not None:
            L.append(f"- opencode's GLM thinking flag on every request: {f['opencode_glm_thinking_flag']}")
        L += ["", "| harness | prompt bytes | sha256 | env (ws/date/git/os/shell) | tools | desc bytes | "
                  "replayed turns with reasoning | served |",
              "|---|---|---|---|---|---|---|---|"]
        for r in block["harnesses"]:
            if not r.get("system_prompt"):
                L.append(f"| {r['harness']} | – | – | – | – | – | – | – |")
                continue
            env = r["environment"]
            rr = r["reasoning_replay"]
            L.append(f"| {r['harness']} | {r['system_prompt']['bytes']} | {r['system_prompt']['sha256'][:12]} | "
                     f"{env['workspace']}/{env['date']}/{env['git']}/{env['os']}/{env['shell']} | {r['tools']['count']} | "
                     f"{r['tools']['description_bytes_total']} | {rr['with_reasoning']} of {rr['assistant_turns']} turns | "
                     f"{', '.join(r['served_models'])} |")
        L.append("")
        for r in block["harnesses"]:
            if not r.get("params"):
                continue
            L.append(f"**{r['harness']}** params: " + "; ".join(f"`{k}`={json.dumps(v)[:120]}" for k, v in r["params"].items()))
            L.append(f"roles: {r['role_sequence']}")
            L.append("tools: " + ", ".join(f"{t['name']} ({t['description_bytes']} B)" for t in r["tools"]["each"]))
            L.append("")
    return "\n".join(L) + "\n"


def write(batch: Path, out_dir: Path | None = None, unseal: bool = False) -> Path:
    out_dir = Path(out_dir or Path(batch) / "captures")
    out_dir.mkdir(parents=True, exist_ok=True)
    rep = build(Path(batch), out_dir, unseal)
    (out_dir / "captures.json").write_text(json.dumps(rep, indent=2, default=str) + "\n")
    (out_dir / "captures.md").write_text(render_markdown(rep))
    return out_dir
