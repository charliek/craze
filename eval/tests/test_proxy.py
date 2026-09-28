"""The recording proxy against a fake upstream (plan 029 §3.1.2, AC-A3, AC-A4)."""

from __future__ import annotations

import asyncio
import gzip
import json

import aiohttp
import pytest

from crazeeval.capture import read_capture
from fakes import FW_KEY, META_KEY, ZAI_KEY, rig


def run(coro):
    return asyncio.run(coro)


def chat(model, stream=True, **extra):
    body = {"model": model, "messages": [{"role": "user", "content": "hi"}], "stream": stream}
    body.update(extra)
    return body


async def post(session, url, body, headers=None):
    h = {"Authorization": "Bearer craze-eval-dummy-key", "Content-Type": "application/json"}
    h.update(headers or {})
    async with session.post(url, data=json.dumps(body), headers=h) as r:
        return r.status, await r.read(), dict(r.headers)


def raw_capture(tmp_path) -> str:
    """The gzipped capture's text, exactly as written."""
    return gzip.decompress((tmp_path / "run" / "capture.jsonl.gz").read_bytes()).decode()


def first_settle(tmp_path) -> dict:
    lines = [json.loads(x) for x in (tmp_path / "ledger.jsonl").read_text().splitlines()]
    return next(x for x in lines if x["kind"] == "settle")


def test_route_embeds_upstream_host(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "zai-coding-plan")
            assert url == f"http://127.0.0.1:{proxy.port}/r/{route.token}/api.z.ai/api/coding/paas/v4"
            assert "api.meta.ai/v1" in proxy.base_url(route, "meta")
            assert "api.fireworks.ai/inference/v1" in proxy.base_url(route, "fireworks")
            assert len(route.token) >= 32
            await proxy.end_run(route)

    run(go())


def test_sse_passthrough_key_injection_and_capture(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            cap_path = tmp_path / "run" / "capture.jsonl"
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, cap_path)
            base = proxy.base_url(route, "zai-coding-plan")
            st, body, hdrs = await post(s, base + "/chat/completions", chat("glm-5.3-flash", stream_options={"include_usage": True}),
                                        headers={"X-Client-Secret-Header": "client-hdr-9"})
            assert st == 200
            assert b"Reading." in body and b"[DONE]" in body
            # The upstream got the real key, never the dummy, and none of the client's credentials.
            seen = up.seen[-1]
            assert seen["headers"]["Authorization"] == f"Bearer {ZAI_KEY}"
            assert seen["path"] == "/api/coding/paas/v4/chat/completions"
            summary = await proxy.end_run(route)
            assert summary["forwarded"] == 1 and summary["key_exposure_refusals"] == 0
            recs = read_capture(tmp_path / "run")
            assert (tmp_path / "run" / "capture.jsonl.gz").exists() and not cap_path.exists()
            r = recs[0]
            assert r["status"] == 200 and r["route"] == "zai-coding-plan" and r["path"] == "/chat/completions"
            assert r["request"]["model"] == "glm-5.3-flash"
            assert r["served_model"] == "glm-5.3-flash-served"
            assert r["usage"] == {"input": 1000, "cached": 600, "output": 200, "reasoning": 50}
            assert r["usage_injected"] is False
            assert any("tool_calls" in json.dumps(e) for e in r["response"]["events"])
            raw = raw_capture(tmp_path)
            # Headers are never written, only content types.
            assert "client-hdr-9" not in raw and "hdr-value-1" not in raw and "craze-eval-dummy-key" not in raw
            assert ZAI_KEY not in raw
            assert r["request_content_type"] == "application/json"
            assert r["response_content_type"].startswith("text/event-stream")

    run(go())


def test_usage_injected_when_missing(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "gx", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            base = proxy.base_url(route, "zai-coding-plan")
            st, _, _ = await post(s, base + "/chat/completions", chat("glm-5.3-flash"))
            assert st == 200
            sent = json.loads(up.seen[-1]["body"])
            assert sent["stream_options"] == {"include_usage": True}
            await proxy.end_run(route)
            r = read_capture(tmp_path / "run")[0]
            assert r["usage_injected"] is True
            assert "stream_options" not in r["request"]  # the capture keeps the original body
            assert r["usage"]["input"] == 1000

    run(go())


def test_json_response_and_models_listing(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"accounts/fireworks/models/deepseek-v4p1-flash"}, tmp_path / "run" / "capture.jsonl")
            base = proxy.base_url(route, "fireworks")
            st, body, _ = await post(s, base + "/chat/completions", chat("accounts/fireworks/models/deepseek-v4p1-flash", stream=False))
            assert st == 200 and json.loads(body)["usage"]["prompt_tokens"] == 100
            assert up.seen[-1]["headers"]["Authorization"] == f"Bearer {FW_KEY}"
            async with s.get(base + "/models", headers={"Authorization": "Bearer craze-eval-dummy-key"}) as r:
                assert r.status == 200
            await proxy.end_run(route)
            recs = read_capture(tmp_path / "run")
            assert recs[0]["response"]["body"]["usage"]["completion_tokens"] == 10
            assert recs[0]["cost"] == pytest.approx((100 * 0.45 + 10 * 1.80) / 1e6)
            assert recs[1]["method"] == "GET" and recs[1]["path"] == "/models" and recs[1]["status"] == 200

    run(go())


