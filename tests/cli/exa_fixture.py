"""A loopback MCP endpoint standing in for Exa's hosted one, for the native
web tools' CLI tests (plan 051 C4).

craze's web caller (internal/harness/websearch/mcp.go) sends one JSON-RPC
`tools/call` POST per web call -- no initialize, no session -- and reads a
text/event-stream reply whose `data:` line is the JSON-RPC response. This
server answers `web_search_exa` and `web_fetch_exa` by replaying the captured
replies in internal/harness/websearch/testdata (search.body, fetch.body) with
the JSON-RPC id rewritten to the request's own, and records every request.
craze reads the endpoint from CRAZE_TEST_EXA_MCP (websearch.Override), which
accepts only an http loopback IP literal with a path: `url` is exactly that.

Stdlib only, like sse_fixture.py: a ThreadingHTTPServer on 127.0.0.1:0.
"""

from __future__ import annotations

import json
import threading
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

TESTDATA = Path(__file__).resolve().parents[2] / "internal" / "harness" / "websearch" / "testdata"

# Which captured reply answers which Exa tool.
REPLAYS = {"web_search_exa": "search.body", "web_fetch_exa": "fetch.body"}


@dataclass
class McpCall:
    """One request the fake saw: the JSON-RPC method, the tool and its arguments."""

    method: str
    tool: str
    arguments: dict
    headers: dict[str, str] = field(default_factory=dict)


class ExaFixture:
    def __init__(self) -> None:
        self.calls: list[McpCall] = []
        # fail makes every tools/call answer a JSON-RPC error instead of the replay.
        self.fail = False
        self._lock = threading.Lock()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: object) -> None:
                pass

            def do_POST(self) -> None:  # noqa: N802 (stdlib's naming)
                length = int(self.headers.get("Content-Length", "0") or "0")
                raw = self.rfile.read(length) if length else b""
                try:
                    req = json.loads(raw)
                except json.JSONDecodeError:
                    self.send_error(400)
                    return
                params = req.get("params") or {}
                call = McpCall(
                    str(req.get("method", "")),
                    str(params.get("name", "")),
                    params.get("arguments") or {},
                    {k.lower(): v for k, v in self.headers.items()},
                )
                with fixture._lock:
                    fixture.calls.append(call)
                    fail = fixture.fail
                replay = REPLAYS.get(call.tool)
                if call.method != "tools/call" or replay is None:
                    self.send_error(404)
                    return
                if fail:
                    env = {"jsonrpc": "2.0", "id": req.get("id"), "error": {"code": -32000, "message": "fixture failure"}}
                    payload = ("event: message\ndata: " + json.dumps(env) + "\n\n").encode()
                else:
                    payload = self._replay(replay, req.get("id"))
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            @staticmethod
            def _replay(name: str, rpc_id: object) -> bytes:
                out = []
                for line in (TESTDATA / name).read_text(encoding="utf-8").splitlines():
                    if line.startswith("data:"):
                        env = json.loads(line[len("data:"):])
                        env["id"] = rpc_id
                        line = "data: " + json.dumps(env)
                    out.append(line)
                return ("\n".join(out) + "\n\n").encode("utf-8")

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self._server.server_address[1]}/mcp"

    def start(self) -> "ExaFixture":
        self._thread.start()
        return self

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()

    def tools_called(self) -> list[str]:
        with self._lock:
            return [c.tool for c in self.calls if c.method == "tools/call"]
