"""The capture file: writing it, reading it, and deriving tool calls and metrics
from it (plan 029 §3.1.2, §3.1.3).

The capture is a journal (review r1-c1 finding 9). A forwarded request is written
as it happens: a ``start`` line (the request) flushed before it goes upstream, an
``events`` line for each batch of streamed SSE events as they pass, and an ``end``
line with the status, usage and accounting. A refused request is one ``record`` line.
:func:`read_capture` reassembles one record per request; a request whose ``end``
never came (a killed proxy) is still there, marked ``incomplete``. The proxy writes
plain JSONL while a run is live and gzips it when the run ends. Every line is
scrubbed of every loaded key before it is written.
"""

from __future__ import annotations

import gzip
import json
import os
import re
import shutil
from pathlib import Path
from typing import Any, Callable, Iterable

from crazeeval.pricing import Usage, usage_any, usage_from_chat, usage_from_responses

CAPTURE_PLAIN = "capture.jsonl"
CAPTURE_GZ = "capture.jsonl.gz"
CAPTURE_LATE = "capture.late.jsonl"
EVENTS_PER_LINE = 256

# -- writing -----------------------------------------------------------------


class CaptureWriter:
    """Append-only JSONL, gzipped into ``capture.jsonl.gz`` on close."""

    def __init__(self, path: Path, scrub: Callable[[str], str]):
        self.path = Path(path)
        self.gz_path = self.path.with_name(self.path.name + ".gz")
        self._scrub = scrub
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = open(self.path, "a", encoding="utf-8")
        self.closed = False

    def write(self, record: dict) -> None:
        """One journal line (flushed at once)."""
        line = self._scrub(json.dumps(record, ensure_ascii=False, default=str))
        if self.closed:
            # A late record (a request still finishing when the run ended) goes
            # beside the gzip rather than being lost.
            with open(self.path.with_name(CAPTURE_LATE), "a", encoding="utf-8") as f:
                f.write(line + "\n")
            return
        self._fh.write(line + "\n")
        self._fh.flush()

    def events(self, seq: int, events: list) -> None:
        for i in range(0, len(events), EVENTS_PER_LINE):
            self.write({"kind": "events", "seq": seq, "events": events[i : i + EVENTS_PER_LINE]})

    def close(self) -> None:
        if self.closed:
            return
        self.closed = True
        self._fh.flush()
        os.fsync(self._fh.fileno())
        self._fh.close()
        tmp = self.gz_path.with_name(self.gz_path.name + ".tmp")
        with open(self.path, "rb") as src, gzip.open(tmp, "wb") as dst:
            shutil.copyfileobj(src, dst)
        os.replace(tmp, self.gz_path)
        os.unlink(self.path)


# -- SSE -----------------------------------------------------------------------


ACCOUNTING_TYPES = {"response.created", "response.completed", "response.incomplete", "response.failed", "response.done"}
SEP = re.compile(rb"\r?\n\r?\n")


def _accounting(obj: Any) -> bool:
    """An event the proxy needs for usage and the served model, kept past any bound."""
    return isinstance(obj, dict) and (obj.get("usage") is not None or obj.get("type") in ACCOUNTING_TYPES)