def test_responses_stream_usage_from_completed(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "codex", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            base = proxy.base_url(route, "meta")
            st, body, _ = await post(s, base + "/responses", {"model": "muse-spark-1.3", "input": "hi", "stream": True})
            assert st == 200 and b"response.completed" in body
            assert up.seen[-1]["headers"]["Authorization"] == f"Bearer {META_KEY}"
            await proxy.end_run(route)
            r = read_capture(tmp_path / "run")[0]
            assert r["usage"] == {"input": 2000, "cached": 1000, "output": 100, "reasoning": 40}
            assert r["served_model"] == "muse-spark-1.3-2026"
            assert r["usage_injected"] is False
            # 1000 uncached at 1.25, 1000 cached at 0.15, 100 out at 4.25
            assert r["cost"] == pytest.approx((1000 * 1.25 + 1000 * 0.15 + 100 * 4.25) / 1e6)

    run(go())


def test_non_target_model_refused_and_recorded(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "opencode", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            base = proxy.base_url(route, "zai-coding-plan")
            st, body, _ = await post(s, base + "/chat/completions", chat("glm-5.3"))
            assert st == 403 and b"model-not-allowed" in body
            assert up.seen == []
            summary = await proxy.end_run(route)
            assert summary["refused"] == {"model-not-allowed": 1}
            r = read_capture(tmp_path / "run")[0]
            assert r["refused"] == "model-not-allowed" and r["request_model"] == "glm-5.3"
            assert led.totals()["requests"] == 0

    run(go())


def test_unpriced_model_refused(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"mystery-model"}, tmp_path / "run" / "capture.jsonl")
            st, body, _ = await post(s, proxy.base_url(route, "meta") + "/chat/completions", chat("mystery-model"))
            assert st == 403 and b"model-unpriced" in body and up.seen == []
            await proxy.end_run(route)

    run(go())


def test_paths_and_upstreams_not_allowlisted(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            base = proxy.base_url(route, "zai-coding-plan")
            st, body, _ = await post(s, base + "/embeddings", {"model": "glm-5.3-flash", "input": "x"})
            assert st == 403 and b"path-not-allowed" in body
            st, _, _ = await post(s, base + "/models", {"model": "glm-5.3-flash"})  # POST /models is not forwarded
            assert st == 403
            # Same host, another prefix (the non-coding Z.AI endpoint) is another upstream.
            other = f"http://127.0.0.1:{proxy.port}/r/{route.token}/api.z.ai/api/paas/v4/chat/completions"
            st, body, _ = await post(s, other, chat("glm-5.3-flash"))
            assert st == 403 and b"upstream-not-allowed" in body
            st, body, _ = await post(s, f"http://127.0.0.1:{proxy.port}/r/{route.token}/evil.example.com/v1/chat/completions", chat("glm-5.3-flash"))
            assert st == 403 and b"upstream-not-allowed" in body
            assert up.seen == []
            summary = await proxy.end_run(route)
            assert summary["refused"] == {"path-not-allowed": 2, "upstream-not-allowed": 2}

    run(go())


def test_route_token_expires_with_the_run(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            base = proxy.base_url(route, "zai-coding-plan")
            await proxy.end_run(route)
            st, body, _ = await post(s, base + "/chat/completions", chat("glm-5.3-flash"))
            assert st == 403 and b"expired-token" in body
            st, body, _ = await post(s, base.replace(route.token, "0" * 32) + "/chat/completions", chat("glm-5.3-flash"))
            assert st == 403 and b"unknown-token" in body
            assert up.seen == []
            log = (tmp_path / "refusals.jsonl").read_text()
            assert "expired-token" in log and "unknown-token" in log and route.token not in log

    run(go())


def test_request_carrying_a_key_is_refused_before_any_upstream_call(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            base = proxy.base_url(route, "zai-coding-plan")
            body = chat("glm-5.3-flash")
            body["messages"].append({"role": "tool", "content": f"cat providers.toml -> api_key = \"{META_KEY}\""})
            st, resp, _ = await post(s, base + "/chat/completions", body)
            assert st == 403 and b"key-exposure" in resp
            # A key in a header is refused the same way.
            st, _, _ = await post(s, base + "/chat/completions", chat("glm-5.3-flash"), headers={"X-Leak": FW_KEY})
            assert st == 403
            assert up.seen == []  # no upstream call happened
            summary = await proxy.end_run(route)
            assert summary["key_exposure_refusals"] == 2 and "key-exposure" in summary["flags"]
            raw = raw_capture(tmp_path)
            assert META_KEY not in raw and FW_KEY not in raw and "<scrubbed-key>" in raw
            assert led.totals()["requests"] == 0

    run(go())


def test_a_key_in_a_response_never_reaches_the_harness(tmp_path):
    """Review r1-c1 finding 4: an upstream that echoes a key -- in a header, in the
    body, or split across two chunks -- reaches the harness redacted, and the capture
    and ledger scrubbed."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            up.echo_key = ZAI_KEY
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            st, body, hdrs = await post(s, proxy.base_url(route, "zai-coding-plan") + "/chat/completions",
                                        chat("glm-5.3-flash", split_key=True))
            assert st == 200
            assert ZAI_KEY.encode() not in body
            assert ZAI_KEY[: len(ZAI_KEY) // 2].encode() + ZAI_KEY[len(ZAI_KEY) // 2 :].encode() not in body
            assert body.count(b"<scrubbed-key>") == 2  # the whole one and the split one
            assert b"leak <scrubbed-key>" in body and b"split <scrubbed-key>" in body
            assert b"[DONE]" in body  # the stream is otherwise intact
            assert not any(ZAI_KEY in v for v in hdrs.values()) and "X-Echo-Auth" not in hdrs
            await proxy.end_run(route)
            raw = raw_capture(tmp_path)
            assert ZAI_KEY not in raw and "<scrubbed-key>" in raw
            assert ZAI_KEY not in (tmp_path / "ledger.jsonl").read_text()

    run(go())


def test_a_hang_up_after_the_exchange_finished_is_harmless():
    """Seen in the C1 fix smoke: the harness hung up after the upstream stream had
    been read to the end; marking it gone must not touch the finished read timeout."""
    from crazeeval.proxy import Exchange

    async def go():
        ex = Exchange(asyncio.get_running_loop())
        async with asyncio.timeout(None) as tm:
            ex.timeout = tm
        ex.mark_gone()  # the timeout context has exited
        assert ex.gone and ex.state["client_gone"]

    run(go())


def test_redactor_holds_back_only_a_possible_key_prefix():
    from crazeeval.proxy import Redactor

    r = Redactor((b"sk-SECRET-123",))
    assert r.feed(b"hello ") == b"hello "  # nothing held: no key prefix at the end
    assert r.feed(b"sk-SEC") == b""  # could be a key: held
    assert r.feed(b"RET-123 bye") == b"<scrubbed-key> bye"
    assert r.feed(b"sk") == b""
    assert r.flush() == b"sk"  # at the end, a partial prefix is just text


def test_capture_is_a_journal_while_the_run_is_live(tmp_path):
    """Review r1-c1 finding 9: a started request and its streamed events are on disk
    before it finishes; a killed proxy leaves them readable."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            cap = tmp_path / "run" / "capture.jsonl"
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, cap)
            url = proxy.base_url(route, "zai-coding-plan") + "/chat/completions"
            async with aiohttp.ClientSession() as s2:
                async with s2.post(url, data=json.dumps(chat("glm-5.3-flash", hang=True)),
                                   headers={"Content-Type": "application/json"}) as r:
                    await r.content.readany()
                    await asyncio.sleep(0.2)
                    kinds = [json.loads(x)["kind"] for x in cap.read_text().splitlines()]
                    assert kinds[0] == "start" and "events" in kinds and "end" not in kinds
                    # What a killed proxy would leave: the request, with its tool call.
                    rec = read_capture(cap)[0]
                    assert rec["incomplete"] is True and rec["request"]["model"] == "glm-5.3-flash"
                    assert any("tool_calls" in json.dumps(e) for e in rec["response"]["events"])
            await proxy.end_run(route, grace=1.0)
            assert not cap.exists() and (tmp_path / "run" / "capture.jsonl.gz").exists()
            rec = read_capture(tmp_path / "run")[0]
            assert "incomplete" not in rec and rec["status"] == 200

    run(go())


def test_escaped_key_in_a_body_is_refused(tmp_path):
    """Review r1-c1 finding 5: a key the raw body carries only JSON-escaped is found in
    the decoded strings; and the body usage injection would re-serialise is checked."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "zai-coding-plan") + "/chat/completions"
            escaped = "".join(f"\\u{ord(c):04x}" for c in META_KEY[:3]) + META_KEY[3:]
            raw = ('{"model": "glm-5.3-flash", "stream": true, "messages": [{"role": "user", "content": "%s"}]}' % escaped).encode()
            assert META_KEY.encode() not in raw  # the raw scan alone would miss it
            async with s.post(url, data=raw, headers={"Content-Type": "application/json"}) as r:
                assert r.status == 403 and b"key-exposure" in await r.read()
            # As a JSON object key, too.
            raw2 = ('{"model": "glm-5.3-flash", "messages": [], "%s": 1}' % escaped).encode()
            async with s.post(url, data=raw2, headers={"Content-Type": "application/json"}) as r:
                assert r.status == 403
            assert up.seen == []
            summary = await proxy.end_run(route)
            assert summary["key_exposure_refusals"] == 2
            assert META_KEY not in raw_capture(tmp_path)

    run(go())


def test_a_route_closed_while_the_body_was_read_is_refused(tmp_path):
    """Review r1-c1 finding 7: a request that started while its route was open, whose
    body finished only after the run ended, is refused -- never forwarded."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "zai-coding-plan") + "/chat/completions"
            body = json.dumps(chat("glm-5.3-flash")).encode()
            release = asyncio.Event()

            async def slow_body():
                yield body[:10]
                await release.wait()
                yield body[10:]

            async def send():
                async with aiohttp.ClientSession() as s2:
                    async with s2.post(url, data=slow_body(), headers={"Content-Type": "application/json"}) as r:
                        return r.status, await r.read()

            sender = asyncio.create_task(send())
            await asyncio.sleep(0.3)  # the handler is now waiting for the body
            closer = asyncio.create_task(proxy.end_run(route, grace=3.0))
            await asyncio.sleep(0.2)
            assert not route.active
            release.set()  # the body completes during the close's grace period
            status, resp = await sender
            await closer
            assert status == 403 and b"expired-token" in resp
            assert up.seen == [] and led.totals()["requests"] == 0

    run(go())


def test_multiple_completions_are_refused(tmp_path):
    """Review r1-c1 finding 11: n=128 would spend 128x the reservation."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"accounts/fireworks/models/kimi-k3"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "fireworks") + "/chat/completions"
            for extra in ({"n": 128}, {"n": 2}, {"best_of": 3}, {"n": True}, {"max_tokens": "big"}, {"max_tokens": -1}):
                st, body, _ = await post(s, url, chat("accounts/fireworks/models/kimi-k3", **extra))
                assert st == 400, extra
            assert up.seen == [] and led.totals()["requests"] == 0
            st, _, _ = await post(s, url, chat("accounts/fireworks/models/kimi-k3", n=1))
            assert st == 200
            summary = await proxy.end_run(route)
            assert summary["refused"] == {"multi-completion": 4, "bad-output-limit": 2}

    run(go())


def test_reservation_uses_the_largest_output_limit(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            body = chat("muse-spark-1.3", stream=False, max_tokens=100, max_completion_tokens=50_000, no_usage=True)
            await post(s, proxy.base_url(route, "meta") + "/chat/completions", body)
            await proxy.end_run(route)
            reserve = [json.loads(x) for x in (tmp_path / "ledger.jsonl").read_text().splitlines() if '"reserve"' in x][0]
            assert reserve["reservation"] >= 50_000 * 4.25 / 1e6

    run(go())


def test_trailing_slash_endpoint_still_gets_usage(tmp_path):
    """Review r1-c1 finding 10: /chat/completions/ is normalised once."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "gx", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            st, _, _ = await post(s, proxy.base_url(route, "zai-coding-plan") + "/chat/completions/", chat("glm-5.3-flash"))
            assert st == 200
            assert json.loads(up.seen[-1]["body"])["stream_options"] == {"include_usage": True}
            assert up.seen[-1]["path"] == "/api/coding/paas/v4/chat/completions"
            await proxy.end_run(route)
            r = read_capture(tmp_path / "run")[0]
            assert r["usage_injected"] is True and r["usage"]["input"] == 1000

    run(go())


def test_disconnect_before_headers_is_still_drained(tmp_path):
    """Review r1-c1 finding 8: a harness that hangs up while the upstream is still
    working on the headers does not lose the usage."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "opencode", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "meta") + "/chat/completions"
            body = json.dumps(chat("muse-spark-1.3", delay_headers=0.6, max_tokens=32000,
                                   stream_options={"include_usage": True}))
            async with aiohttp.ClientSession() as s2:
                try:
                    await asyncio.wait_for(s2.post(url, data=body, headers={"Content-Type": "application/json"}), 0.2)
                except asyncio.TimeoutError:
                    pass  # hung up before any header arrived
            await proxy.end_run(route, grace=5.0)
            settle = [json.loads(x) for x in (tmp_path / "ledger.jsonl").read_text().splitlines() if '"settle"' in x][0]
            assert settle["usage"] == {"input": 1000, "cached": 600, "output": 200, "reasoning": 50}
            assert settle["charged"] < settle["reservation"]
            r = read_capture(tmp_path / "run")[0]
            assert r["drained"] is True and r["client_disconnected"] is True and r["status"] == 200

    run(go())


def test_disconnect_keeps_the_reservation(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "meta") + "/chat/completions"
            body = json.dumps(chat("muse-spark-1.3", hang=True, max_tokens=1000))
            async with aiohttp.ClientSession() as s2:
                async with s2.post(url, data=body, headers={"Content-Type": "application/json"}) as r:
                    await r.content.readany()  # the first chunk arrived; now walk away
            await asyncio.sleep(0.3)
            await proxy.end_run(route, grace=2.0)
            t = led.totals()
            assert t["requests"] == 1 and t["open_reservations"] == 0
            settle = first_settle(tmp_path)
            assert settle["usage"] is None and settle["charged"] == settle["reservation"] > 0
            r = read_capture(tmp_path / "run")[0]
            assert r["usage"] is None

    run(go())


def test_a_hung_up_stream_is_drained_and_reconciled(tmp_path):
    """A harness that drops a stream (a title call cut off at exit) still gets its usage
    reconciled: the proxy reads the rest of the upstream stream."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "opencode", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "meta") + "/chat/completions"
            body = json.dumps(chat("muse-spark-1.3", slow_finish=True, max_tokens=32000,
                                   stream_options={"include_usage": True}))
            async with aiohttp.ClientSession() as s2:
                async with s2.post(url, data=body, headers={"Content-Type": "application/json"}) as r:
                    await r.content.readany()
            await proxy.end_run(route, grace=5.0)
            settle = first_settle(tmp_path)
            assert settle["usage"] == {"input": 1000, "cached": 600, "output": 200, "reasoning": 50}
            assert settle["charged"] == pytest.approx((400 * 1.25 + 600 * 0.15 + 200 * 4.25) / 1e6)
            assert settle["charged"] < settle["reservation"]
            r = read_capture(tmp_path / "run")[0]
            assert r["drained"] is True and r["client_disconnected"] is True

    run(go())


def test_missing_usage_keeps_the_reservation(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            st, _, _ = await post(s, proxy.base_url(route, "meta") + "/chat/completions",
                                  chat("muse-spark-1.3", stream=False, no_usage=True, max_tokens=2000))
            assert st == 200
            await proxy.end_run(route)
            t = led.totals()
            body_len = len(json.dumps(chat("muse-spark-1.3", stream=False, no_usage=True, max_tokens=2000)))
            expected = (body_len * 1.25 + 2000 * 4.25) / 1e6
            assert t["committed"] == pytest.approx(expected, abs=1e-6)

    run(go())


def test_budget_stop_and_run_cap(tmp_path):
    async def go():
        # A run cap smaller than one 16k-token reservation at muse-spark-1.3's output price does not refuse the
        # first request: the cap counts what the run has actually spent, not the next request's worst case.
        # Once the settled spend reaches the cap, the next request is refused.
        async with rig(tmp_path, run_cap=1e-9) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            st, body, _ = await post(s, proxy.base_url(route, "meta") + "/chat/completions", chat("muse-spark-1.3"))
            assert st == 200 and len(up.seen) == 1
            st, body, _ = await post(s, proxy.base_url(route, "meta") + "/chat/completions", chat("muse-spark-1.3"))
            assert st == 402 and b"budget-capped" in body and len(up.seen) == 1
            summary = await proxy.end_run(route)
            assert "budget-capped" in summary["flags"]
        async with rig(tmp_path / "b", cap=0.01) as (proxy, up, led, s):
            route = proxy.register_run("r2", "craze", {"muse-spark-1.3"}, tmp_path / "b" / "run" / "capture.jsonl")
            st, body, _ = await post(s, proxy.base_url(route, "meta") + "/chat/completions", chat("muse-spark-1.3"))
            assert st == 402 and b"budget-stop" in body and proxy.budget_stopped
            await proxy.end_run(route)

    (tmp_path / "b").mkdir()
    run(go())
