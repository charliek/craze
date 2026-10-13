"""Anthropic's Messages API and Muse Wonder in the eval (craze plan 047 D11): the route,
the credential header, the accounting of Messages streams, cache-write and long-context
pricing, a reconciled ledger, and the generated homes."""

from __future__ import annotations

import asyncio
import json
import tomllib
from pathlib import Path

import pytest

from crazeeval import paths
from crazeeval.capture import (
    SSEParser,
    classify,
    metrics,
    read_capture,
    request_effort,
    response_tool_calls,
    tool_results,
    usage_and_model,
)
from crazeeval.config import load_models
from crazeeval.homes import ANTHROPIC_API_ENV, HOMES, anthropic_api, craze_home_multi
from crazeeval.keys import ANTHROPIC_BASE, load_providers
from crazeeval.ledger import Ledger
from crazeeval.pricing import Price, Usage, cost, list_reservation, load_prices, reservation
from fakes import (
    ANT_KEY,
    META_KEY,
    MSG_START_USAGE,
    messages_events,
    rig,
    sse_named,
)
from test_homes_env import FAKE, fake_snapshot, owner_tree

OPUS, HAIKU = "claude-opus-5-5", "claude-haiku-5-5"
DUMMY = {"x-api-key": paths.DUMMY_KEY, "anthropic-version": "2023-06-01", "Content-Type": "application/json"}


def run(coro):
    return asyncio.run(coro)


def msg(model, stream=True, max_tokens=4096, **extra):
    body = {"model": model, "max_tokens": max_tokens, "stream": stream,
            "messages": [{"role": "user", "content": "hi"}], "tools": [{"name": "bash", "input_schema": {}}]}
    body.update(extra)
    return body


async def post(session, url, body, headers=DUMMY):
    async with session.post(url, data=json.dumps(body), headers=headers) as r:
        return r.status, await r.read()


def ledger_lines(tmp_path) -> list[dict]:
    return [json.loads(x) for x in (tmp_path / "ledger.jsonl").read_text().splitlines()]


# The exchange the fake answers with, priced by hand (Opus 5.5: input 4.0, cache hit
# 0.2, 5-minute write 5.0, output 20.0 per 1M).
EXPECT_USAGE = {"input": 1200 + 50000 + 3000, "cached": 50000, "cache_write": 3000, "output": 800, "reasoning": 120}
EXPECT_COST = (1200 * 4.0 + 50000 * 0.2 + 3000 * 5.0 + 800 * 20.0) / 1e6


# -- the route ---------------------------------------------------------------------------


