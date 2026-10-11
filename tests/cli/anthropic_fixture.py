"""A loopback Anthropic API for the native harness's CLI tests (plan 044 §3.6, A6).

craze's anthropic driver talks to Anthropic through its own Messages API client
(internal/harness/llm/messagesapi), and `craze auth login anthropic` and a
native session's open fetch the account's model list through the same client.
The compiled binary reaches this fixture only through the test override,
CRAZE_TEST_ANTHROPIC_API (ANTHROPIC_ENV), which craze honours for a loopback
http URL alone and refuses for anything else -- so a test that misnames it fails
rather than reaching Anthropic. conftest.py fences the variable for every test,
and a case that wants this fixture sets it in its child's environment.

It is a stdlib http.server on 127.0.0.1:0, as sse_fixture.py is, serving:

- GET /v1/models: the account's list, paginated by page_size (has_more,
  last_id, after_id), in the API's shape (capabilities included), so the client
  follows its pages as it would Anthropic's;
- POST /v1/messages: one scripted Reply per request, streamed as the Messages
  API streams it -- message_start, each block's start, deltas and stop (a
  thinking block's text and signature in fragments, a tool_use's input as
  partial JSON), a ping inside a block, message_delta and message_stop.

Every request is recorded: method, path, query, headers (lower-cased) and body,
raw and parsed. The modes that echo the key (P13) answer with the request's own
x-api-key in the message: "http_error" as a 401 body (both endpoints),
"sse_error" as an `error` event in a 200 stream (messages). ANTHROPIC_CANARY is
the key the cases use: obviously not a secret, distinct from sse_fixture's.
"""

from __future__ import annotations

import json
import threading
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

ANTHROPIC_ENV = "CRAZE_TEST_ANTHROPIC_API"
ANTHROPIC_CANARY = "sk-ant-canary-not-a-secret-0007"

# The beta a request carrying block_binding names (plan 044 §3.2).
BINDING_BETA = "thinking-binding-controls-2026-08-01"


def account_model(model_id: str, line: str, created: str, name: str = "") -> dict:
    """One entry of GET /v1/models as the API gives it, capabilities and all."""
    on = {"supported": True}
    return {
        "type": "model",
        "id": model_id,
        "display_name": name or model_id,
        "created_at": created,
        "line": line,
        "max_input_tokens": 1000000,
        "max_tokens": 128000,
        "capabilities": {
            "effort": {"supported": True, "low": on, "medium": on, "high": on, "xhigh": on, "max": on},
            "image_input": on,
            "thinking": {"supported": True, "types": {"enabled": {"supported": False}, "adaptive": on}},
        },
        "lifecycle": "active",
        "deprecated_at": None,
        "retires_at": None,
    }


# The account the fixture lists unless a case says otherwise: two seeds and a
# model no seed names, over two pages (page_size 2).
DEFAULT_MODELS = [
    account_model("claude-haiku-5-5", "haiku", "2026-10-07T18:00:00Z", "Claude Haiku 5.5"),
    account_model("claude-opus-5-5", "opus", "2026-09-21T16:24:00Z", "Claude Opus 5.5"),
    account_model("claude-opus-6", "opus", "2026-10-10T09:00:00Z", "Claude Opus 6"),
]


@dataclass(frozen=True)
class Thinking:
    """A signed thinking block: its text and its signature, each streamed in
    two fragments, so a client that kept only the last fragment would show."""

    text: str
    signature: str


@dataclass(frozen=True)
class Text:
    text: str


@dataclass(frozen=True)
class ToolUse:
    id: str
    name: str
    input: dict


@dataclass(frozen=True)
class Reply:
    """One whole response: its content blocks, in wire order, and why it stopped."""

    blocks: tuple = ()
    stop_reason: str = "end_turn"


@dataclass
class Recorded:
    method: str
    path: str
    query: dict[str, list[str]]
    headers: dict[str, str]
    raw: bytes
    body: dict | None

    @property
    def key(self) -> str:
        return self.headers.get("x-api-key", "")


def _event(kind: str, payload: dict) -> bytes:
    payload = {"type": kind, **payload}
    return f"event: {kind}\ndata: {json.dumps(payload, separators=(',', ':'))}\n\n".encode()


def _halves(s: str) -> list[str]:
    half = len(s) // 2
    return [frag for frag in (s[:half], s[half:]) if frag]


def _stream(model: str, n: int, reply: Reply) -> bytes:
    out = [
        _event(
            "message_start",
            {
                "message": {
                    "id": f"msg_{n:04d}",
                    "type": "message",
                    "role": "assistant",
                    "model": model,
                    "content": [],
                    "stop_reason": None,
                    "usage": {
                        "input_tokens": 12,
                        "output_tokens": 1,
                        "cache_read_input_tokens": 0,
                        "cache_creation_input_tokens": 0,
                    },
                }
            },
        )
    ]
    for i, block in enumerate(reply.blocks):
        if isinstance(block, Thinking):
            out.append(_event("content_block_start", {"index": i, "content_block": {"type": "thinking", "thinking": "", "signature": ""}}))
            out += [_event("content_block_delta", {"index": i, "delta": {"type": "thinking_delta", "thinking": f}}) for f in _halves(block.text)]
            out.append(_event("ping", {}))
            out += [_event("content_block_delta", {"index": i, "delta": {"type": "signature_delta", "signature": f}}) for f in _halves(block.signature)]
        elif isinstance(block, Text):
            out.append(_event("content_block_start", {"index": i, "content_block": {"type": "text", "text": ""}}))
            out += [_event("content_block_delta", {"index": i, "delta": {"type": "text_delta", "text": f}}) for f in _halves(block.text)]
        elif isinstance(block, ToolUse):
            out.append(_event("content_block_start", {"index": i, "content_block": {"type": "tool_use", "id": block.id, "name": block.name, "input": {}}}))
            raw = json.dumps(block.input, separators=(",", ":"))
            out += [_event("content_block_delta", {"index": i, "delta": {"type": "input_json_delta", "partial_json": f}}) for f in _halves(raw)]
        else:  # pragma: no cover - a test bug
            raise AssertionError(f"anthropic_fixture: unknown block {block!r}")
        out.append(_event("content_block_stop", {"index": i}))
    out.append(
        _event(
            "message_delta",
            {"delta": {"stop_reason": reply.stop_reason, "stop_sequence": None}, "usage": {"output_tokens": 20}},
        )
    )
    out.append(_event("message_stop", {}))
    return b"".join(out)


