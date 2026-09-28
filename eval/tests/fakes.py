"""A fake upstream (three providers on one loopback server) and a proxy wired to it."""

from __future__ import annotations

import asyncio
import json
from contextlib import asynccontextmanager
from pathlib import Path

import aiohttp
from aiohttp import web

from crazeeval.keys import KeyRing, Provider
from crazeeval.ledger import Ledger
from crazeeval.pricing import load_prices
from crazeeval.proxy import Proxy

ZAI_KEY = "zk-TEST-zai-7d1f0c2b9e4a4f5f8a1b"
META_KEY = "mk-TEST-meta-3c9e81aa04d74c1c9f2e"
FW_KEY = "fw_TEST_fireworks_5b2d77e0c1f94e2a"
KEYS = {"zai-coding-plan": ZAI_KEY, "meta": META_KEY, "fireworks": FW_KEY}

PROVIDERS = {
    "zai-coding-plan": Provider.from_url("zai-coding-plan", "openai-compat", "https://api.z.ai/api/coding/paas/v4"),
    "meta": Provider.from_url("meta", "openai-compat", "https://api.meta.ai/v1"),
    "fireworks": Provider.from_url("fireworks", "openai-compat", "https://api.fireworks.ai/inference/v1"),
}


def keyring() -> KeyRing:
    return KeyRing(KEYS)


def sse(objs) -> bytes:
    return b"".join(b"data: " + (o if isinstance(o, bytes) else json.dumps(o).encode()) + b"\n\n" for o in objs)