def test_messages_route_admits_only_messages_and_models(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {OPUS, "muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            ant, meta = proxy.base_url(route, "anthropic"), proxy.base_url(route, "meta")
            assert ant == f"http://127.0.0.1:{proxy.port}/r/{route.token}/api.anthropic.com/v1"
            st, _ = await post(s, ant + "/messages", msg(OPUS))
            assert st == 200
            async with s.get(ant + "/models", headers=DUMMY) as r:
                assert r.status == 200
            st, body = await post(s, ant + "/chat/completions", msg(OPUS))  # not on Anthropic's route
            assert st == 403 and b"path-not-allowed" in body
            st, body = await post(s, meta + "/messages", msg("muse-spark-1.3"))  # not on any other route
            assert st == 403 and b"path-not-allowed" in body
            assert [x["path"] for x in up.seen] == ["/v1/messages", "/v1/models"]
            summary = await proxy.end_run(route)
            assert summary["refused"] == {"path-not-allowed": 2}

    run(go())


def test_messages_key_goes_in_x_api_key_and_the_version_passes(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "opencode", {OPUS, "muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            hdrs = {**DUMMY, "anthropic-beta": "thinking-binding-controls-2026-08-01",
                    "Authorization": f"Bearer {paths.DUMMY_KEY}"}
            st, _ = await post(s, proxy.base_url(route, "anthropic") + "/messages", msg(OPUS), headers=hdrs)
            assert st == 200
            seen = {k.lower(): v for k, v in up.seen[-1]["headers"].items()}
            assert seen["x-api-key"] == ANT_KEY and "authorization" not in seen  # the key, and no bearer
            assert seen["anthropic-version"] == "2023-06-01"
            assert seen["anthropic-beta"] == "thinking-binding-controls-2026-08-01"
            # Every other provider keeps its bearer, and a harness's x-api-key never reaches it.
            st, _ = await post(s, proxy.base_url(route, "meta") + "/chat/completions",
                               {"model": "muse-spark-1.3", "messages": [], "stream": False}, headers=hdrs)
            assert st == 200
            seen = {k.lower(): v for k, v in up.seen[-1]["headers"].items()}
            assert seen["authorization"] == f"Bearer {META_KEY}" and "x-api-key" not in seen
            await proxy.end_run(route)

    run(go())


def test_a_loaded_key_in_x_api_key_is_a_key_exposure(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "gx", {OPUS}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "anthropic") + "/messages"
            st, body = await post(s, url, msg(OPUS), headers={**DUMMY, "x-api-key": ANT_KEY})
            assert st == 403 and b"key-exposure" in body
            other = {"anthropic-version": "2023-06-01", "Content-Type": "application/json", "X-Api-Key": META_KEY}
            st, body = await post(s, url, msg(OPUS), headers=other)  # any loaded key, however the header is spelt
            assert st == 403 and b"key-exposure" in body
            assert up.seen == []
            st, _ = await post(s, url, msg(OPUS))  # the dummy key in x-api-key is fine
            assert st == 200 and len(up.seen) == 1
            summary = await proxy.end_run(route)
            assert summary["key_exposure_refusals"] == 2
            assert ANT_KEY not in (tmp_path / "ledger.jsonl").read_text()

    run(go())


# -- accounting --------------------------------------------------------------------------


def test_messages_usage_is_message_start_overwritten_by_each_delta():
    # message_start's output_tokens (1) and the first delta's (300) are replaced by the
    # last cumulative count (800), never added to it.
    evs = messages_events(OPUS, deltas=[{"output_tokens": 300}, {"output_tokens": 800}])
    usage, model = usage_and_model({"events": evs})
    assert model == OPUS
    assert usage.to_dict() == {"input": 54200, "cached": 50000, "cache_write": 3000, "output": 800, "reasoning": 0}
    # A delta that reports the input and cache counts overwrites them too; a null count
    # keeps the earlier one, and thinking tokens are reasoning.
    evs = messages_events(OPUS, deltas=[
        {"input_tokens": 1300, "cache_read_input_tokens": 51000, "cache_creation_input_tokens": None, "output_tokens": 500},
        {"output_tokens": 900, "output_tokens_details": {"thinking_tokens": 200}},
    ])
    usage, _ = usage_and_model({"events": evs})
    assert usage.to_dict() == {"input": 1300 + 51000 + 3000, "cached": 51000, "cache_write": 3000, "output": 900,
                               "reasoning": 200}


def test_a_messages_stream_without_its_delta_has_no_usage():
    # Cut after message_start: the input is known, the output is not -- no usage, so the
    # reservation stands (a stream whose usage never came).
    assert usage_and_model({"events": messages_events(OPUS, cut=True)}) == (None, OPUS)
    # A delta with no message_start before it: the input is not known either.
    assert usage_and_model({"events": messages_events(OPUS)[1:]})[0] is None
    # message_start and a delta, but one of them without a count craze needs.
    no_input = {k: v for k, v in MSG_START_USAGE.items() if k != "input_tokens"}
    assert usage_and_model({"events": messages_events(OPUS, start_usage=no_input)})[0] is None


def test_messages_usage_survives_the_capture_limits():
    # Every event is over the limit: only accounting events are kept, message_start among
    # them (its counts are under message.usage), and repeated deltas fold count by count.
    p = SSEParser(limit_events=0)
    p.feed(sse_named(messages_events(OPUS, deltas=[
        {"input_tokens": 1300, "output_tokens": 400}, {"output_tokens": 950}])))
    p.close()
    kinds = [e.get("type") for e in p.events]
    assert kinds == ["message_start", "message_delta"] and p.dropped > 0
    usage, model = usage_and_model({"events": p.events})
    assert model == OPUS
    assert usage.to_dict() == {"input": 1300 + 50000 + 3000, "cached": 50000, "cache_write": 3000, "output": 950,
                               "reasoning": 0}


def test_a_messages_body_and_its_tool_calls():
    body = {"type": "message", "model": OPUS, "content": [{"type": "text", "text": "OK"},
                                                          {"type": "tool_use", "id": "t9", "name": "read", "input": {"p": 1}}],
            "usage": {**MSG_START_USAGE, "output_tokens": 7}}
    usage, model = usage_and_model({"body": body})
    assert model == OPUS and usage.to_dict() == {"input": 54200, "cached": 50000, "cache_write": 3000, "output": 7,
                                                 "reasoning": 0}
    assert response_tool_calls({"body": body}) == [{"id": "t9", "name": "read", "arguments": {"p": 1}}]
    # The stream's tool_use block, its input assembled from the fragments.
    assert response_tool_calls({"events": messages_events(OPUS)}) == [
        {"id": "toolu_1", "name": "bash", "arguments": {"command": "go test ./..."}}]
    # A later request's tool_result block is the call's result; the effort and a
    # top-level system prompt are read from Messages requests.
    req = {"model": OPUS, "system": [{"type": "text", "text": "You are a title generator."}],
           "output_config": {"effort": "medium"}, "thinking": {"type": "adaptive"},
           "messages": [{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1",
                                                      "content": [{"type": "text", "text": "ok  craze 0.4s"}]}]}]}
    assert tool_results([{"request": req}]) == {"toolu_1": "ok  craze 0.4s"}
    assert request_effort(req) == {"thinking": {"type": "adaptive"}, "output_config.effort": "medium"}
    assert classify({"request": {**req, "tools": [{"name": "bash"}]}}) == "aux"  # the title prompt, by its system


# -- pricing -----------------------------------------------------------------------------


def test_cache_writes_are_priced_apart_from_cache_reads():
    prices = load_prices()
    opus = prices[OPUS]
    u = Usage(**EXPECT_USAGE)
    assert cost(opus, u) == pytest.approx(EXPECT_COST, rel=1e-12)
    assert cost(opus, u, list_price=True) == pytest.approx(EXPECT_COST, rel=1e-12)  # Anthropic's list is the ledger
    # A price with no cache_write charges none (the default): Wonder reports no writes.
    assert prices["muse-wonder-1.0"].cache_write == 0.0
    # Writes never push the uncached part below zero.
    assert cost(opus, Usage(input=100, cached=60, cache_write=90, output=0)) == pytest.approx(
        (60 * 0.2 + 40 * 5.0) / 1e6, rel=1e-12)
    # The record shape: cache_write only when there were writes.
    assert "cache_write" not in Usage(input=5, output=1).to_dict()
    assert Usage(input=5, cache_write=2, output=1).to_dict()["cache_write"] == 2


def test_the_reservation_takes_the_dearer_of_input_and_cache_write():
    opus = load_prices()[OPUS]
    body_bytes, max_tokens = 4096, 2048
    res = reservation(opus, body_bytes, max_tokens)
    assert res == pytest.approx((4096 * 5.0 + 2048 * 20.0) / 1e6, rel=1e-12)  # 5.0, the write, over 4.0
    assert list_reservation(opus, body_bytes, max_tokens) == pytest.approx(res, rel=1e-12)
    # Still an upper bound when every byte is a token written to the cache.
    worst = Usage(input=body_bytes, cache_write=body_bytes, output=max_tokens)
    assert cost(opus, worst) <= res + 1e-12


def test_haiku_long_context_is_per_request_and_includes_the_cache():
    haiku = load_prices()[HAIKU]
    base = lambda u: (u.input - u.cached - u.cache_write) * 0.1 + u.cached * 0.01 + u.cache_write * 0.125 + u.output * 0.5
    long = lambda u: (u.input - u.cached - u.cache_write) * 0.5 + u.cached * 0.05 + u.cache_write * 0.625 + u.output * 2.5
    at = Usage(input=100_000, cached=90_000, cache_write=5_000, output=1_000)  # exactly the threshold: base
    over = Usage(input=100_001, cached=90_000, cache_write=5_000, output=1_000)  # one more cache-read token: long
    assert cost(haiku, at) == pytest.approx(base(at) / 1e6, rel=1e-12)
    assert cost(haiku, over) == pytest.approx(long(over) / 1e6, rel=1e-12)
    # A body no longer than the threshold cannot hold a long prompt; a longer one may.
    assert reservation(haiku, 100_000, 10) == pytest.approx((100_000 * 0.125 + 10 * 0.5) / 1e6, rel=1e-12)
    assert reservation(haiku, 100_001, 10) == pytest.approx((100_001 * 0.625 + 10 * 2.5) / 1e6, rel=1e-12)


def test_a_long_context_tier_must_be_whole(tmp_path):
    good = '[models.m]\ninput = 1.0\ncached_input = 0.1\noutput = 2.0\n'
    tier = '[models.m.long_context]\ninput = 2.0\ncached_input = 0.2\ncache_write = 2.5\noutput = 4.0\n'
    (tmp_path / "ok.toml").write_text(good + "long_context_above = 10\n" + tier)
    assert load_prices(tmp_path / "ok.toml")["m"].long_context.cache_write == 2.5
    for name, text in [("no-threshold", good + tier), ("no-table", good + "long_context_above = 10\n"),
                       ("zero", good + "long_context_above = 0\n" + tier),
                       ("partial", good + "long_context_above = 10\n" + tier.replace("cache_write = 2.5\n", ""))]:
        (tmp_path / f"{name}.toml").write_text(text)
        with pytest.raises(ValueError):
            load_prices(tmp_path / f"{name}.toml")


def test_prices_and_efforts_follow_crazes_catalog():
    """Wonder's and the Claude models' prices, tier and pinned efforts are craze's own."""
    cat = tomllib.loads((paths.REPO_ROOT / "internal/harness/modeltable/catalog.toml").read_text())
    prices, models = load_prices(), load_models()
    default = cat["anthropic_defaults"]["default_effort"]
    for key in ("muse-wonder-1.0", "anthropic/claude-opus-5-5", "anthropic/claude-sonnet-5-5",
                "anthropic/claude-haiku-5-5"):
        em, c = models[key], cat["models"][key]
        assert em.provider == c["provider"] and em.wire_model == c["wire_model"] and em.craze == key
        assert em.effort == (c.get("default_effort") or default) and em.effort in c["efforts"]
        p, cc = prices[em.wire_model], c["cost"]
        assert (p.input, p.cached_input, p.cache_write, p.output) == (
            cc["input"], cc["cache_read"], cc.get("cache_write", 0.0), cc["output"]), key
        assert (p.list_input, p.list_cached_input, p.list_cache_write, p.list_output) == (
            p.input, p.cached_input, p.cache_write, p.output)
        if "long_context" in cc:
            lc = cc["long_context"]
            assert p.long_context_above == cc["long_context_above"]
            assert (p.long_context.input, p.long_context.cached_input, p.long_context.cache_write,
                    p.long_context.output) == (lc["input"], lc["cache_read"], lc["cache_write"], lc["output"])
        else:
            assert p.long_context is None
        assert em.provider != "anthropic" or (p.cache_write > 0 and not em.supports("codex"))


# -- the ledger --------------------------------------------------------------------------


def test_one_messages_exchange_reconciles_in_the_ledger(tmp_path):
    body = msg(OPUS)

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {OPUS}, tmp_path / "run" / "capture.jsonl")
            st, raw = await post(s, proxy.base_url(route, "anthropic") + "/messages", body)
            assert st == 200 and b"message_stop" in raw
            await proxy.end_run(route)

    run(go())
    body_bytes = len(json.dumps(body))
    reserve, settle = ledger_lines(tmp_path)
    assert reserve["kind"] == "reserve" and settle["kind"] == "settle" and reserve["id"] == settle["id"]
    assert reserve["model"] == OPUS and reserve["provider"] == "anthropic"
    assert reserve["reservation"] == pytest.approx((body_bytes * 5.0 + 4096 * 20.0) / 1e6, rel=1e-12)
    assert reserve["list_reservation"] == pytest.approx(reserve["reservation"], rel=1e-12)
    assert settle["usage"] == EXPECT_USAGE
    assert settle["cost"] == pytest.approx(EXPECT_COST, rel=1e-12)
    assert settle["list_cost"] == pytest.approx(EXPECT_COST, rel=1e-12)
    assert settle["charged"] == settle["cost"] < reserve["reservation"]
    led = Ledger(tmp_path / "ledger.jsonl")
    t = led.totals()
    led.close()
    assert t["committed"] == pytest.approx(EXPECT_COST, rel=1e-9) and t["reserved"] == 0 and t["open_reservations"] == 0
    rec = read_capture(tmp_path / "run")[0]
    assert rec["usage"] == EXPECT_USAGE and rec["served_model"] == OPUS and rec["charged"] == settle["charged"]
    m = metrics([rec])
    assert m["tokens"] == {"input": 54200, "cached": 50000, "cache_write": 3000, "output": 800, "reasoning": 120}
    assert m["cost"] == round(EXPECT_COST, 6) and m["tool_calls"] == 1 and m["exec_calls"] == 1


