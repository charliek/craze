"""Prices, usage normalisation and cost math (plan 029 §2.7, §3.1.3, §3.1.7)."""

from __future__ import annotations

import math
import tomllib
from dataclasses import asdict, dataclass, replace
from pathlib import Path
from typing import Any, Mapping

from crazeeval import paths

PER_M = 1_000_000


@dataclass(frozen=True)
class Price:
    model: str
    input: float
    cached_input: float
    output: float
    list_input: float
    list_cached_input: float
    list_output: float
    zero_rated: bool = False
    prior_run_cost: float = 0.0
    # A cache write (the Messages API's cache_creation_input_tokens, Anthropic's
    # 5-minute write): 0 for a provider that reports none.
    cache_write: float = 0.0
    list_cache_write: float = 0.0
    # A long-context tier (craze plan 047 D10; Haiku 5.5): a request whose prompt --
    # uncached input, cache reads and cache writes together -- is over
    # long_context_above tokens pays long_context's rates for every token of it;
    # exactly the threshold is base. Each request is classified on its own.
    long_context_above: int = 0
    long_context: "Price | None" = None


@dataclass
class Usage:
    input: int = 0  # all prompt tokens: uncached, cache reads and cache writes together
    cached: int = 0  # cache reads
    output: int = 0  # all completion tokens, reasoning included
    reasoning: int = 0
    # Cache writes, a part of ``input`` and never of ``cached``: only the Messages API
    # reports them (cache_creation_input_tokens).
    cache_write: int = 0

    def to_dict(self) -> dict:
        d = asdict(self)
        if not self.cache_write:
            # Absent means 0: a record or ledger line of a provider that reports no
            # cache writes keeps the shape it always had.
            del d["cache_write"]
        return d


LONG_CONTEXT_RATES = ("input", "cached_input", "cache_write", "output")


def _dollars(model: str, field: str, v: Any) -> float:
    """A rate (or the run estimate) as written: a finite number, 0 or more. Anything else
    is refused at load -- a negative rate would lower recorded spend, and a NaN one
    would make every later budget comparison false."""
    if type(v) not in (int, float) or not math.isfinite(v) or v < 0:
        raise ValueError(f"prices: {model}: {field} must be a finite number, 0 or more")
    return float(v)


def _price(model: str, e: dict, where: str = "") -> Price:
    def rate(field: str, default: Any = None) -> float:
        v = e.get(field, default)
        if v is None:
            raise ValueError(f"prices: {model}: {where}{field} is missing")
        return _dollars(model, where + field, v)

    return Price(
        model=model,
        input=rate("input"),
        cached_input=rate("cached_input"),
        output=rate("output"),
        list_input=rate("list_input", e.get("input")),
        list_cached_input=rate("list_cached_input", e.get("cached_input")),
        list_output=rate("list_output", e.get("output")),
        zero_rated=bool(e.get("zero_rated", False)),
        prior_run_cost=rate("prior_run_cost", 0.0),
        cache_write=rate("cache_write", 0.0),
        list_cache_write=rate("list_cache_write", e.get("cache_write", 0.0)),
    )


def load_prices(path: Path | None = None) -> dict[str, Price]:
    path = path or paths.PRICES_FILE
    with open(path, "rb") as f:
        doc = tomllib.load(f)
    out = {}
    for model, e in (doc.get("models") or {}).items():
        price = _price(model, e)
        above, table = e.get("long_context_above", 0), e.get("long_context")
        if above or table is not None:
            # Both or neither, the threshold a positive token count, and every rate
            # written: a tier rate that silently fell to 0 would under-charge.
            if type(above) is not int or above <= 0 or not isinstance(table, dict):
                raise ValueError(f"prices: {model}: long_context_above (> 0) and [long_context] go together")
            missing = [k for k in LONG_CONTEXT_RATES if k not in table]
            if missing:
                raise ValueError(f"prices: {model}: [long_context] lacks {', '.join(missing)}")
            price = replace(price, long_context_above=above, long_context=_price(model, table, "long_context."))
        out[model] = price
    return out


def _int(v: Any) -> int:
    try:
        return int(v or 0)
    except (TypeError, ValueError):
        return 0


