"""The proxy's web route and the frozen corpus behind it (craze plan 051 E3, C9)."""

from __future__ import annotations

import asyncio
import json
import math
import os
from pathlib import Path

import pytest

from crazeeval import paths, webcorpus
from crazeeval.capture import metrics, read_capture
from crazeeval.webcorpus import Corpus, CorpusError, Page, load_corpus
from fakes import ANT_KEY, ZAI_KEY, rig

MINI = Path(__file__).parent / "web-corpus"
GOLDEN = paths.REPO_ROOT / "internal" / "harness" / "websearch" / "testdata"
DUMMY = {"Authorization": "Bearer craze-eval-dummy-key", "Content-Type": "application/json"}
# What craze's MCP caller sends (internal/harness/websearch: mcp.go, exa.go).
MCP_HEADERS = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
               "User-Agent": "craze"}


def run(coro):
    return asyncio.run(coro)


def search_call(query: str, rid: int = 1) -> dict:
    return {"jsonrpc": "2.0", "id": rid, "method": "tools/call",
            "params": {"name": "web_search_exa", "arguments": {"query": query, "objective": query, "numResults": 8}}}


def fetch_call(url: str, rid: int = 1) -> dict:
    return {"jsonrpc": "2.0", "id": rid, "method": "tools/call",
            "params": {"name": "web_fetch_exa", "arguments": {"urls": [url], "maxCharacters": 50000}}}


async def mcp(session, url: str, body, method: str = "POST", headers: dict | None = None):
    data = body if isinstance(body, bytes) else json.dumps(body).encode()
    async with session.request(method, url, data=data, headers=headers or MCP_HEADERS) as r:
        return r.status, r.headers.get("Content-Type", ""), await r.read()


def event_data(body: bytes) -> dict:
    """The one SSE message event's JSON."""
    assert body.startswith(b"event: message\ndata: ") and body.endswith(b"\n\n"), body[:80]
    return json.loads(body[len(b"event: message\ndata: "):-2])


def text_of(body: bytes) -> str:
    return event_data(body)["result"]["content"][0]["text"]


def write_corpus(d: Path, pages: dict[str, tuple[str, str, str]]) -> Path:
    d.mkdir(parents=True, exist_ok=True)
    for name, (url, title, body) in pages.items():
        (d / name).write_text(f"URL: {url}\nTitle: {title}\n\n{body}\n")
    return d


# -- the corpus ---------------------------------------------------------------------------


def test_corpus_pages_and_their_errors(tmp_path):
    c = load_corpus(MINI)
    assert [p.file for p in c.pages] == ["a-tides.md", "b-tides-mirror.md", "haefen.md", "harbor-guide.md"]
    assert c.pages[2].title == "Häfen – Überblick" and c.pages[0].body.startswith("Tide tables for the inner")
    bad = {
        "no-headers": "Some text\n",
        "title-first": "Title: T\nURL: https://x.example/a\n\nbody\n",
        "fragment": "URL: https://x.example/a#top\nTitle: T\n\nbody\n",
        "trailing-slash": "URL: https://x.example/a/\nTitle: T\n\nbody\n",
        "not-http": "URL: ftp://x.example/a\nTitle: T\n\nbody\n",
        "empty-body": "URL: https://x.example/a\nTitle: T\n\n\n",
        "title-in-body": "URL: https://x.example/a\nTitle: T\n\nfirst\nTitle: another\n",
    }
    for name, text in bad.items():
        d = tmp_path / name
        d.mkdir()
        (d / "p.md").write_text(text)
        with pytest.raises(CorpusError):
            load_corpus(d)
    dup = write_corpus(tmp_path / "dup", {"a.md": ("https://x.example/a", "A", "a"), "b.md": ("https://x.example/a", "B", "b")})
    with pytest.raises(CorpusError, match="also a.md"):
        load_corpus(dup)
    other = write_corpus(tmp_path / "other", {"a.md": ("https://x.example/a", "A", "a")})
    (other / "notes.txt").write_text("x")
    with pytest.raises(CorpusError, match="only regular"):
        load_corpus(other)
    (tmp_path / "empty").mkdir()
    with pytest.raises(CorpusError, match="no pages"):
        load_corpus(tmp_path / "empty")