def test_a_cut_messages_stream_keeps_its_reservation(tmp_path):
    body = msg(HAIKU, max_tokens=1000, test_cut=True)

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {HAIKU}, tmp_path / "run" / "capture.jsonl")
            st, _ = await post(s, proxy.base_url(route, "anthropic") + "/messages", body)
            assert st == 200
            await proxy.end_run(route)

    run(go())
    reserve, settle = ledger_lines(tmp_path)
    assert settle["usage"] is None and settle["cost"] is None
    assert settle["charged"] == reserve["reservation"] == pytest.approx(
        (len(json.dumps(body)) * 0.125 + 1000 * 0.5) / 1e6, rel=1e-12)  # a short body: Haiku's base tier


# -- homes and keys ----------------------------------------------------------------------


def test_the_craze_home_reaches_anthropic_through_the_override(tmp_path):
    snap = fake_snapshot(tmp_path)
    ms = load_models()
    base = f"http://127.0.0.1:4242/r/{'b' * 32}/api.anthropic.com/v1"
    env = HOMES["craze"](tmp_path / "h", ms["anthropic/claude-haiku-5-5"], snap, base)
    assert env == {"CRAZE_HOME": f"{paths.SANDBOX_HOME}/.craze",
                   ANTHROPIC_API_ENV: f"http://127.0.0.1:4242/r/{'b' * 32}/api.anthropic.com"}
    native = tmp_path / "h" / ".craze" / "native"
    prov = tomllib.loads((native / "providers.toml").read_text())["providers"]
    assert prov == {"anthropic": {"driver": "anthropic", "api_key": paths.DUMMY_KEY}}  # no base_url
    doc = tomllib.loads((native / "models.toml").read_text())
    assert doc["catalog"] is False and list(doc["models"]) == ["anthropic/claude-haiku-5-5"]
    assert doc["models"]["anthropic/claude-haiku-5-5"]["default_effort"] == "medium"
    # Any other model: its route in base_url, and no override.
    env = HOMES["craze"](tmp_path / "m", ms["muse-wonder-1.0"], snap, "http://127.0.0.1:4242/r/t/api.meta.ai/v1")
    assert ANTHROPIC_API_ENV not in env
    prov = tomllib.loads((tmp_path / "m/.craze/native/providers.toml").read_text())["providers"]
    assert prov["meta"]["base_url"] == "http://127.0.0.1:4242/r/t/api.meta.ai/v1"
    # The standalone proxy's home hands the override back for the caller to export.
    env = craze_home_multi(tmp_path / "multi" / ".craze", [ms["muse-wonder-1.0"], ms["anthropic/claude-opus-5-5"]], snap,
                           {"meta": "http://127.0.0.1:1/r/t/api.meta.ai/v1", "anthropic": base})
    assert env == {ANTHROPIC_API_ENV: base[: -len("/v1")]}


