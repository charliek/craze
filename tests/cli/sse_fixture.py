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
The tool-call deltas (plan 019 C11) are the same wire, one step further: a
`tool_calls` delta opening with an id and a name and continuing with argument
fragments keyed by index, which is what makes a tool loop scriptable from
here at all -- see Step and set_script.

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
from itertools import zip_longest
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

    @property
    def messages(self) -> list[dict]:
        """The conversation this request carried, as the wire spells it."""
        msgs = self.body.get("messages")
        return msgs if isinstance(msgs, list) else []

    @property
    def tool_names(self) -> list[str]:
        """The tool ids offered on this request, in the order they were sent."""
        names = []
        for t in self.body.get("tools") or []:
            fn = t.get("function") if isinstance(t, dict) else None
            if isinstance(fn, dict) and isinstance(fn.get("name"), str):
                names.append(fn["name"])
        return names


@dataclass(frozen=True)
class ToolCall:
    """One call in a scripted step: what the model asks craze to run."""

    id: str
    name: str
    arguments: str


@dataclass(frozen=True)
class Step:
    """One whole response: everything the fixture answers one request with.

    A turn is a list of these, one per request, which is what makes a tool
    loop scriptable: the harness sends a request, runs whatever calls came
    back, sends the results, and the next Step answers that second request.
    Before this the fixture had one sticky answer, so every request got the
    same body and a loop could not be expressed at all (plan 019 §2.6).

    finish is the chunk's finish_reason: "tool_calls" when there are calls
    and "stop" when there are not, unless a case wants another -- "length"
    is the output ceiling, which is how a call cut off mid-arguments is
    scripted.

    interleaved says how several calls are laid out on the wire: by default
    every call is opened before any of their arguments follow (head-0,
    head-1, args-0, args-1, ...), which is what a provider streaming calls in
    parallel sends and the only order that tells an accumulator keyed by
    index apart from one tracking a single "current call". False sends each
    call whole before the next one starts, the order a provider that emits
    them one after another sends; both are legal and both are covered.
    """

    text: tuple[str, ...] = ()
    reasoning: tuple[str, ...] = ()
    calls: tuple[ToolCall, ...] = ()
    finish: str = ""
    interleaved: bool = True

    def finish_reason(self) -> str:
        if self.finish:
            return self.finish
        return "tool_calls" if self.calls else "stop"


def answer(*text: str, reasoning: tuple[str, ...] | list[str] = (), finish: str = "") -> Step:
    """A step that answers in words and ends the turn."""
    return Step(text=tuple(text), reasoning=tuple(reasoning), finish=finish)


def call_step(name: str, arguments: dict | str, *, call_id: str = "", finish: str = "") -> Step:
    """A step that calls one tool. A dict is sent as compact JSON.

    call_id defaults to the tool's own name, which is unique within a
    scripted turn that calls each tool once and reads better in a failure
    message than a counter would.
    """
    raw = arguments if isinstance(arguments, str) else json.dumps(arguments, separators=(",", ":"))
    return Step(calls=(ToolCall(call_id or name, name, raw),), finish=finish)


def parallel_step(*calls: ToolCall, finish: str = "", interleaved: bool = True) -> Step:
    """A step that asks for several calls at once, each on its own index."""
    return Step(calls=calls, finish=finish, interleaved=interleaved)