def test_ranking_is_bm25_and_deterministic(tmp_path):
    pages = {f"p{i:02d}.md": (f"https://same.example/{chr(ord('z') - i)}", "Copy", "anchor chain") for i in range(10)}
    pages["rare.md"] = ("https://rare.example/x", "Rare", "anchor chain windlass")
    pages["long.md"] = ("https://long.example/x", "Long", "windlass " + "filler " * 200)
    c = load_corpus(write_corpus(tmp_path / "c", pages))
    first = c.search("windlass")
    # The rare term in the short page beats the same term in a long one (length
    # normalisation); pages without it are no results.
    assert [p.url for p in first] == ["https://rare.example/x", "https://long.example/x"]
    # Ten copies that score the same (and above the longer rare page): the top eight are
    # the first eight of their URLs, ascending, every time.
    ties = c.search("chain")
    copies = sorted(u for u, _, _ in pages.values() if u.startswith("https://same.example/"))
    assert [p.url for p in ties] == copies[:webcorpus.RESULTS]
    ranked = c.ranked("chain")
    assert len(ranked) == 11 and len({s for s, p in ranked if p.url in copies}) == 1
    assert ranked[-1][1].url == "https://rare.example/x"
    assert all(c.search("chain") == ties for _ in range(5))
    # Tokens are lowercased runs of letters and digits: case and punctuation do not
    # matter, a stem does (no stemming), and a query sharing no token has no results.
    assert c.search("WINDLASS!") == first
    assert c.search("windlasses") == [] and c.search("  ...  ") == []
    # No results is a result with no content at all, never words of the route's own.
    assert event_data(webcorpus.answer(c, json.dumps(search_call("windlasses")).encode()).body) == {
        "result": {"content": []}, "jsonrpc": "2.0", "id": 1}
    assert webcorpus.tokens("Über-Höhe 42_b") == ["über", "höhe", "42", "b"]


def test_ties_order_by_url_and_scores_are_bm25():
    page = lambda url, body: Page(url=url, title="t", body=body, file=url[-1] + ".md")  # noqa: E731
    c = Corpus((page("https://b.example/1", "x y"), page("https://a.example/2", "x y"), page("https://c.example/3", "z")))
    assert [p.url for p in c.search("x")] == ["https://a.example/2", "https://b.example/1"]
    # The score, by hand: idf = ln((N - n + .5) / (n + .5) + 1) with N 3 and n 2; tf 1; the
    # page's length (title and body tokens) 3, the average (3 + 3 + 2) / 3.
    idf = math.log((3 - 2 + 0.5) / (2 + 0.5) + 1)
    want = idf * 1 * (1.2 + 1) / (1 + 1.2 * (1 - 0.75 + 0.75 * 3 / (8 / 3)))
    assert [round(s, 12) for s, _ in c.ranked("x")] == [round(want, 12)] * 2
    # Two query tokens add up; a token repeated in the query counts once.
    idf_z = math.log((3 - 1 + 0.5) / (1 + 0.5) + 1)
    want_z = idf_z * 2.2 / (1 + 1.2 * (1 - 0.75 + 0.75 * 2 / (8 / 3)))
    assert [(round(s, 12), p.url) for s, p in c.ranked("z x z")] == [
        (round(want_z, 12), "https://c.example/3"), (round(want, 12), "https://a.example/2"),
        (round(want, 12), "https://b.example/1")]


# -- the route ------------------------------------------------------------------------------