def test_the_override_is_what_craze_accepts_and_keeps_its_path(tmp_path):
    # craze takes an http URL on a loopback IP literal, no user, query or fragment;
    # anything else fails the session (messagesapi.BaseURL), so it is refused here first.
    for bad in ("https://127.0.0.1:1/r/t/api.anthropic.com/v1", "http://localhost:1/r/t/api.anthropic.com/v1",
                "http://10.0.0.1:1/r/t/api.anthropic.com/v1", "http://127.0.0.1:1/r/t/api.anthropic.com",
                "http://127.0.0.1:1/r/t/api.anthropic.com/v1?x=1", "http://u@127.0.0.1:1/r/t/api.anthropic.com/v1"):
        with pytest.raises(ValueError):
            anthropic_api(bad)
    assert anthropic_api("http://[::1]:7/r/t/api.anthropic.com/v1") == "http://[::1]:7/r/t/api.anthropic.com"

    # craze's client posts <override>/v1/messages: through the proxy that is the route's
    # /messages, upstream at Anthropic's /v1/messages.
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {HAIKU}, tmp_path / "run" / "capture.jsonl")
            override = anthropic_api(proxy.base_url(route, "anthropic"))
            st, _ = await post(s, override + "/v1/messages", msg(HAIKU))
            assert st == 200 and up.seen[-1]["path"] == "/v1/messages"
            await proxy.end_run(route)

    run(go())