class Upstream:
    """Records what reached it; answers like the three providers do."""

    def __init__(self):
        self.seen: list[dict] = []
        self.runner = None
        self.port = None
        self.echo_key: str | None = None
        self.hang = asyncio.Event()

    async def start(self):
        app = web.Application()
        app.router.add_route("*", "/{tail:.*}", self.handle)
        self.runner = web.AppRunner(app, access_log=None)
        await self.runner.setup()
        site = web.TCPSite(self.runner, "127.0.0.1", 0)
        await site.start()
        self.port = site._server.sockets[0].getsockname()[1]

    async def stop(self):
        self.hang.set()
        await self.runner.cleanup()

    async def handle(self, request: web.Request):
        body = await request.read()
        rec = {"method": request.method, "path": request.path, "headers": dict(request.headers), "body": body}
        self.seen.append(rec)
        if request.method == "GET" and request.path.endswith("/models"):
            return web.json_response({"object": "list", "data": [{"id": "glm-5.3-flash"}]})
        payload = json.loads(body) if body else {}
        model = payload.get("model")
        if payload.get("delay_headers"):
            # Accepted upstream, but the headers come late: a harness may hang up first.
            await asyncio.sleep(payload["delay_headers"])
        if request.path.endswith("/responses"):
            return await self._responses(request, payload)
        if payload.get("reason_key") and self.echo_key:
            # An error whose HTTP reason phrase echoes the key.
            return web.Response(status=400, reason=f"Bad key {self.echo_key}", text="{}")
        if payload.get("big"):
            # A long stream: many ~64 KiB events, then usage.
            resp = web.StreamResponse(headers={"Content-Type": "text/event-stream"})
            await resp.prepare(request)
            pad = "x" * 65_000
            for _ in range(payload["big"]):
                await resp.write(sse([{"id": "c1", "model": model, "choices": [{"index": 0, "delta": {"content": pad}}]}]))
            await resp.write(sse([{"id": "c1", "model": model, "choices": [], "usage": {
                "prompt_tokens": 1000, "completion_tokens": 200}}]))
            await resp.write(b"data: [DONE]\n\n")
            await resp.write_eof()
            return resp
        if payload.get("stream"):
            headers = {"Content-Type": "text/event-stream", "X-Upstream-Secret": "hdr-value-1"}
            if self.echo_key:
                headers["X-Echo-Auth"] = f"Bearer {self.echo_key}"
            resp = web.StreamResponse(headers=headers)
            await resp.prepare(request)
            chunks = [
                {"id": "c1", "model": model + "-served", "choices": [{"index": 0, "delta": {"role": "assistant", "content": "Reading."}}]},
                {"id": "c1", "model": model + "-served", "choices": [{"index": 0, "delta": {"tool_calls": [
                    {"index": 0, "id": "call_a", "type": "function", "function": {"name": "bash", "arguments": '{"comm'}}]}}]},
                {"id": "c1", "model": model + "-served", "choices": [{"index": 0, "delta": {"tool_calls": [
                    {"index": 0, "function": {"arguments": 'and": "python -m pytest"}'}}]}}]},
            ]
            if self.echo_key:
                chunks.append({"id": "c1", "model": model, "choices": [{"index": 0, "delta": {"content": f"leak {self.echo_key}"}}]})
            await resp.write(sse(chunks))
            if self.echo_key and payload.get("split_key"):
                # The key split across two chunks, with a pause between them.
                half = len(self.echo_key) // 2
                await resp.write(b'data: {"choices": [{"index": 0, "delta": {"content": "split ' + self.echo_key[:half].encode())
                await asyncio.sleep(0.2)
                await resp.write(self.echo_key[half:].encode() + b'"}}]}\n\n')
            if payload.get("hang"):
                await self.hang.wait()
            if payload.get("slow_finish"):
                await asyncio.sleep(0.5)
            so = payload.get("stream_options") or {}
            if so.get("include_usage"):
                await resp.write(sse([{"id": "c1", "model": model, "choices": [], "usage": {
                    "prompt_tokens": 1000, "completion_tokens": 200,
                    "prompt_tokens_details": {"cached_tokens": 600},
                    "completion_tokens_details": {"reasoning_tokens": 50}}}]))
            await resp.write(b"data: [DONE]\n\n")
            await resp.write_eof()
            return resp
        if payload.get("no_usage"):
            return web.json_response({"id": "x", "model": model, "choices": [{"message": {"content": "hi"}}]})
        return web.json_response({
            "id": "x", "model": model,
            "choices": [{"message": {"role": "assistant", "content": "hello"}}],
            "usage": {"prompt_tokens": 100, "completion_tokens": 10},
        })

    async def _responses(self, request, payload):
        resp = web.StreamResponse(headers={"Content-Type": "text/event-stream"})
        await resp.prepare(request)
        model = payload.get("model")
        evs = [
            {"type": "response.created", "response": {"id": "r1", "model": model + "-2026", "status": "in_progress"}},
            {"type": "response.output_item.done", "item": {"type": "function_call", "call_id": "fc1", "name": "exec_command",
                                                           "arguments": '{"cmd": "ls"}'}},
            {"type": "response.completed", "response": {"id": "r1", "model": model + "-2026", "status": "completed",
                                                        "usage": {"input_tokens": 2000, "output_tokens": 100,
                                                                  "input_tokens_details": {"cached_tokens": 1000},
                                                                  "output_tokens_details": {"reasoning_tokens": 40},
                                                                  "total_tokens": 2100}}},
        ]
        await resp.write(sse(evs))
        await resp.write_eof()
        return resp


@asynccontextmanager
async def rig(tmp: Path, *, cap: float = 95.0, run_cap: float = 3.0, ledger_path: Path | None = None,
              max_output: dict | None = None):
    up = Upstream()
    await up.start()
    ring = keyring()
    led = Ledger(ledger_path or tmp / "ledger.jsonl", cap=cap, run_cap=run_cap, scrub=ring.scrub)
    base = f"http://127.0.0.1:{up.port}"
    proxy = Proxy(
        PROVIDERS, ring, led, load_prices(),
        upstream_override={p.host: base for p in PROVIDERS.values()},
        refusal_log=tmp / "refusals.jsonl",
        max_output=max_output,
    )
    await proxy.start()
    session = aiohttp.ClientSession()
    try:
        yield proxy, up, led, session
    finally:
        await session.close()
        await proxy.stop()
        await up.stop()
        led.close()