@dataclass
class AnthropicFixture:
    """A background Anthropic API: the account's model list and scripted
    Messages streams. models_mode and messages_mode are "ok", or a key-echo
    mode ("http_error"; "sse_error" for messages alone)."""

    models: list[dict] = field(default_factory=lambda: list(DEFAULT_MODELS))
    page_size: int = 2
    models_mode: str = "ok"
    messages_mode: str = "ok"
    host: str = "127.0.0.1"

    def __post_init__(self) -> None:
        self._lock = threading.Lock()
        self._script: list[Reply] = []
        self.requests: list[Recorded] = []
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: object) -> None:  # quiet
                pass

            def _record(self) -> Recorded:
                length = int(self.headers.get("Content-Length", "0") or "0")
                raw = self.rfile.read(length) if length else b""
                try:
                    body = json.loads(raw) if raw else None
                except json.JSONDecodeError:
                    body = None
                split = urlsplit(self.path)
                rec = Recorded(
                    self.command,
                    split.path,
                    parse_qs(split.query),
                    {k.lower(): v for k, v in self.headers.items()},
                    raw,
                    body,
                )
                with fixture._lock:
                    fixture.requests.append(rec)
                return rec

            def _send(self, status: int, content_type: str, payload: bytes) -> None:
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(payload)))
                self.send_header("request-id", "req_fixture")
                self.end_headers()
                self.wfile.write(payload)

            def _echo_error(self, rec: Recorded) -> None:
                # The leak a key-echoing error body is (P13): the request's
                # own key in the message. 401 is never retried.
                payload = json.dumps(
                    {
                        "type": "error",
                        "error": {"type": "authentication_error", "message": f"invalid x-api-key: {rec.key}"},
                        "request_id": "req_fixture",
                    }
                ).encode()
                self._send(401, "application/json", payload)

            def do_GET(self) -> None:  # noqa: N802 (stdlib's naming)
                rec = self._record()
                if rec.path != "/v1/models":
                    self._send(404, "application/json", b'{"type":"error","error":{"type":"not_found_error","message":"no such path"}}')
                    return
                if fixture.models_mode == "http_error":
                    self._echo_error(rec)
                    return
                ids = [m["id"] for m in fixture.models]
                after = rec.query.get("after_id", [""])[0]
                start = ids.index(after) + 1 if after in ids else 0
                page = fixture.models[start : start + fixture.page_size]
                payload = json.dumps(
                    {
                        "data": page,
                        "has_more": start + fixture.page_size < len(ids),
                        "first_id": page[0]["id"] if page else None,
                        "last_id": page[-1]["id"] if page else None,
                    }
                ).encode()
                self._send(200, "application/json", payload)

            def do_POST(self) -> None:  # noqa: N802
                rec = self._record()
                if rec.path != "/v1/messages":
                    self._send(404, "application/json", b'{"type":"error","error":{"type":"not_found_error","message":"no such path"}}')
                    return
                mode = fixture.messages_mode
                if mode == "http_error":
                    self._echo_error(rec)
                    return
                if mode == "sse_error":
                    # An error event in a 200 stream, before message_start,
                    # its message the request's key; invalid_request_error is
                    # never retried.
                    payload = _event(
                        "error", {"error": {"type": "invalid_request_error", "message": f"bad x-api-key {rec.key}"}}
                    )
                    self._send(200, "text/event-stream", payload)
                    return
                with fixture._lock:
                    n = sum(1 for r in fixture.requests if r.path == "/v1/messages")
                    reply = fixture._script.pop(0) if fixture._script else None
                if reply is None:
                    payload = json.dumps(
                        {
                            "type": "error",
                            "error": {"type": "invalid_request_error", "message": f"anthropic_fixture: no scripted reply for request {n}"},
                        }
                    ).encode()
                    self._send(400, "application/json", payload)
                    return
                model = (rec.body or {}).get("model", "")
                self._send(200, "text/event-stream", _stream(model, n, reply))

        self._server = ThreadingHTTPServer((self.host, 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    def start(self) -> "AnthropicFixture":
        self._thread.start()
        return self

    @property
    def base_url(self) -> str:
        """CRAZE_TEST_ANTHROPIC_API's value: the API's origin, no path."""
        return f"http://{self.host}:{self._server.server_address[1]}"

    def set_script(self, replies: list[Reply]) -> None:
        """Answer the Messages requests with replies, one each, in order; one
        past the end is a 400 naming it."""
        with self._lock:
            self._script = list(replies)

    def requests_to(self, path: str) -> list[Recorded]:
        with self._lock:
            return [r for r in self.requests if r.path == path]

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)