@dataclass
class SSEFixture:
    """A background HTTP server answering POST <base_url>/chat/completions.

    set_ok / set_script / set_unauthorized / set_server_error switch what the
    *next* request(s) get; requests already seen are kept in .requests so a
    test can assert the wire really carried the canary (the leak path) even
    though craze's own output never may, and -- since a scripted loop's later
    requests carry the earlier calls' results -- that the loop really fed
    them back.
    """

    host: str = "127.0.0.1"

    def __post_init__(self) -> None:
        self._lock = threading.Lock()
        self._mode = "ok"
        self._sticky = Step(text=("ok",))
        # None means "answer every request with _sticky"; a list is consumed
        # one Step per request, and running off its end is a test bug the
        # fixture reports loudly rather than papering over (_reply_exhausted).
        self._script: list[Step] | None = None
        self.unscripted = 0
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
                    n = len(fixture.requests)
                    fixture.requests.append(RecordedRequest(self.path, auth, body))
                    mode = fixture._mode
                    step = fixture._take_step_locked()
                if mode == "ok":
                    if step is None:
                        self._reply_exhausted(n)
                    else:
                        self._reply_ok(step)
                elif mode == "unauthorized":
                    self._reply_error(401, auth)
                elif mode == "servererror":
                    self._reply_error(500, auth)
                else:  # pragma: no cover - a test bug, not a fixture behaviour
                    raise AssertionError(f"sse_fixture: unknown mode {mode!r}")

            def _reply_ok(self, step: Step) -> None:
                payload = _sse_body(step)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def _reply_exhausted(self, n: int) -> None:
                # A scripted turn that asked for one request more than the
                # script has. 400, not 500: the harness retries a 5xx once
                # (plan 018 §3.8) and the retry would hide the mistake behind
                # a second unscripted request. The body says which request it
                # was, so a test that goes off script fails saying so.
                payload = json.dumps(
                    {
                        "error": {
                            "message": f"sse_fixture: no scripted response for request {n}",
                            "type": "invalid_request_error",
                        }
                    }
                ).encode("utf-8")
                self.send_response(400)
                self.send_header("Content-Type", "application/json")
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
        """One answer, repeated for every request: the pre-tools behaviour."""
        with self._lock:
            self._mode = "ok"
            self._script = None
            self._sticky = Step(
                text=tuple(text_parts if text_parts is not None else ["ok"]),
                reasoning=tuple(reasoning_parts or []),
            )

    def set_script(self, steps: list[Step]) -> None:
        """Answer request i with steps[i]; a request past the end fails (400).

        This is how a tool loop is declared: each Step is one model response,
        so a list like [call_step(...), call_step(...), answer(...)] is a
        two-call turn that then ends.
        """
        with self._lock:
            self._mode = "ok"
            self._script = list(steps)
            self.unscripted = 0

    def _take_step_locked(self) -> Step | None:
        """The response for this request. self._lock is held."""
        if self._script is None:
            return self._sticky
        if not self._script:
            self.unscripted += 1
            return None
        return self._script.pop(0)

    @property
    def script_remaining(self) -> int:
        """Scripted steps no request has taken: a turn that ended early."""
        with self._lock:
            return 0 if self._script is None else len(self._script)

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


def _tool_call_chunks(index: int, c: ToolCall) -> list[str]:
    """One tool call as the OpenAI wire streams it.

    The opening delta carries the id, the type and the function's name with
    empty arguments, and the arguments follow in further deltas keyed only by
    index -- which is the shape every OpenAI-compatible provider sends and the
    one openaicompat's accumulator is written against. They are split in two
    on purpose: a fixture that sent the whole arguments string in one delta
    would pass even if nothing accumulated them.
    """
    head = _delta_chunk(
        {
            "tool_calls": [
                {
                    "index": index,
                    "id": c.id,
                    "type": "function",
                    "function": {"name": c.name, "arguments": ""},
                }
            ]
        }
    )
    half = len(c.arguments) // 2
    out = [head]
    for frag in (c.arguments[:half], c.arguments[half:]):
        if frag == "":
            continue
        out.append(_delta_chunk({"tool_calls": [{"index": index, "function": {"arguments": frag}}]}))
    return out


def sse_call_chunks(step: Step) -> list[str]:
    """Every call of step, laid out as step.interleaved asks.

    Interleaved is the default because it is the order that discriminates: a
    reader keyed by index reassembles head-0, head-1, args-0, args-1 exactly
    as it does the sequential order, and a reader that only tracks the call
    it opened last joins index 1's arguments onto index 0 and loses one call
    entirely. Sent one whole call at a time, the two readers agree, so that
    order proves nothing on its own and is kept only as the other shape a
    provider really sends.
    """
    per_call = [_tool_call_chunks(i, c) for i, c in enumerate(step.calls)]
    if not step.interleaved:
        return [chunk for call in per_call for chunk in call]
    return [chunk for round_ in zip_longest(*per_call) for chunk in round_ if chunk is not None]


def _sse_body(step: Step) -> bytes:
    chunks = [_delta_chunk({"reasoning_content": r}) for r in step.reasoning]
    chunks += [_delta_chunk({"content": t}) for t in step.text]
    chunks += sse_call_chunks(step)
    chunks.append(_finish_chunk(step.finish_reason()))
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
