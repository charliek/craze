"""A loopback OpenAI-compatible Chat Completions endpoint for the native
harness's CLI tests (plan 018 C11).

craze's native provider (internal/agent/native.go, internal/harness) talks to
a real HTTP endpoint through Fantasy's openaicompat provider: there is no test
seam in the shipped binary (WithHTTPClient is a Go-test-only hook), so
exercising it from tests/cli means standing up something that actually speaks
the wire protocol. This is a stdlib http.server, no third-party HTTP
dependency, run in a background thread on 127.0.0.1:0 (an OS-assigned port),
so tests never race a fixed port against each other or the developer's own
services.

The chat-completion-chunk and error-envelope shapes mirror
internal/harness/llm/wrap_test.go's sse/textChunk/finishChunk helpers
byte-for-byte, because those are the shapes the real openaicompat client (not
a fake) parses; drifting from them would test a wire format nothing speaks.

Canary handling: SSEFixture's error modes echo the request's Authorization
header into the response body, the way a real provider's 401 sometimes does
(internal/harness/llm/wrap_test.go's TestWrap_401EchoesAuthorizationIsScrubbed).
That is the realistic leak path package llm's scrubber guards (plan 018 §3.5);
CANARY below is the same obviously-fake key every Go fixture in this repo
uses, so a test that failed to scrub it would fail loudly rather than compare
two empty strings.
"""

from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

# CANARY is not a secret: it is the fixed placeholder every native-provider
# test uses so "the canary never appears in stdout/stderr" has one exact
# string to grep for. >= 8 bytes: internal/harness/llm.New refuses a shorter
# key (ErrAPIKeyTooShort) as too obviously a typo to be real.
CANARY = "sk-canary-not-a-secret-0001"

# UNUSED_ENV_KEY is the model table's env_keys entry: a name chosen so it is
# never set in a developer's or CI's real environment, which is what proves
# the inline api_key above -- not some ambient FIREWORKS_API_KEY or
# OPENAI_API_KEY -- is the key that reached the wire (plan 018 §3.8's
# fundedModel and modeltable.Resolve both try env_keys before the inline key).
UNUSED_ENV_KEY = "CRAZE_NATIVE_FIXTURE_UNSET_KEY"


@dataclass
class RecordedRequest:
    path: str
    authorization: str
    body: dict


