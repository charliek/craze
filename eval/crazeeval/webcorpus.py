"""A frozen web corpus behind the proxy (craze plan 051 E3).

A ``web`` task's pages (``*.md`` files, each starting with a ``URL:`` line and a
``Title:`` line) answer the native harness's web tools for the ``craze-web`` arm: the
proxy's ``/r/<token>/web/mcp`` route takes the two ``tools/call`` shapes craze sends to
Exa's MCP endpoint (internal/harness/websearch/exa.go) and answers each in Exa's
event-stream envelope and text layouts, so craze's own parser reads them. The layouts
are Exa's (exa-mcp-server's web_search_exa and web_fetch_exa, as captured in
internal/harness/websearch/testdata/search.body and fetch.body); answer()'s replies for
a fixed mini-corpus are committed beside them as eval-corpus-*.body, which a Go test
parses and tests/test_web_route.py regenerates byte for byte.

Search is BM25 (k1 1.2, b 0.75) over each page's title and body; tokens are lowercased
runs of letters and digits, with no stemming and no stop words; the top eight by score,
ties by URL ascending; each result's content is the page's first 1,500 characters. A
query that shares no token with any page gets a result with an empty content list,
which craze reads as no results. Fetch is an exact match on the URL with its fragment
and trailing slashes trimmed; anything else gets one empty text block, which craze
reads as an empty page, and is counted as a miss. Neither invents words of Exa's:
craze writes its own "no results" and "no content" lines. Pure Python: nothing is
fetched, nothing leaves the host.
"""

from __future__ import annotations

import json
import math
import re
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path
from types import MappingProxyType
from typing import Any, Mapping

# The route: /r/<token>/<WEB_HOST>/<WEB_PATH>.
WEB_HOST = "web"
WEB_PATH = "mcp"

SEARCH_TOOL = "web_search_exa"
FETCH_TOOL = "web_fetch_exa"
RESULTS = 8
CONTENT_CHARS = 1500
K1 = 1.2
B = 0.75
# Exa's result separator, and its search-result block (Highlights: then the content).
SEPARATOR = "\n\n---\n\n"
# The largest request body the route reads: craze's calls are a few hundred bytes.
BODY_LIMIT = 64 * 1024

TOKEN_RE = re.compile(r"[^\W_]+")


class CorpusError(ValueError):
    pass


def tokens(text: str) -> list[str]:
    """Lowercased runs of letters and digits (Unicode ones included)."""
    return TOKEN_RE.findall(text.lower())


def normal_url(url: str) -> str:
    """A URL as fetch compares it: without its fragment and trailing slashes."""
    return url.split("#", 1)[0].rstrip("/")


@dataclass(frozen=True)
class Page:
    url: str
    title: str
    body: str
    file: str


@dataclass(frozen=True)
class Corpus:
    """Pages in file-name order, and the BM25 index over them (read-only)."""

    pages: tuple[Page, ...]
    _tf: tuple[Mapping[str, int], ...] = field(init=False, repr=False, compare=False)
    _len: tuple[int, ...] = field(init=False, repr=False, compare=False)
    _df: Mapping[str, int] = field(init=False, repr=False, compare=False)
    _avgdl: float = field(init=False, repr=False, compare=False)
    _by_url: Mapping[str, Page] = field(init=False, repr=False, compare=False)

    def __post_init__(self) -> None:
        tfs = [Counter(tokens(p.title + "\n" + p.body)) for p in self.pages]
        df: Counter = Counter()
        for tf in tfs:
            df.update(tf.keys())
        lens = tuple(sum(tf.values()) for tf in tfs)
        set_ = object.__setattr__
        set_(self, "_tf", tuple(MappingProxyType(dict(tf)) for tf in tfs))
        set_(self, "_len", lens)
        set_(self, "_df", MappingProxyType(dict(df)))
        set_(self, "_avgdl", (sum(lens) / len(lens)) if lens else 0.0)
        set_(self, "_by_url", MappingProxyType({p.url: p for p in self.pages}))

    def search(self, query: str) -> list[Page]:
        """The top RESULTS pages for ``query`` by BM25, ties by URL ascending; pages
        that share no token with it are never results."""
        return [p for _, p in self.ranked(query)[:RESULTS]]

    def ranked(self, query: str) -> list[tuple[float, Page]]:
        """Every page sharing a token with ``query``, with its BM25 score, best first and
        ties by URL ascending. Each distinct query token counts once."""
        terms = sorted(set(tokens(query)))
        n = len(self.pages)
        scored = []
        for i, page in enumerate(self.pages):
            tf, score, hit = self._tf[i], 0.0, False
            norm = K1 * (1 - B + B * self._len[i] / self._avgdl) if self._avgdl else K1
            for t in terms:  # sorted: the float sum is the same every time
                f = tf.get(t, 0)
                if not f:
                    continue
                hit = True
                df = self._df[t]
                idf = math.log((n - df + 0.5) / (df + 0.5) + 1)
                score += idf * f * (K1 + 1) / (f + norm)
            if hit:
                scored.append((score, page))
        scored.sort(key=lambda x: (-x[0], x[1].url))
        return scored

    def fetch(self, url: str) -> Page | None:
        return self._by_url.get(normal_url(url))