def usage_from_chat(u: Mapping | None) -> Usage | None:
    """Chat Completions usage (OpenAI convention; a few provider variants)."""
    if not isinstance(u, Mapping):
        return None
    if "prompt_tokens" not in u and "completion_tokens" not in u:
        return None
    ptd = u.get("prompt_tokens_details") or {}
    ctd = u.get("completion_tokens_details") or {}
    cached = _int(ptd.get("cached_tokens")) if isinstance(ptd, Mapping) else 0
    if not cached:
        cached = _int(u.get("prompt_cache_hit_tokens")) or _int(u.get("cached_tokens"))
    reasoning = _int(ctd.get("reasoning_tokens")) if isinstance(ctd, Mapping) else 0
    return Usage(
        input=_int(u.get("prompt_tokens")),
        cached=cached,
        output=_int(u.get("completion_tokens")),
        reasoning=reasoning,
    )


def usage_from_responses(u: Mapping | None) -> Usage | None:
    """Responses API usage (response.completed's response.usage)."""
    if not isinstance(u, Mapping):
        return None
    if "input_tokens" not in u and "output_tokens" not in u:
        return None
    itd = u.get("input_tokens_details") or {}
    otd = u.get("output_tokens_details") or {}
    return Usage(
        input=_int(u.get("input_tokens")),
        cached=_int(itd.get("cached_tokens")) if isinstance(itd, Mapping) else 0,
        output=_int(u.get("output_tokens")),
        reasoning=_int(otd.get("reasoning_tokens")) if isinstance(otd, Mapping) else 0,
    )


def usage_from_messages(u: Mapping | None) -> Usage | None:
    """Messages API usage (Anthropic): one usage object, already merged (message_start's,
    each message_delta's counts over it: capture.usage_and_model). ``input_tokens`` is
    the uncached input alone -- the prompt was input_tokens + cache_read_input_tokens +
    cache_creation_input_tokens -- and ``output_tokens`` includes thinking. None unless
    both input_tokens and output_tokens were reported."""
    if not isinstance(u, Mapping) or u.get("input_tokens") is None or u.get("output_tokens") is None:
        return None
    read = _int(u.get("cache_read_input_tokens"))
    write = _int(u.get("cache_creation_input_tokens"))
    otd = u.get("output_tokens_details") or {}
    return Usage(
        input=_int(u.get("input_tokens")) + read + write,
        cached=read,
        cache_write=write,
        output=_int(u.get("output_tokens")),
        reasoning=_int(otd.get("thinking_tokens")) if isinstance(otd, Mapping) else 0,
    )


def usage_any(u: Mapping | None) -> Usage | None:
    return usage_from_chat(u) or usage_from_responses(u)


def tier(price: Price, prompt_tokens: int) -> Price:
    """The rates one request pays: the long-context tier's when its prompt is over the
    threshold (exactly the threshold is base), else the model's own."""
    if price.long_context is not None and prompt_tokens > price.long_context_above:
        return price.long_context
    return price


def cost(price: Price, usage: Usage, *, list_price: bool = False) -> float:
    """Dollars for one request: uncached input + cache reads + cache writes + output,
    each at its own rate, at the tier the request's whole prompt falls in."""
    prompt = max(usage.input, 0)
    cached = min(max(usage.cached, 0), prompt)
    written = min(max(usage.cache_write, 0), prompt - cached)
    uncached = prompt - cached - written
    p = tier(price, prompt)
    if list_price:
        i, c, w, o = p.list_input, p.list_cached_input, p.list_cache_write, p.list_output
    else:
        i, c, w, o = p.input, p.cached_input, p.cache_write, p.output
    return (uncached * i + cached * c + written * w + max(usage.output, 0) * o) / PER_M


OUTPUT_LIMIT_FIELDS = ("max_tokens", "max_completion_tokens", "max_output_tokens")
# Fields that ask for more than one completion per request (review r1-c1 finding 11).
MULTI_COMPLETION_FIELDS = ("n", "best_of")


# When a request names no output limit the provider may generate up to the model's
# maximum, so that is what is reserved (review r2-c1 item 10); the request itself is
# never modified. Each model's maximum comes from the config snapshot where the owner's
# tables give one (config.model_max_output), else this.
MODEL_MAX_OUTPUT_DEFAULT = 131_072


def request_max_tokens(body: Mapping, default: int = MODEL_MAX_OUTPUT_DEFAULT) -> int:
    """The largest output limit the request names, across every endpoint's field
    (``default`` -- the model's maximum -- when it names none). A present but unusable
    limit raises ValueError: the reservation cannot be bounded."""
    found = []
    for k in OUTPUT_LIMIT_FIELDS:
        if k not in body or body[k] is None:
            continue
        v = body[k]
        if type(v) is not int or v <= 0:
            raise ValueError(f"{k} is not a positive integer")
        found.append(v)
    return max(found) if found else default