def test_route_refused_for_a_run_with_no_corpus(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("plain", "craze", {"glm-5.3-flash"}, tmp_path / "run" / "capture.jsonl")
            st, ctype, body = await mcp(s, proxy.web_url(route), search_call("anything"))
            assert st == 403 and b"web-not-available" in body and ctype.startswith("application/json")
            summary = await proxy.end_run(route)
            # Nothing a model request touches: no request counted, no capture record, no
            # web counters, no web journal; the refusal log has it.
            assert summary["requests"] == 0 and summary["refused"] == {} and "web" not in summary
            assert read_capture(tmp_path / "run") == [] and not (tmp_path / "run" / "web.jsonl").exists()
            log = [json.loads(x) for x in (tmp_path / "refusals.jsonl").read_text().splitlines()]
            assert log[-1]["reason"] == "web-not-available" and log[-1]["run_id"] == "plain"
            assert up.seen == []

    run(go())


def test_a_token_reaches_only_its_own_corpus(tmp_path):
    other = write_corpus(tmp_path / "other", {"o.md": ("https://other.example/tides", "Other tides", "tide harbor")})

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            ra = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                    corpus=load_corpus(MINI))
            rb = proxy.register_run("b", "craze-web", {"glm-5.3-flash"}, tmp_path / "b" / "capture.jsonl",
                                    corpus=load_corpus(other))
            assert ra.token != rb.token and ra.token not in proxy.web_url(rb)
            _, _, a = await mcp(s, proxy.web_url(ra), search_call("harbor tide"))
            _, _, b = await mcp(s, proxy.web_url(rb), search_call("harbor tide"))
            assert "https://docs.example.org/" in text_of(a) and "other.example" not in text_of(a)
            assert text_of(b).startswith("Title: Other tides\nURL: https://other.example/tides\n")
            assert "docs.example.org" not in text_of(b)
            _, _, miss = await mcp(s, proxy.web_url(rb), fetch_call("https://docs.example.org/tides"))
            assert text_of(miss) == ""
            sa, sb = await proxy.end_run(ra), await proxy.end_run(rb)
            assert sa["web"]["searches"] == 1 and sa["web"]["fetches"] == 0
            assert sb["web"] == {"searches": 1, "fetches": 1, "misses": 1, "bytes_served": sb["web"]["bytes_served"],
                                 "malformed": 0, "refused": 0}
            # The token stops working with its run.
            st, _, body = await mcp(s, proxy.web_url(ra), search_call("harbor"))
            assert st == 403 and b"expired-token" in body

    run(go())