def test_the_opencode_and_gx_homes_for_anthropic(tmp_path):
    snap = fake_snapshot(tmp_path)
    em = load_models()["anthropic/claude-sonnet-5-5"]
    base = "http://127.0.0.1:4242/r/t/api.anthropic.com/v1"
    HOMES["opencode"](tmp_path / "oc", em, snap, base)
    o = json.loads((tmp_path / "oc/opencode.json").read_text())
    assert o["model"] == o["small_model"] == "anthropic/claude-sonnet-5-5" and o["enabled_providers"] == ["anthropic"]
    assert o["provider"] == {"anthropic": {"options": {"baseURL": base, "apiKey": paths.DUMMY_KEY}}}
    HOMES["gx"](tmp_path / "gx", em, snap, base)
    g = tomllib.loads((tmp_path / "gx/.grok/config.toml").read_text())
    assert g["model_providers"]["anthropic"] == {"base_url": base, "api_key": paths.DUMMY_KEY, "api_backend": "messages",
                                                 "extra_headers": {"anthropic-version": "2023-06-01"}}
    assert g["model"]["anthropic/claude-sonnet-5-5"]["reasoning_effort"] == "medium"
    with pytest.raises(ValueError):
        HOMES["codex"](tmp_path / "cx", em, snap, base)  # no Responses API at Anthropic