class SSEParser:
    """Incremental ``text/event-stream`` parser keeping each event's parsed ``data``,
    bounded by bytes as well as events (review r2-c1 item 8):

    - an event longer than ``max_event_bytes`` is skipped up to its separator (the
      buffer never holds more than that);
    - past ``max_stored_bytes`` of kept event data, only accounting events (usage, the
      served model) are kept, so reconciliation still works.
    """

    def __init__(self, limit_events: int = 200_000, max_event_bytes: int = 16 * 1024 * 1024,
                 max_stored_bytes: int = 64 * 1024 * 1024):
        self._buf = b""
        self.events: list[Any] = []
        self.done = False
        self._limit = limit_events
        self._max_event = max_event_bytes
        self._max_stored = max_stored_bytes
        self._skipping = False
        self.stored_bytes = 0
        self.dropped = 0
        self.dropped_bytes = 0
        # Past the limit, an accounting event replaces the slot already kept for its
        # ``type`` (or the untyped chat shape) instead of appending -- a stream of
        # repeated small usage events cannot grow this past a handful of entries.
        self._acc_slots: dict[Any, int] = {}

    def feed(self, chunk: bytes) -> None:
        scan_from = max(0, len(self._buf) - 3)
        self._buf += chunk
        while True:
            m = SEP.search(self._buf, scan_from)
            scan_from = 0
            if not m:
                break
            raw, self._buf = self._buf[: m.start()], self._buf[m.end() :]
            if self._skipping:
                self._skipping = False  # the oversized event ends here
                self.dropped_bytes += len(raw)
                continue
            self._event(raw)
        if len(self._buf) > self._max_event:
            self.dropped_bytes += len(self._buf) - 3
            if not self._skipping:
                self.dropped += 1
            self._skipping = True
            self._buf = self._buf[-3:]  # a separator may straddle the cut

    def close(self) -> None:
        if self._buf.strip() and not self._skipping:
            self._event(self._buf)
        self._buf = b""

    def _event(self, raw: bytes) -> None:
        data_lines = []
        for line in raw.splitlines():
            if line.startswith(b"data:"):
                d = line[5:]
                if d.startswith(b" "):
                    d = d[1:]
                data_lines.append(d)
        if not data_lines:
            return
        data = b"\n".join(data_lines).decode("utf-8", errors="replace")
        if data.strip() == "[DONE]":
            self.done = True
            return
        try:
            obj = json.loads(data)
        except ValueError:
            obj = {"_raw": data[:10_000]}
        over = len(self.events) >= self._limit or self.stored_bytes + len(data) > self._max_stored
        if over:
            if not _accounting(obj):
                self.dropped += 1
                self.dropped_bytes += len(data)
                return
            # Bounded accounting state: the latest event of this shape replaces the
            # one already kept for it, so usage/model reconciliation still works
            # without the list (or stored_bytes) growing with every repeat.
            key = obj.get("type") if isinstance(obj.get("type"), str) else None
            idx = self._acc_slots.get(key)
            if idx is not None and idx < len(self.events):
                self.events[idx] = obj
            else:
                self._acc_slots[key] = len(self.events)
                self.events.append(obj)
            return
        self.events.append(obj)
        self.stored_bytes += len(data)


# -- reading -------------------------------------------------------------------


def capture_path(run_dir: Path) -> Path | None:
    for name in (CAPTURE_GZ, CAPTURE_PLAIN):
        p = Path(run_dir) / name
        if p.exists():
            return p
    return None


def _read_jsonl(f, out: list[dict]) -> None:
    """Binary lines, each record decoded on its own (review r2-c1 item 9): a record cut
    anywhere -- mid-JSON, mid-UTF-8 character -- by a killed proxy is skipped, every
    earlier one kept; a truncated gzip keeps what decompressed."""
    try:
        for raw in f:
            line = raw.strip()
            if not line:
                continue
            try:
                out.append(json.loads(line.decode("utf-8")))
            except ValueError:  # includes UnicodeDecodeError
                pass
    except (EOFError, OSError):
        pass


def assemble(lines: list[dict]) -> list[dict]:
    """Journal lines -> one record per request, in the order requests started."""
    recs: dict = {}
    order: list = []
    for obj in lines:
        if not isinstance(obj, dict):
            continue
        kind = obj.get("kind")
        if kind is None or kind == "record":
            key = ("record", len(order))
            recs[key] = {k: v for k, v in obj.items() if k != "kind"}
            order.append(key)
            continue
        key = ("seq", obj.get("seq"))
        if key not in recs:
            recs[key] = {"seq": obj.get("seq"), "incomplete": True}
            order.append(key)
        r = recs[key]
        if kind == "start":
            r.update({k: v for k, v in obj.items() if k != "kind"})
        elif kind == "events":
            r.setdefault("response", {}).setdefault("events", []).extend(obj.get("events") or [])
        elif kind == "end":
            r.update({k: v for k, v in obj.items() if k not in ("kind", "response")})
            r.setdefault("response", {}).update(obj.get("response") or {})
            r.pop("incomplete", None)
    return [recs[k] for k in order]