def parse_page(text: str, file: str) -> Page:
    lines = text.replace("\r\n", "\n").split("\n")
    if len(lines) < 2 or not lines[0].startswith("URL:") or not lines[1].startswith("Title:"):
        raise CorpusError(f"{file}: a page starts with a URL: line, then a Title: line")
    url = lines[0][len("URL:"):].strip()
    title = lines[1][len("Title:"):].strip()
    if not re.match(r"^https?://[^/\s]+", url) or any(c.isspace() for c in url):
        raise CorpusError(f"{file}: URL {url!r} is not an http(s) URL")
    if normal_url(url) != url:
        raise CorpusError(f"{file}: URL {url!r} has a fragment or a trailing slash")
    if not title:
        raise CorpusError(f"{file}: empty title")
    body = "\n".join(lines[2:]).strip()
    if not body:
        raise CorpusError(f"{file}: empty body")
    # Exa's search layout starts a result at a Title: line after a "---" line: a body
    # holding one would split its own result in two.
    if any(line.startswith("Title:") for line in body.split("\n")):
        raise CorpusError(f"{file}: a body line starts with Title:")
    return Page(url=url, title=title, body=body, file=file)


def load_corpus(d: Path) -> Corpus:
    """The pages of the directory ``d``: only regular ``*.md`` files, URLs unique."""
    d = Path(d)
    if not d.is_dir() or d.is_symlink():
        raise CorpusError(f"{d}: not a directory")
    pages, seen = [], {}
    for p in sorted(d.iterdir()):
        if p.is_symlink() or not p.is_file() or p.suffix != ".md":
            raise CorpusError(f"{p.name}: a corpus holds only regular *.md files")
        page = parse_page(p.read_text(encoding="utf-8"), p.name)
        if page.url in seen:
            raise CorpusError(f"{p.name}: URL {page.url} is also {seen[page.url]}'s")
        seen[page.url] = p.name
        pages.append(page)
    if not pages:
        raise CorpusError(f"{d}: no pages")
    return Corpus(tuple(pages))


# -- Exa's layouts -------------------------------------------------------------------


def search_text(pages: list[Page]) -> str:
    """web_search_exa's text for one or more results: one block per result, joined by
    Exa's separator. (No results is no text block at all: answer.)"""
    return SEPARATOR.join(
        f"Title: {p.title}\nURL: {p.url}\nPublished: N/A\nAuthor: N/A\nHighlights:\n{p.body[:CONTENT_CHARS]}"
        for p in pages)


def fetch_text(page: Page, max_chars: int | None = None) -> str:
    """web_fetch_exa's text for one page: a title line, a URL line, a blank line, the page."""
    body = page.body if max_chars is None else page.body[:max_chars]
    return f"# {page.title}\nURL: {page.url}\n\n{body}".strip()


def _json(obj) -> bytes:
    # As Exa's server writes it: compact, non-ASCII as itself.
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False).encode()


def _event(obj) -> bytes:
    return b"event: message\ndata: " + _json(obj) + b"\n\n"