def test_the_anthropic_provider_is_read_as_craze_reads_it(tmp_path):
    craze, _ = owner_tree(tmp_path)
    table, ring = load_providers(craze / "providers.toml", env={})
    a = table["anthropic"]
    assert (a.driver, a.base_url, a.host, a.prefix, a.messages) == ("anthropic", ANTHROPIC_BASE, "api.anthropic.com",
                                                                     "v1", True)
    assert not table["meta"].messages
    assert ring.key_for("anthropic") == FAKE["anthropic"]  # the entry's key
    # craze's variable comes first; the stored key is still scrubbed.
    _, ring = load_providers(craze / "providers.toml", env={"ANTHROPIC_API_KEY": "sk-ant-env-value-0123456789"})
    assert ring.key_for("anthropic") == "sk-ant-env-value-0123456789" and FAKE["anthropic"] in ring.secrets()
    # An entry's env_keys -- an empty list included -- replaces craze's, and its endpoint
    # and driver are craze's whatever it writes.
    text = (craze / "providers.toml").read_text().replace(
        "[providers.anthropic]\n", '[providers.anthropic]\nenv_keys = []\ndriver = "openai-compat"\n'
        'base_url = "https://gateway.example/v1"\n')
    (craze / "providers.toml").write_text(text)
    table, ring = load_providers(craze / "providers.toml", env={"ANTHROPIC_API_KEY": "sk-ant-env-value-0123456789"})
    assert ring.key_for("anthropic") == FAKE["anthropic"] and table["anthropic"].base_url == ANTHROPIC_BASE
    assert table["anthropic"].messages
    # No entry at all: an exported key still funds it, as it funds craze.
    (tmp_path / "p.toml").write_text("version = 1\n")
    table, ring = load_providers(tmp_path / "p.toml", env={"ANTHROPIC_API_KEY": "sk-ant-env-value-0123456789"})
    assert "anthropic" in table and ring.key_for("anthropic") == "sk-ant-env-value-0123456789"


def test_the_snapshot_needs_no_entry_for_the_locked_provider(tmp_path):
    from crazeeval.snapshot import build_snapshot

    craze, grok = owner_tree(tmp_path)
    text = (craze / "providers.toml").read_text()
    (craze / "providers.toml").write_text(text[: text.index("[providers.anthropic]")])  # an env-only key
    snap = build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)
    assert snap["models"]["anthropic/claude-opus-5-5"]["craze_provider"] == {}
    assert snap["models"]["anthropic/claude-opus-5-5"]["gx_provider"]["api_backend"] == "messages"
    assert FAKE["anthropic"] not in json.dumps(snap)
    with pytest.raises(KeyError):  # an unlocked provider still needs its entry
        (craze / "providers.toml").write_text(text.replace("[providers.meta]", "[providers.notmeta]"))
        build_snapshot(load_models(), craze_dir=craze, grok_dir=grok)


def test_a_price_without_a_cache_write_rate_defaults_to_zero(tmp_path):
    (tmp_path / "p.toml").write_text('[models.m]\ninput = 1.0\ncached_input = 0.1\noutput = 2.0\n')
    p = load_prices(tmp_path / "p.toml")["m"]
    assert (p.cache_write, p.list_cache_write, p.long_context) == (0.0, 0.0, None)
    assert Price("x", 1, 0.1, 2, 1, 0.1, 2).cache_write == 0.0


# -- review c4c5 (findings 1-6) ----------------------------------------------------------

SYSTEM_BLOCKS = [{"type": "text", "text": "You are craze, a coding agent."},
                 {"type": "text", "text": "Workspace: /sandbox/work/smoke-py", "cache_control": {"type": "ephemeral"}}]


def _messages_batch(root: Path, efforts: dict) -> Path:
    """A batch of one smoke-fix run per harness on Opus 5.5, each a Messages capture at
    its effort, every one with adaptive thinking and the same top-level system prompt."""
    for harness, effort in efforts.items():
        rep = root / "runs" / harness / "anthropic_claude-opus-5-5" / "smoke-fix" / "rep1"
        adir = rep / "attempt-1"
        adir.mkdir(parents=True)
        req = {"model": OPUS, "max_tokens": 32000, "stream": True, "system": SYSTEM_BLOCKS,
               "thinking": {"type": "adaptive"}, "output_config": {"effort": effort},
               "tools": [{"name": "bash", "input_schema": {"type": "object"}}],
               "messages": [{"role": "user", "content": "Fix the failing test."}]}
        rec = {"seq": 1, "path": "/messages", "status": 200, "request": req,
               "response": {"events": messages_events(OPUS)}, "usage": EXPECT_USAGE, "served_model": OPUS}
        (adir / "capture.jsonl").write_text(json.dumps(rec) + "\n")
        res = {"harness": harness, "status": "ok", "contamination": [], "metrics": metrics(read_capture(adir)),
               "attempts": [{"attempt": 1, "dir": "attempt-1"}]}
        (rep / "result.json").write_text(json.dumps(res))
    return root


