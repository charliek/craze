"""Probe Exa's hosted MCP endpoint the way craze's websearch caller does.

One JSON-RPC tools/call (or tools/list) per POST, no initialize, with the key
in X-Api-Key when --keyed (internal/harness/websearch/mcp.go). The key is read
from craze's credentials.toml ($CRAZE_HOME or ~/.craze, native/) and is never
printed: every printed text has it replaced first. Python 3.11+, stdlib only.

    python3 exa_probe.py [--keyed] list [--tools a,b,c]
    python3 exa_probe.py [--keyed] burst [-n 8] [--retries 0] [--query Q]
    python3 exa_probe.py [--keyed] call TOOL 'JSON-ARGS' [--tools a,b,c]
    python3 exa_probe.py --keyed compare

Experiments E2-E8 in ../02-experiments.md were run with these commands.
"""

import argparse
import concurrent.futures as cf
import json
import os
import pathlib
import random
import re
import time
import tomllib
import urllib.error
import urllib.parse
import urllib.request

ENDPOINT = "https://mcp.exa.ai/mcp"
ADVANCED = "web_search_exa,web_fetch_exa,web_search_advanced_exa"


def load_key():
    home = pathlib.Path(os.environ.get("CRAZE_HOME", pathlib.Path.home() / ".craze"))
    try:
        with open(home / "native" / "credentials.toml", "rb") as f:
            return tomllib.load(f)["exa"]["api_key"].strip()
    except (OSError, KeyError, tomllib.TOMLDecodeError):
        return ""


KEY = load_key()


def scrub(text):
    return text.replace(KEY, "[key]") if KEY else text