def read_capture(path_or_dir: Path) -> list[dict]:
    p = Path(path_or_dir)
    if p.is_dir():
        found = capture_path(p)
        if found is None:
            return []
        p = found
    opener = gzip.open if p.suffix == ".gz" else open
    out: list[dict] = []
    with opener(p, "rb") as f:
        _read_jsonl(f, out)
    late = p.with_name(CAPTURE_LATE)
    if late.exists():
        with open(late, "rb") as f:
            _read_jsonl(f, out)
    return assemble(out)


# -- response parsing ----------------------------------------------------------


def usage_and_model(response: dict | None) -> tuple[Usage | None, str | None]:
    """The reported usage and the served model of one captured response."""
    if not response:
        return None, None
    usage: Usage | None = None
    model: str | None = None
    if "events" in response:
        for ev in response["events"]:
            if not isinstance(ev, dict):
                continue
            t = ev.get("type")
            if isinstance(t, str) and t.startswith("response."):
                r = ev.get("response")
                if isinstance(r, dict):
                    model = r.get("model") or model
                    if t in ("response.completed", "response.incomplete", "response.failed", "response.done"):
                        usage = usage_from_responses(r.get("usage")) or usage
                continue
            if ev.get("model"):
                model = model or ev.get("model")
            u = usage_from_chat(ev.get("usage"))
            if u is not None:
                usage = u
        return usage, model
    body = response.get("body")
    if isinstance(body, dict):
        model = body.get("model")
        usage = usage_any(body.get("usage"))
    return usage, model


def _parse_args(s: Any) -> Any:
    if isinstance(s, str):
        try:
            return json.loads(s)
        except ValueError:
            return s
    return s


def response_tool_calls(response: dict | None) -> list[dict]:
    """Tool calls the model made in one captured response, in order."""
    if not response:
        return []
    calls: list[dict] = []
    if "events" in response:
        events = [e for e in response["events"] if isinstance(e, dict)]
        if any(isinstance(e.get("type"), str) and e["type"].startswith("response.") for e in events):
            items = [e.get("item") for e in events if e.get("type") == "response.output_item.done"]
            items = [i for i in items if isinstance(i, dict)]
            if not items:
                for e in events:
                    if e.get("type") in ("response.completed", "response.incomplete", "response.done"):
                        r = e.get("response") or {}
                        items = [i for i in (r.get("output") or []) if isinstance(i, dict)]
            calls.extend(_responses_items(items))
            return calls
        acc: dict[tuple[int, int], dict] = {}
        order: list[tuple[int, int]] = []
        for e in events:
            for ch in e.get("choices") or []:
                if not isinstance(ch, dict):
                    continue
                ci = ch.get("index", 0) or 0
                delta = ch.get("delta") or ch.get("message") or {}
                for tc in delta.get("tool_calls") or []:
                    if not isinstance(tc, dict):
                        continue
                    idx = tc.get("index", len(order))
                    k = (ci, idx if isinstance(idx, int) else 0)
                    if k not in acc:
                        acc[k] = {"id": None, "name": "", "arguments": ""}
                        order.append(k)
                    cur = acc[k]
                    if tc.get("id"):
                        cur["id"] = tc["id"]
                    fn = tc.get("function") or {}
                    if fn.get("name"):
                        cur["name"] = cur["name"] or fn["name"]
                    if isinstance(fn.get("arguments"), str):
                        cur["arguments"] += fn["arguments"]
        for k in order:
            c = acc[k]
            calls.append({"id": c["id"], "name": c["name"], "arguments": _parse_args(c["arguments"])})
        return calls
    body = response.get("body")
    if isinstance(body, dict):
        if isinstance(body.get("output"), list):
            return _responses_items([i for i in body["output"] if isinstance(i, dict)])
        for ch in body.get("choices") or []:
            msg = (ch or {}).get("message") or {}
            for tc in msg.get("tool_calls") or []:
                fn = (tc or {}).get("function") or {}
                calls.append(
                    {"id": tc.get("id"), "name": fn.get("name", ""), "arguments": _parse_args(fn.get("arguments"))}
                )
    return calls