def test_effort_parity_reads_the_messages_effort(tmp_path):
    from crazeeval import captures

    # Both harnesses send thinking {"type": "adaptive"}; only the effort differs.
    out = captures.write(_messages_batch(tmp_path / "mismatch", {"craze": "medium", "opencode": "high"}))
    f = json.loads((out / "captures.json").read_text())["models"]["anthropic/claude-opus-5-5"]["fidelity"]
    assert f["effort_by_harness"] == {"craze": ["medium"], "opencode": ["high"]}
    assert f["effort_parity"] is False and f["pinned_effort"] == "medium"
    assert "effort parity: **no**" in (out / "captures.md").read_text()
    out = captures.write(_messages_batch(tmp_path / "match", {"craze": "medium", "opencode": "medium"}))
    f = json.loads((out / "captures.json").read_text())["models"]["anthropic/claude-opus-5-5"]["fidelity"]
    assert f["effort_parity"] is True


def test_the_messages_system_prompt_is_the_system_prompt(tmp_path):
    from crazeeval import captures

    text = "You are craze, a coding agent.\nWorkspace: /sandbox/work/smoke-py"
    for system in (SYSTEM_BLOCKS, text):
        req = {"model": OPUS, "system": system, "max_tokens": 10, "messages": [{"role": "user", "content": "hi"}]}
        assert captures.system_text(req) == text
        assert captures.params(req) == {"model": OPUS, "max_tokens": 10}  # the system prompt is not a parameter
    out = captures.write(_messages_batch(tmp_path, {"craze": "medium"}))
    row = json.loads((out / "captures.json").read_text())["models"]["anthropic/claude-opus-5-5"]["harnesses"][0]
    sp = row["system_prompt"]
    assert sp["bytes"] == len(text.encode()) and Path(sp["file"]).read_text() == text
    assert row["environment"]["workspace"] == "system" and "system" not in row["params"]