def result_event(rpc_id, text: str | None) -> bytes:
    """A result with one text block holding ``text`` (empty for a fetch miss), or, for
    None, an empty content list (a search with no results)."""
    content = [] if text is None else [{"type": "text", "text": text, "_meta": {"searchTime": 0}}]
    return _event({"result": {"content": content}, "jsonrpc": "2.0", "id": rpc_id})


def error_event(rpc_id, code: int, message: str) -> bytes:
    return _event({"jsonrpc": "2.0", "id": rpc_id, "error": {"code": code, "message": message}})


# -- one call --------------------------------------------------------------------------

SSE = "text/event-stream"
JSON = "application/json"


@dataclass
class Reply:
    """What the route sends, and the journal line it writes."""

    status: int
    content_type: str
    body: bytes
    entry: dict[str, Any]


def _bad_request(code: int, message: str, why: str) -> Reply:
    body = _json({"jsonrpc": "2.0", "id": None, "error": {"code": code, "message": message}})
    return Reply(400, JSON, body, {"op": "malformed", "why": why})


def _valid_id(v) -> bool:
    return (isinstance(v, int) and not isinstance(v, bool)) or isinstance(v, str)


def decode(raw: bytes) -> dict | Reply:
    """A request body as a JSON-RPC 2.0 request with an id, or the HTTP 400 reply for a
    body that is not one: not JSON (invalid UTF-8 included), nested past what the
    parser holds, or holding a string no reply or journal line could encode (a lone
    surrogate, which JSON's \\u escapes allow)."""
    try:
        req = json.loads(raw)
    except (ValueError, RecursionError):
        return _bad_request(-32700, "Parse error", "not JSON")
    if not isinstance(req, dict) or req.get("jsonrpc") != "2.0" or not _valid_id(req.get("id")) \
            or not isinstance(req.get("method"), str):
        return _bad_request(-32600, "Invalid Request", "not a JSON-RPC 2.0 request with an id")
    try:
        json.dumps(req, ensure_ascii=False).encode()
    except (UnicodeEncodeError, RecursionError, ValueError):
        return _bad_request(-32600, "Invalid Request", "a string that is not valid Unicode")
    return req


def answer(corpus: Corpus, raw: bytes) -> Reply:
    """The route's reply to one request body (decode, then respond)."""
    req = decode(raw)
    return req if isinstance(req, Reply) else respond(corpus, req)


def respond(corpus: Corpus, req: dict) -> Reply:
    """The reply to one decoded request. A request for anything but the two tools is a
    JSON-RPC error on the stream, as Exa's server answers one."""
    rid, method = req["id"], req["method"]

    def err(code: int, message: str, why: str) -> Reply:
        return Reply(200, SSE, error_event(rid, code, message), {"op": "malformed", "why": why})

    if method != "tools/call":
        return err(-32601, "Method not found", f"method {method[:80]!r}")
    params = req.get("params")
    name = params.get("name") if isinstance(params, dict) else None
    args = params.get("arguments") if isinstance(params, dict) else None
    if name not in (SEARCH_TOOL, FETCH_TOOL):
        return err(-32602, f"Tool {str(name)[:80]} not found", "unknown tool")
    if not isinstance(args, dict):
        return err(-32602, f"Invalid arguments for tool {name}", "arguments not an object")
    if name == SEARCH_TOOL:
        query = args.get("query")
        if not isinstance(query, str) or not query.strip():
            return err(-32602, f"Invalid arguments for tool {name}: query", "no query")
        pages = corpus.search(query)
        return Reply(200, SSE, result_event(rid, search_text(pages) if pages else None),
                     {"op": "search", "query": query, "results": [p.url for p in pages]})
    urls = args.get("urls")
    if isinstance(urls, str):
        urls = [urls]
    if not (isinstance(urls, list) and len(urls) == 1 and isinstance(urls[0], str) and urls[0]):
        return err(-32602, f"Invalid arguments for tool {name}: urls", "not one URL")
    url = urls[0]
    limit = args.get("maxCharacters")
    limit = limit if isinstance(limit, int) and not isinstance(limit, bool) and limit > 0 else None
    page = corpus.fetch(url)
    text = fetch_text(page, limit) if page is not None else ""
    return Reply(200, SSE, result_event(rid, text),
                 {"op": "fetch", "url": url, "hit": page is not None, **({"page": page.url} if page else {})})