def _responses_items(items: Iterable[dict]) -> list[dict]:
    out = []
    for it in items:
        t = it.get("type")
        cid = it.get("call_id") or it.get("id")
        if t == "function_call":
            out.append({"id": cid, "name": it.get("name", ""), "arguments": _parse_args(it.get("arguments"))})
        elif t == "custom_tool_call":
            out.append({"id": cid, "name": it.get("name", ""), "arguments": it.get("input")})
        elif t == "local_shell_call":
            action = it.get("action") or {}
            out.append({"id": cid, "name": "local_shell", "arguments": {"command": action.get("command")}})
        elif isinstance(t, str) and t.endswith("_call"):
            # Server-side tools (web_search_call, ...): recorded by type.
            out.append({"id": it.get("id"), "name": t, "arguments": {k: v for k, v in it.items() if k not in ("id", "type")}})
    return out


# -- request parsing -----------------------------------------------------------


def offered_tools(request: Any) -> list[str]:
    if not isinstance(request, dict):
        return []
    names = []
    for t in request.get("tools") or []:
        if not isinstance(t, dict):
            continue
        fn = t.get("function")
        if isinstance(fn, dict) and fn.get("name"):
            names.append(fn["name"])
        elif t.get("name"):
            names.append(t["name"])
        elif t.get("type"):
            names.append(str(t["type"]))
    return names


def request_effort(request: Any) -> dict:
    """The reasoning controls a request carries on the wire."""
    if not isinstance(request, dict):
        return {}
    out = {}
    if "reasoning_effort" in request:
        out["reasoning_effort"] = request["reasoning_effort"]
    r = request.get("reasoning")
    if isinstance(r, dict):
        for k in ("effort", "summary"):
            if k in r:
                out[f"reasoning.{k}"] = r[k]
    if "thinking" in request:
        out["thinking"] = request["thinking"]
    return out


WEB_TOOL_RE = re.compile(r"web[_-]?(search|fetch)|webfetch|websearch|codesearch|^fetch$|browser|url_fetch", re.I)


def web_tools(names: Iterable[str]) -> list[str]:
    return sorted({n for n in names if WEB_TOOL_RE.search(n)})


AUX_PROMPT_RE = re.compile(
    r"title generator|generate a (short |concise )?title|a title for (this|the) (conversation|session)|"
    r"Summarize this conversation so it can continue|summari[sz]e (the|this) conversation|"
    r"create a detailed summary of the conversation|generating the session title",
    re.I,
)


def classify(record: dict, calls: list[dict] | None = None) -> str:
    """``main`` or ``aux`` by request and response shape (§3.1.3).

    Aux: a known title/summary prompt, or no tools offered and a short single
    completion with no tool calls. Tools alone never decide it (craze's summarizer
    sends inert tools). ``calls``: the response's tool calls, when already parsed.
    """
    req = record.get("request")
    if not isinstance(req, dict):
        return "main"
    last_user = ""
    msgs = req.get("messages") or []
    for m in reversed(msgs):
        if isinstance(m, dict) and m.get("role") == "user":
            c = m.get("content")
            last_user = c if isinstance(c, str) else json.dumps(c)[:4000]
            break
    system = ""
    for m in msgs:
        if isinstance(m, dict) and m.get("role") == "system" and isinstance(m.get("content"), str):
            system = m["content"]
            break
    if AUX_PROMPT_RE.search(last_user[:4000]) or AUX_PROMPT_RE.search(system[:2000]):
        return "aux"
    if isinstance(req.get("instructions"), str) and AUX_PROMPT_RE.search(req["instructions"][:2000]):
        return "aux"
    if calls is None:
        calls = response_tool_calls(record.get("response"))
    usage = record.get("usage") or {}
    if not offered_tools(req) and not calls and int(usage.get("output") or 0) < 400:
        return "aux"
    return "main"


# -- tool-call classification ----------------------------------------------------