def test_corpus_is_bound_once(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            with pytest.raises(AttributeError, match="bound once"):
                route.corpus = None
            with pytest.raises(TypeError):
                proxy.register_run("b", "craze-web", {"glm-5.3-flash"}, tmp_path / "b" / "capture.jsonl",
                                   corpus=str(MINI))
            with pytest.raises(Exception):
                route.corpus.pages[0].url = "https://elsewhere.example"  # frozen pages
            await proxy.end_run(route)

    run(go())


def test_fetch_hit_and_miss(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            url = proxy.web_url(route)
            hits = ["https://docs.example.org/tides", "https://docs.example.org/tides/", "https://docs.example.org/tides#x",
                    "https://docs.example.org/tides/#x"]
            for u in hits:
                st, ctype, body = await mcp(s, url, fetch_call(u))
                assert st == 200 and ctype.startswith("text/event-stream")
                assert text_of(body).startswith("# Tide tables\nURL: https://docs.example.org/tides\n\nTide tables for")
            misses = ["https://docs.example.org/tides?x=1", "http://docs.example.org/tides", "https://docs.example.org/Tides",
                      "https://docs.example.org"]
            for u in misses:
                _, _, body = await mcp(s, url, fetch_call(u))
                assert event_data(body)["result"]["content"] == [{"type": "text", "text": "", "_meta": {"searchTime": 0}}]
            # maxCharacters cuts the page, as Exa's does.
            req = fetch_call("https://docs.example.org/harbor-guide")
            req["params"]["arguments"]["maxCharacters"] = 20
            _, _, body = await mcp(s, url, req)
            assert text_of(body) == "# Harbor guide\nURL: https://docs.example.org/harbor-guide\n\n# Moorings\n\nThe harb"
            summary = await proxy.end_run(route)
            assert summary["web"]["fetches"] == 9 and summary["web"]["misses"] == 4
            lines = [json.loads(x) for x in (tmp_path / "a" / "web.jsonl").read_text().splitlines()]
            calls = [x for x in lines if x["kind"] == "call"]
            assert [x["hit"] for x in calls] == [True] * 4 + [False] * 4 + [True]
            assert calls[0]["page"] == "https://docs.example.org/tides" and calls[0]["url"] == hits[0]
            assert lines[-1] == {"kind": "summary", "run_id": "a", **summary["web"]}

    run(go())


def test_malformed_requests(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            url = proxy.web_url(route)
            # Not a JSON-RPC request: HTTP 400 with a JSON-RPC error.
            for body, code in [(b"{not json", -32700), (b"[1, 2]", -32600), (b'{"jsonrpc":"2.0","method":"tools/call"}', -32600),
                               (b'{"jsonrpc":"1.0","id":1,"method":"tools/call"}', -32600),
                               (b'{"jsonrpc":"2.0","id":true,"method":"tools/call"}', -32600)]:
                st, ctype, out = await mcp(s, url, body)
                assert st == 400 and ctype.startswith("application/json")
                assert json.loads(out)["error"]["code"] == code
            # A request, but not one the route answers: a JSON-RPC error on the stream.
            for req, code in [({"jsonrpc": "2.0", "id": 7, "method": "initialize", "params": {}}, -32601),
                              ({"jsonrpc": "2.0", "id": 7, "method": "tools/list"}, -32601),
                              ({"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": {"name": "crawling_exa"}}, -32602),
                              ({"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": {"name": "web_search_exa"}}, -32602),
                              (search_call("   ", 7), -32602),
                              ({**fetch_call("x", 7), "params": {"name": "web_fetch_exa", "arguments": {"urls": ["a", "b"]}}},
                               -32602),
                              ({**fetch_call("x", 7), "params": {"name": "web_fetch_exa", "arguments": {"urls": []}}}, -32602)]:
                st, ctype, out = await mcp(s, url, req)
                assert st == 200 and ctype.startswith("text/event-stream")
                ev = event_data(out)
                assert ev["id"] == 7 and ev["error"]["code"] == code and "result" not in ev
            # A string id is echoed as it came.
            _, _, out = await mcp(s, url, {**search_call("harbor"), "id": "abc"})
            assert event_data(out)["id"] == "abc"
            # The route answers POST …/web/mcp only, with a bounded body.
            st, _, _ = await mcp(s, url, search_call("harbor"), method="GET")
            assert st == 405
            st, _, _ = await mcp(s, url.replace("/web/mcp", "/web/other"), search_call("harbor"))
            assert st == 404
            st, _, _ = await mcp(s, url + "?x=1", search_call("harbor"))
            assert st == 404
            st, _, _ = await mcp(s, url, b"{" + b" " * (webcorpus.BODY_LIMIT + 1) + b"}")
            assert st == 413
            # A loaded key in a web request is a key exposure, flagged on the run.
            st, _, _ = await mcp(s, url, search_call(f"harbor {ZAI_KEY}"))
            assert st == 403
            summary = await proxy.end_run(route)
            assert summary["web"]["malformed"] == 12 and summary["web"]["refused"] == 5
            assert summary["web"]["searches"] == 1 and summary["key_exposure_refusals"] == 1
            assert "key-exposure" in summary["flags"] and summary["requests"] == 0
            journal = (tmp_path / "a" / "web.jsonl").read_text()
            assert ZAI_KEY not in journal and read_capture(tmp_path / "a") == []

    run(go())


def test_a_body_over_the_limit_is_refused_while_it_is_read(tmp_path):
    """A chunked body (no Content-Length) is refused once it passes BODY_LIMIT, while
    the client is still sending: never buffered whole first."""

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            gate = asyncio.Event()

            async def body():
                for _ in range(4):  # 256 KiB, then the client waits: the body never ends
                    yield b" " * 65536
                await gate.wait()
                yield b"{}"

            req = asyncio.ensure_future(s.post(proxy.web_url(route), data=body(), headers=MCP_HEADERS))
            try:
                resp = await asyncio.wait_for(asyncio.shield(req), 5)
            finally:
                gate.set()
            assert resp.status == 413 and b"web-body-too-large" in await resp.read()
            resp.release()
            summary = await proxy.end_run(route)
            assert summary["web"]["refused"] == 1 and summary["web"]["malformed"] == 0

    run(go())


def test_crafted_json_is_a_journaled_malformed_request(tmp_path):
    """JSON the parser cannot hold (nested past the recursion limit) and strings no
    reply could encode (a lone surrogate in the id or the query) are malformed
    requests: answered, journaled and counted once, nothing else counted."""
    deep = b"[" * 10000 + b"]" * 10000 + b"\n"
    lone_id = json.dumps(search_call("harbor")).replace('"id": 1', '"id": "\\ud800"').encode()
    lone_query = json.dumps(search_call("harbor")).replace('"harbor"', '"harbor \\ud800"', 1).encode()
    assert len(deep) > 20000 and b"\\ud800" in lone_id and b"\\ud800" in lone_query

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            sizes = []
            for raw in (deep, lone_id, lone_query):
                st, ctype, out = await mcp(s, proxy.web_url(route), raw)
                assert st == 400 and ctype.startswith("application/json"), (st, out[:200])
                assert json.loads(out)["error"]["code"] in (-32700, -32600)
                sizes.append(len(out))
            summary = await proxy.end_run(route)
            assert summary["web"] == {"searches": 0, "fetches": 0, "misses": 0, "bytes_served": sum(sizes),
                                      "malformed": 3, "refused": 0}
            lines = [json.loads(x) for x in (tmp_path / "a" / "web.jsonl").read_text().splitlines()]
            assert [x["op"] for x in lines if x["kind"] == "call"] == ["malformed"] * 3
            assert lines[-1] == {"kind": "summary", "run_id": "a", **summary["web"]}

    run(go())


def test_an_escaped_key_is_a_key_exposure(tmp_path):
    """A loaded key the body carries only JSON-escaped (its first character as \\u0073)
    is refused as a key exposure, like the model route's, wherever it is -- the
    query or the id, which a reply would otherwise echo."""
    escaped = "\\u0073" + ANT_KEY[1:]
    assert ANT_KEY[0] == "s"
    in_query = json.dumps(search_call("harbor KEY")).replace("KEY", escaped).encode()
    in_id = json.dumps(search_call("harbor")).replace('"id": 1', f'"id": "{escaped}"').encode()
    assert ANT_KEY.encode() not in in_query + in_id

    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            for raw in (in_query, in_id):
                st, _, out = await mcp(s, proxy.web_url(route), raw)
                assert st == 403 and b"key-exposure" in out, (st, out[:200])
                assert ANT_KEY.encode() not in out and ANT_KEY[1:].encode() not in out
            summary = await proxy.end_run(route)
            assert summary["key_exposure_refusals"] == 2 and "key-exposure" in summary["flags"]
            assert summary["web"]["searches"] == 0 and summary["web"]["refused"] == 2
            assert ANT_KEY not in (tmp_path / "a" / "web.jsonl").read_text()

    run(go())


def test_web_calls_stay_out_of_model_accounting(tmp_path):
    async def go():
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("a", "craze-web", {"glm-5.3-flash"}, tmp_path / "a" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            base = proxy.base_url(route, "zai-coding-plan")
            body = {"model": "glm-5.3-flash", "messages": [{"role": "user", "content": "hi"}], "stream": True}
            async with s.post(base + "/chat/completions", data=json.dumps(body), headers=DUMMY) as r:
                assert r.status == 200
                await r.read()
            for q in ("harbor tide", "moorings", "zzz-no-match"):
                await mcp(s, proxy.web_url(route), search_call(q))
            await mcp(s, proxy.web_url(route), fetch_call("https://docs.example.org/de/haefen"))
            summary = await proxy.end_run(route)
            # The model exchange is the run's one request, record and ledger entry.
            assert summary["requests"] == 1 and summary["forwarded"] == 1 and summary["refused"] == {}
            recs = read_capture(tmp_path / "a")
            assert len(recs) == 1 and recs[0]["path"] == "/chat/completions"
            m = metrics(recs)
            assert m["requests"] == 1
            assert "web/mcp" not in json.dumps(recs) and "harbor tide" not in json.dumps(recs)
            ledger = [json.loads(x) for x in (tmp_path / "ledger.jsonl").read_text().splitlines()]
            assert {x.get("run_id") for x in ledger if x.get("run_id")} == {"a"}
            assert sum(1 for x in ledger if x.get("kind") == "reserve") == 1
            assert len(up.seen) == 1
            # The web calls are in their own journal and the summary's web counters.
            w = summary["web"]
            assert (w["searches"], w["fetches"], w["misses"], w["malformed"]) == (3, 1, 0, 0)
            lines = [json.loads(x) for x in (tmp_path / "a" / "web.jsonl").read_text().splitlines()]
            calls = [x for x in lines if x["kind"] == "call"]
            assert [x["op"] for x in calls] == ["search", "search", "search", "fetch"]
            assert calls[2]["results"] == [] and w["bytes_served"] == sum(x["bytes"] for x in calls)
            assert lines[-1]["kind"] == "summary"

    run(go())


# -- the golden replies -----------------------------------------------------------------------

# Each golden reply: the request craze sends, and the file the route's answer must equal
# byte for byte. internal/harness/websearch/exa_test.go (TestEvalCorpusReplies) parses
# the same files with craze's own reader. CRAZEEVAL_UPDATE_GOLDEN=1 rewrites them.
GOLDENS = {
    "eval-corpus-search.body": search_call("harbor tide schedule", 1),
    "eval-corpus-fetch.body": fetch_call("https://docs.example.org/de/haefen/#gezeiten", 2),
    "eval-corpus-fetch-miss.body": fetch_call("https://docs.example.org/nowhere", 3),
    "eval-corpus-search-empty.body": search_call("zzz no such words", 4),
}


def test_golden_replies_are_what_the_route_sends(tmp_path):
    async def go():
        out = {}
        async with rig(tmp_path) as (proxy, up, led, s):
            route = proxy.register_run("g", "craze-web", {"glm-5.3-flash"}, tmp_path / "g" / "capture.jsonl",
                                       corpus=load_corpus(MINI))
            for name, req in GOLDENS.items():
                st, ctype, body = await mcp(s, proxy.web_url(route), req)
                assert st == 200 and ctype.startswith("text/event-stream")
                out[name] = body
            await proxy.end_run(route)
        return out

    got = run(go())
    if os.environ.get("CRAZEEVAL_UPDATE_GOLDEN") == "1":
        for name, body in got.items():
            (GOLDEN / name).write_bytes(body)
    for name, body in got.items():
        assert (GOLDEN / name).read_bytes() == body, f"{name}: the route's reply changed (CRAZEEVAL_UPDATE_GOLDEN=1)"
    # What the Go test then reads: the search's three results in order, the long page
    # cut at 1,500 characters, the non-ASCII page as itself.
    text = text_of(got["eval-corpus-search.body"])
    assert [line for line in text.split("\n") if line.startswith("URL: ")] == [
        "URL: https://docs.example.org/harbor-guide", "URL: https://docs.example.org/tides",
        "URL: https://mirror.example.org/tides"]
    assert "Häfen – Überblick".encode() in got["eval-corpus-fetch.body"]
    assert text_of(got["eval-corpus-fetch-miss.body"]) == ""
    assert event_data(got["eval-corpus-search-empty.body"])["result"] == {"content": []}


def test_layouts_match_exas_captured_replies():
    """The route's envelope is the captured one's, key for key and byte for byte."""
    captured = (GOLDEN / "search.body").read_bytes()
    ev = event_data(captured)
    mine = webcorpus.result_event(ev["id"], ev["result"]["content"][0]["text"])
    want = captured.replace(json.dumps(ev["result"]["content"][0]["_meta"], separators=(",", ":")).encode(),
                            b'{"searchTime":0}')
    assert mine == want
    # A search block is laid out as Exa's are: Title, URL, Published, Author, Highlights.
    first = ev["result"]["content"][0]["text"].split("\n")[:5]
    assert [line.split(":")[0] for line in first] == ["Title", "URL", "Published", "Author", "Highlights"]
    page = Page(url="https://go.dev/doc/devel/release", title="Release History - The Go Programming Language",
                body="body", file="x.md")
    fetched = text_of((GOLDEN / "fetch.body").read_bytes())
    assert webcorpus.fetch_text(page).split("\n")[:3] == fetched.split("\n")[:3]


def test_task_corpus_answers_the_task_question():
    """T-W1's corpus, as craze-web's route would search it: the endpoint's page first."""
    from crazeeval.tasks import load_tasks

    t = load_tasks()["T-W1"]
    top = t.web_corpus.search("quayside api berth slots paging")
    assert top[0].file == "v3-berth-slots.md"
    assert t.web_corpus.search("quayside authentication api key header")[0].file == "v3-authentication.md"
