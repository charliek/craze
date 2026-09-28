"""Prices, usage normalisation and cost math (plan 029 §2.7, §3.1.3, §3.1.7)."""

from __future__ import annotations

import tomllib
from dataclasses import asdict, dataclass
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


@dataclass
class Usage:
    input: int = 0  # all prompt tokens, cached included
    cached: int = 0
    output: int = 0  # all completion tokens, reasoning included
    reasoning: int = 0

    def to_dict(self) -> dict:
        return asdict(self)


def load_prices(path: Path | None = None) -> dict[str, Price]:
    path = path or paths.PRICES_FILE
    with open(path, "rb") as f:
        doc = tomllib.load(f)
    out = {}
    for model, e in (doc.get("models") or {}).items():
        out[model] = Price(
            model=model,
            input=float(e["input"]),
            cached_input=float(e["cached_input"]),
            output=float(e["output"]),
            list_input=float(e.get("list_input", e["input"])),
            list_cached_input=float(e.get("list_cached_input", e["cached_input"])),
            list_output=float(e.get("list_output", e["output"])),
            zero_rated=bool(e.get("zero_rated", False)),
            prior_run_cost=float(e.get("prior_run_cost", 0.0)),
        )
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


def usage_any(u: Mapping | None) -> Usage | None:
    return usage_from_chat(u) or usage_from_responses(u)


def cost(price: Price, usage: Usage, *, list_price: bool = False) -> float:
    """Dollars for one request: uncached input + cached input + output."""
    cached = min(max(usage.cached, 0), max(usage.input, 0))
    uncached = max(usage.input - cached, 0)
    if list_price:
        i, c, o = price.list_input, price.list_cached_input, price.list_output
    else:
        i, c, o = price.input, price.cached_input, price.output
    return (uncached * i + cached * c + max(usage.output, 0) * o) / PER_M


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


def _estimate(body_bytes: int, max_tokens: int, input_rate: float, output_rate: float) -> float:
    # body_bytes is a true upper bound on input tokens: a byte-level tokenizer emits at
    # most one token per byte, so no tokenizer can produce more tokens than this.
    return (body_bytes * input_rate + max_tokens * output_rate) / PER_M


def reservation(price: Price, body_bytes: int, max_tokens: int) -> float:
    """The pre-forward upper bound (§3.1.7): the request's byte count -- an upper bound on
    input tokens -- at the uncached ledger price, plus max_tokens (or the model's maximum)
    at the output price."""
    return _estimate(body_bytes, max_tokens, price.input, price.output)


def list_reservation(price: Price, body_bytes: int, max_tokens: int) -> float:
    return _estimate(body_bytes, max_tokens, price.list_input, price.list_output)