def post(body, keyed, endpoint=ENDPOINT, timeout=25):
    if keyed and not KEY:
        raise SystemExit("--keyed: no [exa] api_key in credentials.toml (craze auth login websearch/exa)")
    req = urllib.request.Request(endpoint, data=json.dumps(body).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    req.add_header("User-Agent", "craze-probe")
    if keyed:
        req.add_header("X-Api-Key", KEY)
    start = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw, status, headers = resp.read(), resp.status, dict(resp.headers)
    except urllib.error.HTTPError as e:
        raw, status, headers = e.read(), e.code, dict(e.headers)
    return status, headers, scrub(raw.decode("utf-8", "replace")), time.monotonic() - start


def parse(raw):
    """The JSON-RPC answer in a JSON or SSE reply, or None."""
    raw = raw.strip()
    if raw.startswith("{"):
        return json.loads(raw)
    for line in raw.splitlines():
        if not line.startswith("data:"):
            continue
        try:
            msg = json.loads(line[5:].lstrip())
        except ValueError:
            continue
        if "result" in msg or "error" in msg:
            return msg
    return None


def call(tool, args, keyed, endpoint=ENDPOINT, rid=1):
    body = {"jsonrpc": "2.0", "id": rid, "method": "tools/call", "params": {"name": tool, "arguments": args}}
    return post(body, keyed, endpoint)


def text_of(msg):
    """(isError, text) of a JSON-RPC answer."""
    if not msg:
        return None, None
    if "error" in msg:
        return True, json.dumps(msg["error"])
    res = msg["result"]
    text = "".join(b.get("text", "") for b in res.get("content", []) if b.get("type") == "text")
    return bool(res.get("isError")), text


def rate_headers(h):
    return {k: v for k, v in h.items() if "retry" in k.lower() or "ratelimit" in k.lower()}


def rate_limited(status, raw):
    if status == 429:
        return True
    if status != 200:
        return False
    is_err, text = text_of(parse(raw))
    return bool(is_err) and re.search(r"\berror \(429\)", text or "") is not None


def cmd_list(a):
    endpoint = ENDPOINT + (f"?tools={a.tools}" if a.tools else "")
    status, _, raw, _ = post({"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}}, a.keyed, endpoint)
    msg = parse(raw)
    print("HTTP", status)
    if not msg or "result" not in msg:
        print(raw[:2000])
        return
    for t in msg["result"]["tools"]:
        schema = t.get("inputSchema", {})
        print(f"\n## {t['name']}  required={schema.get('required')}")
        for name, p in schema.get("properties", {}).items():
            desc = (p.get("description") or "").replace("\n", " ")
            enum = f" enum={p['enum']}" if "enum" in p else ""
            print(f"  - {name}: {p.get('type', '')}{enum} -- {desc[:160]}")


def cmd_burst(a):
    """n parallel web_search_exa calls, optionally retrying rate-limited ones."""

    def one(i):
        q = f"{a.query} {i}"
        tries, waited = 0, 0.0
        while True:
            status, h, raw, el = call("web_search_exa", {"query": q, "objective": q, "numResults": 8}, a.keyed, rid=i + 1)
            tries += 1
            if not rate_limited(status, raw) or tries > a.retries:
                break
            wait = float(h.get("retry-after") or h.get("Retry-After") or 1) * (1 + random.random()) * tries
            time.sleep(wait)
            waited += wait
        is_err, text = text_of(parse(raw)) if status == 200 else (None, raw)
        ok = status == 200 and not is_err
        return i, ok, status, is_err, tries, el + waited, len(raw), rate_headers(h), (text or "")[:160].replace("\n", " ")

    with cf.ThreadPoolExecutor(max_workers=a.n) as ex:
        rows = list(ex.map(one, range(a.n)))
    print(f"keyed={a.keyed} n={a.n} retries={a.retries}: ok={sum(r[1] for r in rows)} failed={sum(not r[1] for r in rows)}")
    for i, ok, status, is_err, tries, total, size, hdrs, head in rows:
        print(f"  [{i}] {'OK ' if ok else 'ERR'} HTTP {status} isError={is_err} tries={tries} {total:.1f}s {size}B {hdrs} {'' if ok else head}")


def cmd_call(a):
    endpoint = ENDPOINT + (f"?tools={a.tools}" if a.tools else "")
    status, h, raw, el = call(a.tool, json.loads(a.args), a.keyed, endpoint)
    is_err, text = text_of(parse(raw)) if status == 200 else (None, None)
    print(f"HTTP {status} {el:.1f}s raw={len(raw)}B isError={is_err} {rate_headers(h)}")
    print((raw if text is None else text)[: a.head])


def cmd_compare(a):
    """Bytes and hosts for one query: craze's call against the alternatives (E8)."""
    q = "Obsidian Tasks plugin due date emoji format"
    variants = [
        ("craze today: web_search_exa n=8, objective=query", "web_search_exa",
         {"query": q, "objective": q, "numResults": 8}, ""),
        ("web_search_exa n=8, model-written objective", "web_search_exa",
         {"query": q, "numResults": 8, "objective": "Rank the official Obsidian Tasks documentation "
          "(publish.obsidian.md/tasks) and its GitHub repo first; exclude blogs and SEO roundups."}, ""),
        ("web_search_exa n=4", "web_search_exa", {"query": q, "objective": q, "numResults": 4}, ""),
        ("advanced: includeDomains, highlights 1500, n=5", "web_search_advanced_exa",
         {"query": q, "numResults": 5, "includeDomains": ["publish.obsidian.md", "github.com"],
          "enableHighlights": True, "highlightsMaxCharacters": 1500}, ADVANCED),
    ]
    for label, tool, args, tools in variants:
        endpoint = ENDPOINT + (f"?tools={tools}" if tools else "")
        status, _, raw, el = call(tool, args, a.keyed, endpoint)
        is_err, text = text_of(parse(raw)) if status == 200 else (True, raw)
        text = text or ""
        try:
            urls = [r["url"] for r in json.loads(text)["results"]]
        except (ValueError, KeyError, TypeError):
            urls = re.findall(r"^URL: (\S+)", text, re.M)
        hosts = ", ".join(urllib.parse.urlparse(u).netloc for u in urls)
        print(f"\n## {label}\nHTTP {status} isError={is_err} {el:.1f}s text={len(text)}B (~{len(text) // 4} tokens) results={len(urls)}")
        print("  hosts:", hosts or text[:300])
        time.sleep(1.2)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--keyed", action="store_true", help="send the stored key")
    sub = p.add_subparsers(required=True)
    s = sub.add_parser("list")
    s.add_argument("--tools", default="")
    s.set_defaults(fn=cmd_list)
    s = sub.add_parser("burst")
    s.add_argument("-n", type=int, default=8)
    s.add_argument("--retries", type=int, default=0)
    s.add_argument("--query", default="markdown task syntax due date")
    s.set_defaults(fn=cmd_burst)
    s = sub.add_parser("call")
    s.add_argument("tool")
    s.add_argument("args")
    s.add_argument("--tools", default="")
    s.add_argument("--head", type=int, default=1500)
    s.set_defaults(fn=cmd_call)
    s = sub.add_parser("compare")
    s.set_defaults(fn=cmd_compare)
    a = p.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()
