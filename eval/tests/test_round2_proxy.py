"""Review r2-c1: reason phrases, stalled clients, bounded parsing, killed-proxy
captures, and reservations for requests with no output limit (items A-D)."""

from __future__ import annotations

import asyncio
import gzip
import json
import socket
from pathlib import Path

import pytest

from crazeeval import proxy as proxymod
from crazeeval.capture import SSEParser, read_capture
from fakes import ZAI_KEY, rig


def run(coro):
    return asyncio.run(coro)


def chat(model, stream=True, **extra):
    body = {"model": model, "messages": [{"role": "user", "content": "hi"}], "stream": stream}
    body.update(extra)
    return body


def test_a_key_in_the_reason_phrase_is_dropped(tmp_path):
    """Item A: the upstream HTTP reason phrase never carries a key to the harness."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            up.echo_key = ZAI_KEY
            route = proxy.register_run("r1", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "zai-coding-plan") + "/chat/completions"
            async with s.post(url, data=json.dumps(chat("glm-5.3-flash", stream=False, reason_key=True)),
                              headers={"Content-Type": "application/json"}) as r:
                assert r.status == 400
                assert ZAI_KEY not in (r.reason or "") and r.reason == "Bad Request"
            await proxy.end_run(route)
            assert proxy._reason("Upstream Busy") == "Upstream Busy"

    run(go())


def test_a_client_that_stops_reading_is_drained_without_buffering(tmp_path, monkeypatch):
    """Item B: a connected harness that stops reading neither blocks accounting nor
    makes the proxy buffer the stream: the exchange switches to discard-and-drain."""
    monkeypatch.setattr(proxymod, "QUEUE_LIMIT_BYTES", 256 * 1024)
    monkeypatch.setattr(proxymod, "WRITE_DEADLINE_S", 0.5)

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            path = proxy.base_url(route, "meta").split(str(proxy.port), 1)[1] + "/chat/completions"
            body = json.dumps(chat("muse-spark-1.3", big=256, stream_options={"include_usage": True})).encode()
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
            sock.connect(("127.0.0.1", proxy.port))
            reader, writer = await asyncio.open_connection(sock=sock, limit=4096)
            writer.write(b"POST " + path.encode() + b" HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n"
                         b"Content-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body)
            await writer.drain()
            await asyncio.sleep(3.0)  # connected, never reading; 16 MiB comes upstream
            await proxy.end_run(route, grace=10.0)
            writer.close()
            rec = read_capture(tmp_path / "run")[0]
            assert rec["client_stalled"] is True and rec["drained"] is True
            assert rec["max_queued_bytes"] <= 256 * 1024
            assert rec["usage"] == {"input": 1000, "cached": 0, "output": 200, "reasoning": 0}
            settle = [json.loads(x) for x in (tmp_path / "ledger.jsonl").read_text().splitlines() if '"settle"' in x][0]
            assert settle["usage"] is not None and settle["charged"] < settle["reservation"]

    run(go())


def test_sse_parser_is_bounded_by_bytes():
    """Item B: an oversized event is skipped to its separator; past the stored-bytes
    budget only accounting events are kept."""
    p = SSEParser(max_event_bytes=100, max_stored_bytes=200)
    p.feed(b"data: " + b"x" * 150)  # no separator yet, already over the event bound
    assert len(p._buf) <= 100
    p.feed(b"y" * 1000 + b"\n\ndata: {\"a\": 1}\n\n")
    assert p.events == [{"a": 1}] and p.dropped == 1 and p.dropped_bytes > 1000
    for i in range(20):
        p.feed(b'data: {"n": %d, "pad": "0123456789"}\n\n' % i)
    p.feed(b'data: {"choices": [], "usage": {"prompt_tokens": 5, "completion_tokens": 1}}\n\n')
    p.feed(b'data: {"type": "response.completed", "response": {"usage": {}}}\n\n')
    assert p.stored_bytes <= 200 + 200  # accounting events may pass the budget, nothing else
    assert p.events[-2]["usage"]["prompt_tokens"] == 5 and p.events[-1]["type"] == "response.completed"
    assert p.dropped > 1


def test_sse_parser_bounds_repeated_accounting_events(tmp_path):
    """Review r3-c1 item B: past the storage limit, a stream of repeated small
    ``usage`` events replaces bounded accounting state instead of appending forever
    -- stored events stay O(1) while the final usage is still reported."""
    from crazeeval.capture import usage_and_model

    p = SSEParser(max_event_bytes=1_000_000, max_stored_bytes=100)
    for i in range(1000):
        p.feed(b'data: {"choices": [], "usage": {"prompt_tokens": %d, "completion_tokens": 1}}\n\n' % i)
    assert len(p.events) <= 5  # bounded, not 1000
    usage, _ = usage_and_model({"events": p.events})
    assert usage is not None and usage.input == 999 and usage.output == 1


def test_a_record_cut_mid_character_does_not_lose_the_capture(tmp_path):
    """Item C: a proxy killed mid-write can cut a record inside a UTF-8 character;
    every earlier record still reads."""
    good = [{"kind": "record", "seq": 1, "status": 200, "note": "café"},
            {"kind": "start", "seq": 2, "request": {"model": "m", "content": "naïve"}}]
    raw = b"".join(json.dumps(r, ensure_ascii=False).encode() + b"\n" for r in good)
    cut = json.dumps({"kind": "events", "seq": 2, "events": [{"t": "ééé"}]}, ensure_ascii=False).encode()
    cut = cut[: cut.index("é".encode()) + 1]  # half of a two-byte character
    (tmp_path / "capture.jsonl").write_bytes(raw + cut)
    recs = read_capture(tmp_path / "capture.jsonl")
    assert [r["seq"] for r in recs] == [1, 2] and recs[0]["note"] == "café" and recs[1]["incomplete"] is True
    # A truncated gzip keeps what decompressed.
    gz = gzip.compress(raw * 50)
    (tmp_path / "t").mkdir()
    (tmp_path / "t" / "capture.jsonl.gz").write_bytes(gz[: len(gz) - 20])
    assert len(read_capture(tmp_path / "t")) >= 1


def test_no_output_limit_reserves_the_model_maximum_and_forwards_unchanged(tmp_path):
    """Item D: a request naming no output limit is reserved at the model's maximum;
    the request that goes upstream is exactly what the harness sent."""

    async def go():
        async with rig(tmp_path, max_output={"muse-spark-1.3": 50_000}) as (proxy, up, led, s):
            route = proxy.register_run("r1", "codex", {"muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            body = {"model": "muse-spark-1.3", "input": "hi", "stream": True}
            raw = json.dumps(body).encode()
            async with s.post(proxy.base_url(route, "meta") + "/responses", data=raw,
                              headers={"Content-Type": "application/json"}) as r:
                assert r.status == 200
                await r.read()
            await proxy.end_run(route)
            assert up.seen[-1]["body"] == raw  # untouched: no limit added
            reserve = [json.loads(x) for x in (tmp_path / "ledger.jsonl").read_text().splitlines() if '"reserve"' in x][0]
            assert reserve["reservation"] == pytest.approx((len(raw) * 1.25 + 50_000 * 4.25) / 1e6)

    run(go())


def test_snapshot_max_output_prefers_the_owner_tables():
    from crazeeval.config import Snapshot, load_models
    from crazeeval.pricing import MODEL_MAX_OUTPUT_DEFAULT

    ms = load_models()
    snap = Snapshot(dir=Path("snap"), hash="x", config={"models": {
        "muse-spark-1.3": {"craze": {"max_output_tokens": 131072}, "gx": {"max_completion_tokens": 65536}},
        "fireworks/kimi-k3": {"craze": {}, "gx": {}},
    }})
    got = snap.max_output([ms["muse-spark-1.3"], ms["fireworks/kimi-k3"], ms["glm-5.3"]])
    assert got == {"muse-spark-1.3": 131072, "accounts/fireworks/models/kimi-k3": MODEL_MAX_OUTPUT_DEFAULT,
                   "glm-5.3": MODEL_MAX_OUTPUT_DEFAULT}
