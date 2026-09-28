"""The recording reverse proxy (plan 029 §3.1.2).

Routes are ``/r/<token>/<upstream-host>/<upstream-path-prefix>/<endpoint>``: the
upstream host stays visible in every base URL a harness is given, and ``<token>`` is
a random per-run value that stops working when the run ends. The proxy is the only
holder of provider keys; it replaces ``Authorization`` on the way out and refuses any
request that carries a loaded key -- raw, in any decoded JSON string, or in the body
it would actually forward. It forwards only ``POST …/chat/completions``, ``POST
…/responses`` and ``GET …/models``, enforces the run's target-model allowlist and the
budget (one completion per request; reserve against the largest output limit before
forwarding, reconcile after), streams responses to the harness with every loaded
key redacted (headers and body, keys split across chunks included), and journals
every request to the run's capture as it happens, scrubbed.

Each forwarded request's upstream exchange runs in a task of its own from the first
byte (review r1-c1 finding 8): a harness that hangs up -- before or after the
response headers -- only stops the relay to it; the exchange reads on (bounded) so
the usage still arrives and is reconciled.

Besides TCP (the standalone proxy for the live smoke) it listens on a Unix socket,
which is all an agent's sandbox can reach (sandbox.py's relay).
"""

from __future__ import annotations

import asyncio
import json
import secrets
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping

import aiohttp
from aiohttp import web

from crazeeval.capture import CaptureWriter, SSEParser, usage_and_model
from crazeeval.keys import KeyRing, Provider
from crazeeval.ledger import BUDGET_STOP, Ledger
from crazeeval.pricing import (
    MODEL_MAX_OUTPUT_DEFAULT,
    Price,
    cost,
    list_reservation,
    multi_completion,
    request_max_tokens,
    reservation,
)

ALLOWED_ENDPOINTS = {("POST", "/chat/completions"), ("POST", "/responses"), ("GET", "/models")}

HOP_BY_HOP = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "trailers",
    "transfer-encoding", "upgrade",
}
# Never forwarded upstream: hop-by-hop, framing, encoding (aiohttp decompresses and
# re-frames), and every credential header (the proxy sets Authorization itself).
REQUEST_DROP = HOP_BY_HOP | {
    "host", "content-length", "content-encoding", "accept-encoding",
    "authorization", "x-api-key", "api-key", "cookie",
}
RESPONSE_DROP = HOP_BY_HOP | {"content-length", "content-encoding", "set-cookie", "authorization", "www-authenticate"}
BODY_CAPTURE_LIMIT = 32 * 1024 * 1024
# How long the proxy keeps reading an upstream stream after the harness hung up on
# it, so its usage still arrives (a title call cut off when the harness exits).
DRAIN_TIMEOUT_S = 120.0
# Bytes an exchange may hold for a harness that is slow to read; past this the
# harness is treated as stalled: the exchange stops queueing for it and drains
# (review r2-c1 item 8). And how long one write to the harness may take.
QUEUE_LIMIT_BYTES = 8 * 1024 * 1024
WRITE_DEADLINE_S = 60.0
REDACTED = b"<scrubbed-key>"

# Refusal reasons (recorded in the capture and counted per run).
KEY_EXPOSURE = "key-exposure"
MODEL_NOT_ALLOWED = "model-not-allowed"
MODEL_UNPRICED = "model-unpriced"
PATH_NOT_ALLOWED = "path-not-allowed"
UPSTREAM_NOT_ALLOWED = "upstream-not-allowed"
BAD_JSON = "bad-json"
BAD_LIMIT = "bad-output-limit"
MULTI_COMPLETION = "multi-completion"
NO_KEY = "no-key"
EXPIRED = "expired-token"


@dataclass
class RouteState:
    token: str
    run_id: str
    harness: str
    models: frozenset[str]
    capture: CaptureWriter
    active: bool = True
    seq: int = 0
    counters: dict = field(default_factory=lambda: {"requests": 0, "forwarded": 0, "refused": {}, "upstream_errors": 0})
    flags: set = field(default_factory=set)
    inflight: set = field(default_factory=set)

    def next_seq(self) -> int:
        self.seq += 1
        return self.seq

    def refused(self, reason: str) -> None:
        self.counters["refused"][reason] = self.counters["refused"].get(reason, 0) + 1

    def summary(self) -> dict:
        return {
            "run_id": self.run_id,
            "harness": self.harness,
            "models": sorted(self.models),
            "requests": self.counters["requests"],
            "forwarded": self.counters["forwarded"],
            "refused": dict(self.counters["refused"]),
            "key_exposure_refusals": self.counters["refused"].get(KEY_EXPOSURE, 0),
            "upstream_errors": self.counters["upstream_errors"],
            "flags": sorted(self.flags),
        }