EXEC_TOOLS = {
    "bash", "shell", "exec_command", "local_shell", "run_terminal_cmd", "run_command", "terminal",
    "exec", "execute_command", "unified_exec", "shell_command", "run_shell_command", "run", "run_terminal_command",
}
WRITE_TOOLS = {
    "write", "edit", "multiedit", "patch", "apply_patch", "write_file", "edit_file", "create_file",
    "str_replace", "search_replace", "str_replace_editor", "str_replace_based_edit_tool", "file_edit",
    "file_write", "replace", "insert", "notebook_edit",
}
COMMAND_KEYS = ("command", "cmd", "script", "commands", "input")


def call_command(call: dict) -> str:
    args = call.get("arguments")
    if isinstance(args, str):
        return args
    if isinstance(args, dict):
        for k in COMMAND_KEYS:
            v = args.get(k)
            if isinstance(v, list):
                return " ".join(str(x) for x in v)
            if isinstance(v, str):
                return v
    return ""


def call_text(call: dict) -> str:
    args = call.get("arguments")
    return args if isinstance(args, str) else json.dumps(args, ensure_ascii=False)


def is_exec(call: dict) -> bool:
    return (call.get("name") or "").lower() in EXEC_TOOLS


def is_write(call: dict) -> bool:
    return (call.get("name") or "").lower() in WRITE_TOOLS


def all_tool_calls(records: list[dict]) -> list[dict]:
    out = []
    for i, r in enumerate(records):
        for c in response_tool_calls(r.get("response")):
            c = dict(c)
            c["request_seq"] = r.get("seq", i)
            out.append(c)
    return out


def content_text(c) -> str:
    """A message's (or a tool result's) content as text: a string as is, a parts list
    joined by lines."""
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        parts = []
        for p in c:
            if isinstance(p, dict):
                parts.append(str(p.get("text") or p.get("output") or p.get("content") or ""))
            else:
                parts.append(str(p))
        return "\n".join(parts)
    if c is None:
        return ""
    return json.dumps(c, ensure_ascii=False)


def tool_results(records: list[dict]) -> dict[str, str]:
    """call id -> the result text the harness sent back to the model (the first time):
    a chat ``tool`` message or a Responses ``*_call_output`` item in a later request.
    A call with no entry here never had its result sent -- it was only attempted."""
    out: dict[str, str] = {}
    for r in records:
        req = r.get("request")
        if not isinstance(req, dict):
            continue
        for m in req.get("messages") or []:
            if isinstance(m, dict) and m.get("role") == "tool" and m.get("tool_call_id"):
                out.setdefault(m["tool_call_id"], content_text(m.get("content")))
        items = req.get("input")
        if isinstance(items, list):
            for it in items:
                if isinstance(it, dict) and str(it.get("type", "")).endswith("_call_output") and it.get("call_id"):
                    out.setdefault(it["call_id"], content_text(it.get("output")))
    return out


# -- tool kinds ---------------------------------------------------------------------------

# Every tool name the four harnesses offer (read from the C1/C2 captures: craze's opencode
# profile, gx, opencode, codex) and a few they may call, by kind. The judge's packet shows
# the kind, never the name: the names alone would tell it which harness a side is
# (review r1-c2 §5). An unknown name is "other".
TOOL_KINDS: dict[str, tuple[str, ...]] = {
    "shell": ("bash", "shell", "exec_command", "local_shell", "run_terminal_cmd", "run_terminal_command",
              "run_command", "terminal", "exec", "execute_command", "unified_exec", "shell_command",
              "run_shell_command"),
    "shell-input": ("write_stdin",),
    "read": ("read", "read_file", "view", "view_file", "open_file", "cat", "view_image"),
    "edit": ("edit", "multiedit", "patch", "apply_patch", "search_replace", "str_replace", "str_replace_editor",
             "str_replace_based_edit_tool", "edit_file", "file_edit", "replace", "insert", "notebook_edit"),
    "write": ("write", "write_file", "create_file", "file_write"),
    "search": ("grep", "glob", "search", "codesearch", "find", "find_files", "search_files", "ripgrep", "rg"),
    "list": ("list", "ls", "list_dir", "list_directory", "list_files"),
    "todo": ("todo_write", "todowrite", "todoread", "todo_read", "update_plan", "write_todos", "todo"),
    "delegate": ("agent", "task", "spawn_subagent", "spawn_agent", "multi_agent_v1", "subagent", "send_input"),
    "delegate-output": ("agent_output", "get_command_or_subagent_output", "wait", "kill_command_or_subagent"),
    "plan": ("exit_plan_mode", "enter_plan_mode", "plan_exit", "plan_enter"),
    "ask": ("ask_user_question", "question", "request_user_input"),
    "skill": ("skill",),
    "title": ("session_title",),
    "goal": ("create_goal", "get_goal", "update_goal"),
    "schedule": ("scheduler_create", "scheduler_delete", "scheduler_list", "monitor", "workflow"),
    "tool-search": ("search_tool", "use_tool"),
    "web": ("web_search", "websearch", "webfetch", "web_fetch", "fetch", "web_search_call"),
}
_KIND_OF = {name: kind for kind, names in TOOL_KINDS.items() for name in names}