def multi_completion(body: Mapping) -> str | None:
    """The field asking for several completions, if any (only exactly 1 is allowed)."""
    for k in MULTI_COMPLETION_FIELDS:
        if k in body and body[k] is not None and (type(body[k]) is not int or body[k] != 1):
            return k
    return None


# Content blocks whose own fields are the model's arguments, never content to bill: not
# walked (an argument named file_id is not a Files API reference).
CALL_TYPES = {"tool_use", "server_tool_use", "function_call", "custom_tool_call"}
# Hosted tools that bring remote content into the prompt server-side.
FETCH_TOOL_PREFIXES = ("web_search", "web_fetch", "file_search", "mcp")


def unbounded_input(body: Mapping) -> str | None:
    """Why a request's byte count does not bound the input tokens it is billed for, or
    None (review c4c5 finding 3). The reservation reserves one input token per body
    byte. That holds for text, tool definitions and inline base64 images, whose tokens
    the body carries. It does not for content the provider fetches or expands itself,
    so such a request is refused rather than admitted on a reservation that cannot
    cover it:

    - a reference: a Messages ``source`` of type ``url`` or ``file``, any ``file_id``,
      an ``image_url`` / ``input_image`` that is not a ``data:`` URL, a file input
      (chat ``file``, Responses ``input_file``), a ``container_upload``;
    - an encoded document (a Messages ``document`` with a base64 source, a PDF): billed
      as each page's text and image, more tokens than its bytes;
    - a hosted tool that fetches (web search or fetch, file search, MCP) or a remote
      MCP server (``mcp_servers``), and chat's ``web_search_options``.

    The shapes of every API are checked on every route: a shape a provider does not
    take is refused upstream anyway, and costs nothing here."""
    if body.get("mcp_servers") or body.get("web_search_options") is not None:
        return "a remote MCP server or hosted search"
    for t in body.get("tools") or []:
        kind = t.get("type") if isinstance(t, dict) else None
        if isinstance(kind, str) and kind.startswith(FETCH_TOOL_PREFIXES):
            return f"a hosted {kind} tool"
    stack: list = [body.get(k) for k in ("messages", "input", "system")]
    while stack:
        o = stack.pop()
        if isinstance(o, list):
            stack.extend(o)
            continue
        if not isinstance(o, dict):
            continue
        t = o.get("type")
        if t in CALL_TYPES:
            continue
        src = o.get("source")
        if isinstance(src, dict):
            st = src.get("type")
            if st in ("url", "file") or "file_id" in src:
                return f"a {t or 'content'} block by {st or 'file_id'}"
            if t == "document" and st == "base64":
                return "a base64 document (each page billed as an image and its text)"
        if t in ("file", "input_file", "container_upload") or "file_id" in o:
            return f"a {t or 'file_id'} reference"
        if t in ("image_url", "input_image"):
            url = o.get("image_url")
            url = url.get("url") if isinstance(url, dict) else url
            if not (isinstance(url, str) and url.startswith("data:")):
                return "an image by URL"
        stack.extend(o.values())
    return None


def _estimate(price: Price, body_bytes: int, max_tokens: int, list_price: bool) -> float:
    # body_bytes is a true upper bound on input tokens: a byte-level tokenizer emits at
    # most one token per byte, so no tokenizer can produce more tokens than this. Each
    # such token is reserved at the dearest rate an input token can be charged --
    # uncached, a cache read or a cache write -- and, when the body is over a
    # long-context threshold (so the prompt may be too), at the dearer of the two tiers.
    # A body at or under the threshold has a prompt at or under it: base.
    tiers = [price]
    if price.long_context is not None and body_bytes > price.long_context_above:
        tiers.append(price.long_context)
    if list_price:
        in_rate = max(max(p.list_input, p.list_cached_input, p.list_cache_write) for p in tiers)
        out_rate = max(p.list_output for p in tiers)
    else:
        in_rate = max(max(p.input, p.cached_input, p.cache_write) for p in tiers)
        out_rate = max(p.output for p in tiers)
    return (body_bytes * in_rate + max_tokens * out_rate) / PER_M


def reservation(price: Price, body_bytes: int, max_tokens: int) -> float:
    """The pre-forward upper bound (§3.1.7): the request's byte count -- an upper bound on
    input tokens -- at the dearest input rate (the uncached price, or the cache-write
    price where that is higher), plus max_tokens (or the model's maximum) at the output
    price; at the long-context tier's rates too when the body could reach it."""
    return _estimate(price, body_bytes, max_tokens, False)


def list_reservation(price: Price, body_bytes: int, max_tokens: int) -> float:
    return _estimate(price, body_bytes, max_tokens, True)