def test_content_the_body_cannot_bound_is_refused(tmp_path):
    png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

    def with_block(block):
        return msg(HAIKU, messages=[{"role": "user", "content": [block, {"type": "text", "text": "Summarize."}]}])

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "opencode", {HAIKU}, tmp_path / "run" / "capture.jsonl")
            url = proxy.base_url(route, "anthropic") + "/messages"
            doc = {"type": "document", "source": {"type": "url", "url": "https://example.com/big.pdf"}}
            st, body = await post(s, url, with_block(doc))
            assert st == 403 and b"unbounded-input" in body and b"document block by url" in body
            img = {"type": "image", "source": {"type": "url", "url": "https://example.com/cat.png"}}
            st, body = await post(s, url, with_block(img))
            assert st == 403 and b"unbounded-input" in body and b"image block by url" in body
            assert up.seen == [] and led.totals()["requests"] == 0  # nothing reserved, nothing sent
            inline = {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": png}}
            st, _ = await post(s, url, with_block(inline))
            assert st == 200 and len(up.seen) == 1 and png in up.seen[0]["body"].decode()
            summary = await proxy.end_run(route)
            assert summary["refused"] == {"unbounded-input": 2} and "unbounded-input" in summary["flags"]
            assert summary["forwarded"] == 1

    run(go())


def test_every_api_shape_that_escapes_the_byte_bound():
    from crazeeval.pricing import unbounded_input

    data_url = "data:image/png;base64,iVBORw0KGgo="
    user = lambda *content: {"messages": [{"role": "user", "content": list(content)}]}  # noqa: E731
    refused = {
        "messages url image": user({"type": "image", "source": {"type": "url", "url": "https://x/a.png"}}),
        "messages files api": user({"type": "document", "source": {"type": "file", "file_id": "file_1"}}),
        "messages base64 pdf": user({"type": "document", "source": {"type": "base64", "media_type": "application/pdf",
                                                                     "data": "JVBERi0="}}),
        "in a tool_result": user({"type": "tool_result", "tool_use_id": "t", "content": [
            {"type": "image", "source": {"type": "url", "url": "https://x/a.png"}}]}),
        "container upload": user({"type": "container_upload", "file_id": "file_2"}),
        "server web fetch": {"tools": [{"type": "web_fetch_20250910", "name": "web_fetch"}], **user()},
        "remote mcp": {"mcp_servers": [{"type": "url", "url": "https://mcp.example"}], **user()},
        "chat http image_url": user({"type": "image_url", "image_url": {"url": "https://x/a.png"}}),
        "chat file": user({"type": "file", "file": {"file_data": "data:application/pdf;base64,JVBERi0="}}),
        "chat search": {"web_search_options": {}, **user()},
        "responses http input_image": {"input": [{"type": "message", "role": "user", "content": [
            {"type": "input_image", "image_url": "https://x/a.png"}]}]},
        "responses file_id image": {"input": [{"role": "user", "content": [{"type": "input_image", "file_id": "f"}]}]},
        "responses input_file": {"input": [{"role": "user", "content": [{"type": "input_file", "file_url": "https://x"}]}]},
        "responses hosted search": {"tools": [{"type": "web_search"}], "input": "hi"},
    }
    for name, body in refused.items():
        assert unbounded_input(body) is not None, name
    admitted = {
        "plain chat": user({"type": "text", "text": "https://x/a.png is a URL in text"}),
        "chat data image": user({"type": "image_url", "image_url": {"url": data_url}}),
        "responses data image": {"input": [{"role": "user", "content": [{"type": "input_image", "image_url": data_url}]}]},
        "messages base64 image": user({"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "x"}}),
        "messages text document": user({"type": "document", "source": {"type": "text", "media_type": "text/plain",
                                                                         "data": "notes"}}),
        "a call's arguments": {"messages": [{"role": "assistant", "content": [
            {"type": "tool_use", "id": "t", "name": "fetch_file", "input": {"file_id": "f", "url": "https://x"}}]}]},
        "client tools": {"tools": [{"name": "bash", "input_schema": {}}, {"type": "function", "function": {"name": "f"}}],
                         **user()},
    }
    for name, body in admitted.items():
        assert unbounded_input(body) is None, name


def test_a_key_in_the_path_is_an_exposure_and_never_echoed(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("r1", "craze", {OPUS, "muse-spark-1.3"}, tmp_path / "run" / "capture.jsonl")
            st, body = await post(s, proxy.base_url(route, "anthropic") + "/messages/" + ANT_KEY, msg(OPUS))
            assert st == 403 and b"key-exposure" in body and ANT_KEY.encode() not in body
            # An endpoint no route forwards, with a key in its path: the exposure, not the path, is refused.
            st, body = await post(s, proxy.base_url(route, "meta") + "/embeddings/" + META_KEY, msg("muse-spark-1.3"))
            assert st == 403 and b"key-exposure" in body and META_KEY.encode() not in body
            assert up.seen == []
            summary = await proxy.end_run(route)
            assert summary["key_exposure_refusals"] == 2 and summary["refused"] == {"key-exposure": 2}
            # Every message the harness sees is scrubbed where it is built, whatever it holds.
            resp = proxy._refuse(route, {}, 403, "x", f"echoing {ANT_KEY} and {META_KEY}")
            assert ANT_KEY.encode() not in resp.body and META_KEY.encode() not in resp.body
            assert b"<scrubbed-key>" in resp.body

    run(go())


def test_the_probe_generates_an_anthropic_home(tmp_path):
    from crazeeval.probe import probe_env

    snap = fake_snapshot(tmp_path)
    env = probe_env(tmp_path / "home", load_models()["anthropic/claude-haiku-5-5"], snap)
    assert env[ANTHROPIC_API_ENV] == "http://127.0.0.1:1/r/probe/anthropic"
    assert {"CRAZE_HOME", "GROK_HOME", "OPENCODE_CONFIG"} <= set(env) and paths.DUMMY_KEY_VAR not in env  # no codex
    g = tomllib.loads((tmp_path / "home/.grok/config.toml").read_text())
    assert g["model_providers"]["anthropic"]["base_url"] == "http://127.0.0.1:1/r/probe/anthropic/v1"
    # Every other model's home still generates on the same placeholder.
    for em in load_models().values():
        probe_env(tmp_path / "all" / em.slug, em, snap)


def test_a_negative_or_nan_rate_is_refused(tmp_path):
    base = '[models.m]\ninput = 1.0\ncached_input = 0.1\noutput = 2.0\nlong_context_above = 10\n'
    tier = '[models.m.long_context]\ninput = 2.0\ncached_input = 0.2\ncache_write = {w}\noutput = {o}\n'
    cases = {
        "negative tier rate": base + tier.format(w="-0.625", o="4.0"),
        "nan tier rate": base + tier.format(w="2.5", o="nan"),
        "inf base rate": base.replace("output = 2.0", "output = inf") + tier.format(w="2.5", o="4.0"),
        "negative list rate": base + "list_cache_write = -1.0\n" + tier.format(w="2.5", o="4.0"),
        "a string": base.replace("input = 1.0", 'input = "1.0"') + tier.format(w="2.5", o="4.0"),
    }
    for name, text in cases.items():
        (tmp_path / "p.toml").write_text(text)
        with pytest.raises(ValueError, match="must be a finite number, 0 or more"):  # each case, by name
            load_prices(tmp_path / "p.toml")
    (tmp_path / "p.toml").write_text(base + tier.format(w="0", o="4"))  # zero and integers are rates
    assert load_prices(tmp_path / "p.toml")["m"].long_context.output == 4.0