def tool_kind(name: str | None) -> str:
    """The harness-neutral kind of a tool name (``other`` when unknown)."""
    return _KIND_OF.get((name or "").lower(), "other")


# -- metrics ---------------------------------------------------------------------


def metrics(records: list[dict]) -> dict:
    """Per-run numbers from the capture (§3.1.3)."""
    m = {
        "requests": 0,
        "forwarded": 0,
        "main_requests": 0,
        "aux_requests": 0,
        "refused": {},
        "statuses": {},
        "tool_calls": 0,
        "exec_calls": 0,
        "write_calls": 0,
        "tokens": {"input": 0, "cached": 0, "output": 0, "reasoning": 0},
        "cost": 0.0,
        "list_cost": 0.0,
        "usage_missing": 0,
        "usage_injected": 0,
        "served_models": [],
        "request_models": [],
        "offered_tools": [],
        "web_tools_offered": [],
        "efforts": [],
        "main_efforts": [],
        "labels": [],
        "streamed_tool_call": False,
    }
    served, req_models, tools, efforts, main_efforts = set(), set(), set(), [], []
    for r in records:
        m["requests"] += 1
        if r.get("refused"):
            m["refused"][r["refused"]] = m["refused"].get(r["refused"], 0) + 1
            continue
        m["forwarded"] += 1
        st = str(r.get("status"))
        m["statuses"][st] = m["statuses"].get(st, 0) + 1
        req = r.get("request")
        eff = None
        if isinstance(req, dict):
            if req.get("model"):
                req_models.add(req["model"])
            tools.update(offered_tools(req))
            eff = request_effort(req)
            if eff and eff not in efforts:
                efforts.append(eff)
        if r.get("usage_injected"):
            m["usage_injected"] += 1
        calls = response_tool_calls(r.get("response"))
        label = classify(r, calls)
        m["labels"].append(label)
        m[f"{label}_requests"] += 1
        if label == "main" and eff is not None and eff not in main_efforts:
            main_efforts.append(eff)
        m["tool_calls"] += len(calls)
        m["exec_calls"] += sum(1 for c in calls if is_exec(c))
        m["write_calls"] += sum(1 for c in calls if is_write(c))
        if calls and "events" in (r.get("response") or {}):
            m["streamed_tool_call"] = True
        u = r.get("usage")
        if u:
            for k in m["tokens"]:
                m["tokens"][k] += int(u.get(k) or 0)
        else:
            m["usage_missing"] += 1
        m["cost"] += float(r.get("cost") or 0.0)
        m["list_cost"] += float(r.get("list_cost") or 0.0)
        if r.get("served_model"):
            served.add(r["served_model"])
    m["cost"] = round(m["cost"], 6)
    m["list_cost"] = round(m["list_cost"], 6)
    m["served_models"] = sorted(served)
    m["request_models"] = sorted(req_models)
    m["offered_tools"] = sorted(tools)
    m["web_tools_offered"] = web_tools(tools)
    m["efforts"] = efforts
    # The effective reasoning controls of the model's main requests (effort parity).
    m["main_efforts"] = main_efforts
    return m