class Redactor:
    """Bytes to the harness with every loaded key replaced. Holds back only a tail
    that could be the start of a key split across chunks (review r1-c1 finding 4)."""

    def __init__(self, forms: tuple[bytes, ...]):
        self._forms = forms
        self._held = b""
        self.redactions = 0

    def _replace(self, buf: bytes) -> bytes:
        for f in self._forms:
            if f in buf:
                self.redactions += buf.count(f)
                buf = buf.replace(f, REDACTED)
        return buf

    def feed(self, chunk: bytes) -> bytes:
        buf = self._replace(self._held + chunk)
        cut = len(buf)
        for f in self._forms:
            for k in range(min(len(f) - 1, len(buf)), 0, -1):
                if buf.endswith(f[:k]):
                    cut = min(cut, len(buf) - k)
                    break
        self._held = buf[cut:]
        return buf[:cut]

    def flush(self) -> bytes:
        out, self._held = self._replace(self._held), b""
        return out


_EOF = object()


class Exchange:
    """One forwarded request's upstream side, owned by its own task.

    Chunks for the harness wait in a queue bounded by bytes (``QUEUE_LIMIT_BYTES``):
    a harness that stops reading -- connected or not -- switches the exchange to
    discard-and-drain, so the read and its accounting go on and nothing buffers
    without bound.
    """

    def __init__(self, loop: asyncio.AbstractEventLoop):
        self.loop = loop
        self.headers: asyncio.Future = loop.create_future()
        self.queue: asyncio.Queue = asyncio.Queue()
        self.queued_bytes = 0
        self.max_queued_bytes = 0
        self.gone = False
        self.timeout: asyncio.Timeout | None = None
        self.state: dict[str, Any] = {"status": None, "ctype": None, "parser": None, "buf": bytearray(), "error": None,
                                      "client_gone": False, "client_stalled": False}

    def offer(self, chunk: bytes) -> None:
        """Queue a chunk for the harness, or give up on a harness that is not reading."""
        if self.gone:
            return
        if self.queued_bytes + len(chunk) > QUEUE_LIMIT_BYTES:
            self.mark_gone(stalled=True)
            return
        self.queue.put_nowait(chunk)
        self.queued_bytes += len(chunk)
        self.max_queued_bytes = max(self.max_queued_bytes, self.queued_bytes)
        self.state["max_queued_bytes"] = self.max_queued_bytes

    async def take(self):
        item = await self.queue.get()
        if isinstance(item, bytes):
            self.queued_bytes -= len(item)
        return item

    def mark_gone(self, stalled: bool = False) -> None:
        """The harness hung up or stopped reading: stop queueing for it and bound the
        rest of the read."""
        if stalled:
            self.state["client_stalled"] = True
        if self.gone:
            return
        self.gone = True
        self.state["client_gone"] = True
        while not self.queue.empty():
            self.queue.get_nowait()
        self.queued_bytes = 0
        self.queue.put_nowait(_EOF)  # a handler still waiting ends its response
        if self.timeout is not None:
            try:
                self.timeout.reschedule(self.loop.time() + DRAIN_TIMEOUT_S)
            except RuntimeError:
                pass  # the exchange already finished reading