@dataclass
class SSEFixture:
    """A background HTTP server answering POST <base_url>/chat/completions.

    set_ok / set_unauthorized / set_server_error switch what the *next*
    request(s) get; requests already seen are kept in .requests so a test can
    assert the wire really carried the canary (the leak path) even though
    craze's own output never may.
    """

    host: str = "127.0.0.1"

    def __post_init__(self) -> None:
        self._lock = threading.Lock()
        self._mode = "ok"
        self._text_parts: list[str] = ["ok"]
        self._reasoning_parts: list[str] = []
        self.requests: list[RecordedRequest] = []
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: object) -> None:  # quiet: no test needs stdlib's own log
                pass

            def do_POST(self) -> None:  # noqa: N802 (stdlib's naming)
                length = int(self.headers.get("Content-Length", "0") or "0")
                raw = self.rfile.read(length) if length else b""
                try:
                    body = json.loads(raw) if raw else {}
                except json.JSONDecodeError:
                    body = {"_unparsed": raw.decode("utf-8", "replace")}
                auth = self.headers.get("Authorization", "")
                with fixture._lock:
                    fixture.requests.append(RecordedRequest(self.path, auth, body))
                    mode = fixture._mode
                    text_parts = list(fixture._text_parts)
                    reasoning_parts = list(fixture._reasoning_parts)
                if mode == "ok":
                    self._reply_ok(text_parts, reasoning_parts)
                elif mode == "unauthorized":
                    self._reply_error(401, auth)
                elif mode == "servererror":
                    self._reply_error(500, auth)
                else:  # pragma: no cover - a test bug, not a fixture behaviour
                    raise AssertionError(f"sse_fixture: unknown mode {mode!r}")

            def _reply_ok(self, text_parts: list[str], reasoning_parts: list[str]) -> None:
                payload = _sse_body(text_parts, reasoning_parts)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def _reply_error(self, status: int, authorization: str) -> None:
                # The realistic leak path (plan 018 §3.5): a provider that
                # echoes the Authorization header back in its error body.
                # Retry-After-Ms keeps Fantasy's 5s default backoff (agent.go)
                # from making a retried 500 slow.
                message = f"invalid credentials: {authorization}"
                payload = json.dumps(
                    {"error": {"message": message, "type": "invalid_request_error"}}
                ).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.send_header("Retry-After-Ms", "1")
                self.end_headers()
                self.wfile.write(payload)

        self._handler = Handler
        self._server = ThreadingHTTPServer((self.host, 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    def start(self) -> "SSEFixture":
        self._thread.start()
        return self

    @property
    def port(self) -> int:
        return self._server.server_address[1]

    @property
    def base_url(self) -> str:
        """The model table's base_url: openaicompat appends "/chat/completions"."""
        return f"http://{self.host}:{self.port}/v1"

    def set_ok(self, text_parts: list[str] | None = None, reasoning_parts: list[str] | None = None) -> None:
        with self._lock:
            self._mode = "ok"
            self._text_parts = text_parts if text_parts is not None else ["ok"]
            self._reasoning_parts = reasoning_parts or []

    def set_unauthorized(self) -> None:
        with self._lock:
            self._mode = "unauthorized"

    def set_server_error(self) -> None:
        with self._lock:
            self._mode = "servererror"

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    def __enter__(self) -> "SSEFixture":
        return self.start()

    def __exit__(self, *exc: object) -> None:
        self.close()


def _delta_chunk(delta: dict) -> str:
    return json.dumps(
        {
            "id": "c",
            "object": "chat.completion.chunk",
            "choices": [{"index": 0, "delta": delta, "finish_reason": None}],
        }
    )


def _finish_chunk(reason: str) -> str:
    return json.dumps(
        {
            "id": "c",
            "object": "chat.completion.chunk",
            "choices": [{"index": 0, "delta": {}, "finish_reason": reason}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
        }
    )


def _sse_body(text_parts: list[str], reasoning_parts: list[str]) -> bytes:
    chunks = [_delta_chunk({"reasoning_content": r}) for r in reasoning_parts]
    chunks += [_delta_chunk({"content": t}) for t in text_parts]
    chunks.append(_finish_chunk("stop"))
    out = "".join(f"data: {c}\n\n" for c in chunks)
    out += "data: [DONE]\n\n"
    return out.encode("utf-8")


def write_native_config(
    native_dir: Path,
    base_url: str,
    api_key: str = CANARY,
    *,
    provider_id: str = "fixture",
    alias: str = "fixture-model",
    wire_model: str = "fixture-wire-model",
    name: str = "Fixture Model",
) -> None:
    """Write providers.toml (0600) and models.toml (0644) in the version-1
    schema internal/harness/modeltable reads (modeltable.go's providerEntry
    and modelEntry), pointed at a fixture server.

    env_keys names UNUSED_ENV_KEY -- never set in this process's environment
    -- so the inline api_key is what actually resolves (modeltable.Resolve
    tries env_keys before the inline key), and no real provider key sitting in
    a developer's shell can shadow it.
    """
    native_dir.mkdir(parents=True, exist_ok=True)
    os.chmod(native_dir, 0o700)

    providers_toml = (
        "version = 1\n"
        "\n"
        f"[providers.{provider_id}]\n"
        'driver = "openai-compat"\n'
        f'base_url = "{base_url}"\n'
        f'env_keys = ["{UNUSED_ENV_KEY}"]\n'
        f'api_key = "{api_key}"\n'
        'source = "manual"\n'
    )
    providers_path = native_dir / "providers.toml"
    providers_path.write_text(providers_toml, encoding="utf-8")
    os.chmod(providers_path, 0o600)

    models_toml = (
        "version = 1\n"
        f'default_model = "{alias}"\n'
        "\n"
        f"[models.{alias}]\n"
        f'provider = "{provider_id}"\n'
        f'wire_model = "{wire_model}"\n'
        f'name = "{name}"\n'
        'source = "manual"\n'
    )
    models_path = native_dir / "models.toml"
    models_path.write_text(models_toml, encoding="utf-8")
    os.chmod(models_path, 0o644)