class Proxy:
    def __init__(
        self,
        providers: Mapping[str, Provider],
        keyring: KeyRing,
        ledger: Ledger,
        prices: Mapping[str, Price],
        *,
        host: str = "127.0.0.1",
        port: int = 0,
        upstream_override: Mapping[str, str] | None = None,
        refusal_log: Path | None = None,
        max_output: Mapping[str, int] | None = None,
    ):
        self.providers = dict(providers)
        self.keyring = keyring
        self.ledger = ledger
        self.prices = dict(prices)
        self.host = host
        self.port = port
        self.unix_path: Path | None = None
        self._override = dict(upstream_override or {})
        # Each wire model's maximum output tokens: the reservation for a request that
        # names no output limit (review r2-c1 item 10); the request itself is untouched.
        self.max_output = dict(max_output or {})
        self._refusal_log = Path(refusal_log) if refusal_log else None
        self._routes: dict[str, RouteState] = {}
        self._expired: dict[str, str] = {}
        self._runner: web.AppRunner | None = None
        self._session: aiohttp.ClientSession | None = None
        self.budget_stopped = False
        self.unrouted_refusals = 0

    # -- lifecycle -------------------------------------------------------------

    async def start(self, unix_path: Path | None = None) -> None:
        app = web.Application(client_max_size=512 * 1024 * 1024)
        app.router.add_route("*", "/{tail:.*}", self._handle)
        self._runner = web.AppRunner(app, access_log=None, handler_cancellation=True)
        await self._runner.setup()
        site = web.TCPSite(self._runner, self.host, self.port)
        await site.start()
        sockets = site._server.sockets  # type: ignore[union-attr]
        self.port = sockets[0].getsockname()[1]
        if unix_path is not None:
            await web.UnixSite(self._runner, str(unix_path)).start()
            self.unix_path = Path(unix_path)
        self._session = aiohttp.ClientSession(
            auto_decompress=True,
            timeout=aiohttp.ClientTimeout(total=None, sock_connect=30, sock_read=900),
        )

    async def stop(self) -> None:
        for route in list(self._routes.values()):
            await self.end_run(route, grace=2.0)
        if self._session is not None:
            await self._session.close()
        if self._runner is not None:
            await self._runner.cleanup()

    def route_table(self) -> list[dict]:
        """Hosts and prefixes only (for the provenance manifest)."""
        return [
            {"provider": p.name, "host": p.host, "prefix": p.prefix} for p in sorted(self.providers.values(), key=lambda p: p.name)
        ]

    # -- runs ------------------------------------------------------------------

    def register_run(self, run_id: str, harness: str, models: set[str] | frozenset[str], capture_path: Path) -> RouteState:
        token = secrets.token_hex(16)
        route = RouteState(
            token=token,
            run_id=run_id,
            harness=harness,
            models=frozenset(models),
            capture=CaptureWriter(Path(capture_path), self.keyring.scrub),
        )
        self._routes[token] = route
        return route

    def base_url(self, route: RouteState, provider: str) -> str:
        p = self.providers[provider]
        tail = f"/{p.prefix}" if p.prefix else ""
        return f"http://{self.host}:{self.port}/r/{route.token}/{p.host}{tail}"

    async def end_run(self, route: RouteState, grace: float = 30.0) -> dict:
        """Close the route: its token stops working at once. In-flight requests and
        their exchanges get ``grace`` seconds to deliver their usage, then are cancelled
        (an exchange cancelled without usage keeps its reservation)."""
        route.active = False
        loop = asyncio.get_running_loop()
        deadline = loop.time() + grace
        while True:
            pending = {t for t in route.inflight if not t.done()}
            left = deadline - loop.time()
            if not pending or left <= 0:
                break
            await asyncio.wait(pending, timeout=left)
        for _ in range(3):
            still = {t for t in route.inflight if not t.done()}
            if not still:
                break
            for t in still:
                t.cancel()
            await asyncio.gather(*still, return_exceptions=True)
        route.capture.close()
        self._routes.pop(route.token, None)
        self._expired[route.token] = route.run_id
        return route.summary()

    # -- helpers ---------------------------------------------------------------

    def _match_provider(self, host: str, rest: str) -> Provider | None:
        best = None
        for p in self.providers.values():
            if p.host != host:
                continue
            if p.prefix and not (rest == p.prefix or rest.startswith(p.prefix + "/")):
                continue
            if best is None or len(p.prefix) > len(best.prefix):
                best = p
        return best

    def _upstream_url(self, p: Provider, endpoint: str, query: str) -> str:
        base = self._override.get(p.host, f"https://{p.host}")
        url = f"{base}/{p.prefix}{endpoint}" if p.prefix else f"{base}{endpoint}"
        return f"{url}?{query}" if query else url

    def _log_unrouted(self, rec: dict) -> None:
        self.unrouted_refusals += 1
        if self._refusal_log is None:
            return
        self._refusal_log.parent.mkdir(parents=True, exist_ok=True)
        with open(self._refusal_log, "a", encoding="utf-8") as f:
            f.write(self.keyring.scrub(json.dumps(rec, default=str)) + "\n")

    @staticmethod
    def _error(status: int, reason: str, message: str) -> web.Response:
        return web.json_response(
            {"error": {"message": f"crazeeval proxy: {message}", "type": reason, "code": reason}}, status=status
        )

    def _refuse(self, route: RouteState, rec: dict, status: int, reason: str, message: str) -> web.Response:
        route.refused(reason)
        rec.update({"kind": "record", "status": status, "refused": reason, "duration_s": 0.0})
        route.capture.write(rec)
        return self._error(status, reason, message)

    def _response_headers(self, up: aiohttp.ClientResponse) -> dict:
        out = {}
        for k, v in up.headers.items():
            if k.lower() in RESPONSE_DROP or self.keyring.contains_key(k) or self.keyring.contains_key(v):
                continue
            out[k] = v
        return out

    # -- the handler -------------------------------------------------------------

    async def _handle(self, request: web.Request) -> web.StreamResponse:
        parts = request.path.split("/", 4)
        if len(parts) < 5 or parts[1] != "r" or not parts[2] or not parts[3]:
            if request.method in ("GET", "HEAD") and request.path == "/":
                # gx pre-warms its HTTP client with an unauthenticated GET of the
                # base URL's origin; nothing is forwarded, nothing counted.
                return self._error(404, "not-found", "nothing here")
            self._log_unrouted({"ts": time.time(), "method": request.method, "reason": "bad-path"})
            return self._error(404, "bad-path", "not a /r/<token>/<host>/... route")
        token, host, rest = parts[2], parts[3], parts[4]
        route = self._routes.get(token)
        if route is None or not route.active:
            reason = EXPIRED if (token in self._expired or route is not None) else "unknown-token"
            self._log_unrouted(
                {
                    "ts": time.time(),
                    "method": request.method,
                    "reason": reason,
                    "run_id": self._expired.get(token) or (route.run_id if route else None),
                    "host": host,
                }
            )
            return self._error(403, reason, "this route is not open")
        task = asyncio.current_task()
        route.inflight.add(task)
        try:
            return await self._handle_route(request, route, host, rest)
        finally:
            route.inflight.discard(task)

    async def _handle_route(self, request: web.Request, route: RouteState, host: str, rest: str) -> web.StreamResponse:
        t0 = time.monotonic()
        route.counters["requests"] += 1
        rec: dict[str, Any] = {
            "ts": time.time(),
            "run_id": route.run_id,
            "harness": route.harness,
            "seq": route.next_seq(),
            "method": request.method,
            "host": host,
            "route": None,
            "path": None,
            "query": request.query_string or "",
            "request_content_type": request.headers.get("Content-Type"),
        }
        provider = self._match_provider(host, rest)
        if provider is None:
            rec["path"] = "/" + rest
            return self._refuse(route, rec, 403, UPSTREAM_NOT_ALLOWED, "upstream not in the route table")
        rec["route"] = provider.name
        raw_endpoint = "/" + rest[len(provider.prefix) :].lstrip("/") if provider.prefix else "/" + rest
        # One normal form for every later decision (review r1-c1 finding 10).
        endpoint = raw_endpoint.rstrip("/") or "/"
        rec["path"] = endpoint
        if (request.method, endpoint) not in ALLOWED_ENDPOINTS:
            return self._refuse(route, rec, 403, PATH_NOT_ALLOWED, f"{request.method} {raw_endpoint} is not forwarded")
        body = await request.read()
        header_blob = "\n".join(f"{k}: {v}" for k, v in request.headers.items())
        if any(self.keyring.contains_key(part) for part in (body, request.path, request.query_string, header_blob)):
            return self._key_exposure(route, rec, body)
        key = self.keyring.key_for(provider.name)
        if not key:
            return self._refuse(route, rec, 403, NO_KEY, f"no key for provider {provider.name}")

        fwd_body: bytes | None = None
        res = None
        price: Price | None = None
        if request.method == "POST":
            try:
                payload = json.loads(body)
            except ValueError:
                rec["request"] = body.decode("utf-8", errors="replace")[:100_000]
                return self._refuse(route, rec, 400, BAD_JSON, "request body is not JSON")
            if not isinstance(payload, dict):
                rec["request"] = payload
                return self._refuse(route, rec, 400, BAD_JSON, "request body is not a JSON object")
            # A key the raw body only carried escaped (review r1-c1 finding 5).
            if self.keyring.json_contains_key(payload):
                return self._key_exposure(route, rec, body)
            rec["request"] = payload
            model = payload.get("model")
            rec["request_model"] = model
            if model not in route.models:
                return self._refuse(route, rec, 403, MODEL_NOT_ALLOWED, f"model {model!r} is not this run's target")
            price = self.prices.get(model)
            if price is None:
                return self._refuse(route, rec, 403, MODEL_UNPRICED, f"model {model!r} has no price")
            multi = multi_completion(payload)
            if multi is not None:
                return self._refuse(route, rec, 400, MULTI_COMPLETION, f"{multi} must be 1")
            try:
                max_tokens = request_max_tokens(payload, self.max_output.get(model, MODEL_MAX_OUTPUT_DEFAULT))
            except ValueError as e:
                return self._refuse(route, rec, 400, BAD_LIMIT, str(e))
            injected = False
            fwd_payload = payload
            if endpoint == "/chat/completions" and payload.get("stream") is True:
                so = payload.get("stream_options")
                if not (isinstance(so, dict) and so.get("include_usage") is True):
                    fwd_payload = dict(payload)
                    fwd_payload["stream_options"] = {**(so if isinstance(so, dict) else {}), "include_usage": True}
                    injected = True
            rec["usage_injected"] = injected
            fwd_body = json.dumps(fwd_payload).encode() if injected else body
            # The bytes that would actually leave, after any re-serialisation.
            if self.keyring.contains_key(fwd_body):
                return self._key_exposure(route, rec, body)
            amount = reservation(price, len(fwd_body), max_tokens)
            # The route may have closed while the body was read: re-check, and admit
            # with no await in between (review r1-c1 finding 7).
            if not route.active:
                return self._refuse(route, rec, 403, EXPIRED, "this route closed")
            adm = self.ledger.admit(
                run_id=route.run_id,
                harness=route.harness,
                model=model,
                provider=provider.name,
                amount=amount,
                list_amount=list_reservation(price, len(fwd_body), max_tokens),
            )
            if not adm.ok:
                route.flags.add(adm.reason)
                if adm.reason == BUDGET_STOP:
                    self.budget_stopped = True
                rec["reservation"] = amount
                return self._refuse(route, rec, 402, adm.reason, f"{adm.reason}: spend limit reached")
            res = adm.reservation
            rec["reservation"] = res.amount
        elif not route.active:
            return self._refuse(route, rec, 403, EXPIRED, "this route closed")

        headers = {k: v for k, v in request.headers.items() if k.lower() not in REQUEST_DROP}
        headers["Authorization"] = f"Bearer {key}"
        if fwd_body is not None:
            headers["Content-Type"] = request.headers.get("Content-Type", "application/json")
        url = self._upstream_url(provider, endpoint, request.query_string)

        route.counters["forwarded"] += 1
        route.capture.write({"kind": "start", **rec})
        loop = asyncio.get_running_loop()
        ex = Exchange(loop)
        task = loop.create_task(self._exchange(route, rec, ex, res, price, t0, request.method, url, fwd_body, headers))
        route.inflight.add(task)
        task.add_done_callback(route.inflight.discard)
        response: web.StreamResponse | None = None
        try:
            up = await asyncio.shield(ex.headers)
            if up is None:
                return self._error(502, "upstream-error", f"upstream request failed ({ex.state['error']})")
            response = web.StreamResponse(status=up.status, reason=self._reason(up.reason),
                                          headers=self._response_headers(up))
            await response.prepare(request)
            red = Redactor(self.keyring.byte_forms())
            while True:
                item = await ex.take()
                if item is _EOF:
                    break
                out = red.feed(item)
                if out:
                    await self._write(ex, response, out)
            tail = red.flush()
            if tail and not ex.gone:
                await self._write(ex, response, tail)
            if not ex.gone:
                await asyncio.wait_for(response.write_eof(), WRITE_DEADLINE_S)
            return response
        except asyncio.TimeoutError:
            # The harness stopped reading: discard-and-drain; the exchange accounts on.
            ex.mark_gone(stalled=True)
            return response if response is not None else web.Response(status=504)
        except asyncio.CancelledError:
            ex.mark_gone()
            raise
        except (ConnectionResetError, ConnectionError, RuntimeError):
            ex.mark_gone()
            return response if response is not None else web.Response(status=499)

    @staticmethod
    async def _write(ex: Exchange, response: web.StreamResponse, data: bytes) -> None:
        if ex.gone:
            return
        await asyncio.wait_for(response.write(data), WRITE_DEADLINE_S)

    def _reason(self, reason: str | None) -> str | None:
        """The upstream reason phrase, unless it holds a key (then the default one)."""
        if not reason or self.keyring.contains_key(reason) or "\r" in reason or "\n" in reason:
            return None
        return reason

    def _key_exposure(self, route: RouteState, rec: dict, body: bytes) -> web.Response:
        route.flags.add(KEY_EXPOSURE)
        rec["request"] = self.keyring.scrub(body.decode("utf-8", errors="replace"))
        return self._refuse(route, rec, 403, KEY_EXPOSURE, "the request carries a provider key")

    async def _exchange(self, route: RouteState, rec: dict, ex: Exchange, res, price, t0: float, method: str, url: str,
                        fwd_body: bytes | None, headers: dict) -> None:
        """Send the request, relay its chunks to the handler's queue and journal the
        stream, reading to the end whether or not the harness is still there."""
        state = ex.state
        up: aiohttp.ClientResponse | None = None
        try:
            async with asyncio.timeout(None) as tm:
                ex.timeout = tm
                if ex.gone:
                    tm.reschedule(ex.loop.time() + DRAIN_TIMEOUT_S)
                assert self._session is not None
                up = await self._session.request(method, url, data=fwd_body, headers=headers, allow_redirects=False)
                state["status"] = up.status
                state["ctype"] = up.headers.get("Content-Type", "")
                if "text/event-stream" in (state["ctype"] or ""):
                    state["parser"] = SSEParser()
                if not ex.headers.done():
                    ex.headers.set_result(up)
                async for chunk in up.content.iter_any():
                    self._tee(route, rec, state, chunk)
                    ex.offer(chunk)
            ex.timeout = None
            if ex.gone:
                rec["drained"] = True
        except TimeoutError:
            state["error"] = "drain-timeout"
            rec["drain_error"] = "timeout"
        except asyncio.CancelledError:
            state["error"] = state["error"] or "cancelled"
            raise
        except (aiohttp.ClientError, OSError) as e:
            state["error"] = f"upstream {type(e).__name__}"
            route.counters["upstream_errors"] += 1
        finally:
            if not ex.headers.done():
                ex.headers.set_result(None)
            ex.queue.put_nowait(_EOF)
            if up is not None:
                up.close()
            self._finish(route, rec, state, res, price, t0)

    @staticmethod
    def _tee(route: RouteState, rec: dict, state: dict, chunk: bytes) -> None:
        parser: SSEParser | None = state["parser"]
        if parser is not None:
            before = len(parser.events)
            parser.feed(chunk)
            if len(parser.events) > before:
                route.capture.events(rec["seq"], parser.events[before:])
        elif len(state["buf"]) < BODY_CAPTURE_LIMIT:
            state["buf"].extend(chunk)

    def _finish(self, route: RouteState, rec: dict, state: dict, res, price: Price | None, t0: float) -> None:
        """The journal's ``end`` line and the ledger's reconciliation."""
        parser: SSEParser | None = state["parser"]
        end: dict[str, Any] = {"kind": "end", "seq": rec["seq"]}
        if parser is not None:
            before = len(parser.events)
            parser.close()
            if len(parser.events) > before:
                route.capture.events(rec["seq"], parser.events[before:])
            end["response"] = {"done": parser.done}
            if parser.dropped:
                end["response"]["dropped_events"] = parser.dropped
                end["response"]["dropped_bytes"] = parser.dropped_bytes
            usage, served = usage_and_model({"events": parser.events})
        else:
            raw = bytes(state["buf"])
            try:
                end["response"] = {"body": json.loads(raw)} if raw else {"body": None}
            except ValueError:
                end["response"] = {"body": raw.decode("utf-8", errors="replace")[:1_000_000]}
            usage, served = usage_and_model(end["response"])
        end["status"] = state["status"]
        end["response_content_type"] = state["ctype"]
        end["duration_s"] = round(time.monotonic() - t0, 3)
        for k in ("drained", "drain_error"):
            if k in rec:
                end[k] = rec[k]
        if state["error"]:
            end["error"] = state["error"]
        if state["client_gone"]:
            end["client_disconnected"] = True
        if state.get("client_stalled"):
            end["client_stalled"] = True
            end["max_queued_bytes"] = state.get("max_queued_bytes", 0)
        end["served_model"] = served
        end["usage"] = usage.to_dict() if usage is not None else None
        if res is not None and price is not None:
            c = cost(price, usage) if usage is not None else None
            lc = cost(price, usage, list_price=True) if usage is not None else None
            end["cost"] = c
            end["list_cost"] = lc
            end["charged"] = self.ledger.settle(res, usage, c, lc, status=state["status"], note=state["error"])
        route.capture.write(end)


async def serve_forever(proxy: Proxy) -> None:
    await proxy.start()
    try:
        while True:
            await asyncio.sleep(3600)
    finally:
        await proxy.stop()
