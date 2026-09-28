"""Budget reservations, the persisted ledger and cost math (plan 029 §3.1.7, AC-A4)."""

from __future__ import annotations

import json
import multiprocessing
import threading

import pytest

from crazeeval.ledger import BUDGET_CAPPED, BUDGET_STOP, Ledger
from crazeeval.pricing import (
    MODEL_MAX_OUTPUT_DEFAULT,
    Usage,
    cost,
    load_prices,
    request_max_tokens,
    reservation,
    usage_from_chat,
    usage_from_responses,
)


def seed(path, amount):
    """A ledger whose committed spend is ``amount`` (one settled request)."""
    led = Ledger(path, cap=95.0, run_cap=1e9)
    a = led.admit(run_id="seed", harness="h", model="m", provider="p", amount=0.0)
    led.settle(a.reservation, Usage(input=1), amount, amount)
    led.close()


def test_edge_at_94_99_admits_only_what_fits(tmp_path):
    p = tmp_path / "ledger.jsonl"
    seed(p, 94.99)
    led = Ledger(p, cap=95.0, run_cap=100.0)  # reloaded from the file
    assert led.totals()["total"] == pytest.approx(94.99)
    assert led.admit(run_id="r", harness="h", model="m", provider="p", amount=0.005).ok
    refused = led.admit(run_id="r", harness="h", model="m", provider="p", amount=0.02)
    assert not refused.ok and refused.reason == BUDGET_STOP
    led.close()


def test_edge_at_95_01_refuses_everything_even_free(tmp_path):
    p = tmp_path / "ledger.jsonl"
    seed(p, 95.01)
    led = Ledger(p, cap=95.0)
    for amount in (0.0, 0.001):
        a = led.admit(run_id="r", harness="h", model="glm-5.3-flash", provider="p", amount=amount)
        assert not a.ok and a.reason == BUDGET_STOP
    led.close()


def test_concurrent_admission_never_passes_the_cap(tmp_path):
    p = tmp_path / "ledger.jsonl"
    seed(p, 94.0)
    led = Ledger(p, cap=95.0, run_cap=1e9)
    admitted = []

    def worker():
        for _ in range(20):
            a = led.admit(run_id="r", harness="h", model="m", provider="p", amount=0.0625)
            if a.ok:
                admitted.append(a.reservation.amount)

    ts = [threading.Thread(target=worker) for _ in range(8)]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    assert len(admitted) == 16  # 1.0 / 0.0625, exact in binary
    assert led.totals()["total"] <= 95.0 + 1e-9
    led.close()


def _proc_admit(path, n, amount, q):
    led = Ledger(path, cap=95.0, run_cap=1e9)
    ok = 0
    for _ in range(n):
        if led.admit(run_id="x", harness="h", model="m", provider="p", amount=amount).ok:
            ok += 1
    led.close()
    q.put(ok)


def test_file_lock_keeps_two_processes_under_the_cap(tmp_path):
    p = tmp_path / "ledger.jsonl"
    seed(p, 94.0)
    ctx = multiprocessing.get_context("fork")
    q = ctx.Queue()
    procs = [ctx.Process(target=_proc_admit, args=(str(p), 30, 0.125, q)) for _ in range(3)]
    for pr in procs:
        pr.start()
    for pr in procs:
        pr.join(60)
    total_ok = sum(q.get() for _ in procs)
    assert total_ok == 8  # 1.0 / 0.125, shared between three processes
    led = Ledger(p, cap=95.0)
    assert led.totals()["total"] == pytest.approx(95.0)
    led.close()


def test_per_run_cap(tmp_path):
    led = Ledger(tmp_path / "l.jsonl", cap=95.0, run_cap=3.0)
    # The cap counts what the run has actually spent: open reservations, however large, do not cap it
    # (a no-limit request reserves the model's whole output ceiling).
    first = led.admit(run_id="a", harness="h", model="m", provider="p", amount=5.9)
    assert first.ok
    assert led.admit(run_id="a", harness="h", model="m", provider="p", amount=5.9).ok
    led.settle(first.reservation, Usage(input=10, output=1), 3.1, 3.1)  # this run has now spent $3.10
    a = led.admit(run_id="a", harness="h", model="m", provider="p", amount=0.6)
    assert not a.ok and a.reason == BUDGET_CAPPED
    assert led.admit(run_id="b", harness="h", model="m", provider="p", amount=0.6).ok  # another run is fine
    led.close()


def test_settle_reconciles_or_keeps_the_reservation(tmp_path):
    led = Ledger(tmp_path / "l.jsonl", cap=95.0)
    a = led.admit(run_id="a", harness="h", model="m", provider="p", amount=0.5)
    b = led.admit(run_id="a", harness="h", model="m", provider="p", amount=0.4)
    led.admit(run_id="a", harness="h", model="m", provider="p", amount=0.3)  # stays in flight
    assert led.totals()["reserved"] == pytest.approx(1.2)
    led.settle(a.reservation, Usage(input=10, output=1), 0.01, 0.01)  # reconciled to actual
    led.settle(b.reservation, None, None, None, status=None, note="client disconnected")  # kept
    t = led.totals()
    assert t["committed"] == pytest.approx(0.41)
    assert t["reserved"] == pytest.approx(0.3)  # c is still in flight
    led.close()
    # A crash leaves c unsettled: reloading still counts its reservation.
    led2 = Ledger(tmp_path / "l.jsonl", cap=95.0)
    assert led2.totals()["total"] == pytest.approx(0.71)
    led2.close()


def test_ledger_lines_are_fsynced_jsonl(tmp_path):
    led = Ledger(tmp_path / "l.jsonl")
    a = led.admit(run_id="a", harness="craze", model="glm-5.3-flash", provider="zai-coding-plan", amount=0.0)
    led.settle(a.reservation, Usage(100, 20, 10, 0), 0.0, 0.0002)
    led.close()
    lines = [json.loads(x) for x in (tmp_path / "l.jsonl").read_text().splitlines()]
    assert [x["kind"] for x in lines] == ["reserve", "settle"]
    assert lines[1]["run_id"] == "a" and lines[1]["harness"] == "craze" and lines[1]["usage"]["input"] == 100


def test_cost_math_with_cached_tokens():
    prices = load_prices()
    p = prices["accounts/fireworks/models/kimi-k3"]
    u = Usage(input=10_000, cached=6_000, output=1_000)
    assert cost(p, u) == pytest.approx((4_000 * 4.50 + 6_000 * 0.45 + 1_000 * 22.50) / 1e6)
    assert cost(p, u, list_price=True) == pytest.approx((4_000 * 3.00 + 6_000 * 0.30 + 1_000 * 15.00) / 1e6)
    z = prices["glm-5.3-flash"]
    assert z.zero_rated and cost(z, u) == 0.0 and cost(z, u, list_price=True) > 0
    c = prices["muse-spark-1.3-contributor"]
    assert cost(c, u) == pytest.approx((10_000 * 0.10 + 1_000 * 0.20) / 1e6)  # cached at the full rate


def test_reservation_formula():
    p = load_prices()["muse-spark-1.3"]
    assert reservation(p, 3000, 1000) == pytest.approx((1000 * 1.25 + 1000 * 4.25) / 1e6)
    # No limit in the request: the model's maximum (review r2-c1 item 10).
    assert request_max_tokens({}) == MODEL_MAX_OUTPUT_DEFAULT == 131_072
    assert request_max_tokens({}, 65_536) == 65_536
    assert request_max_tokens({"max_tokens": 10, "max_output_tokens": 500}, 65_536) == 500
    assert request_max_tokens({"max_completion_tokens": 77}) == 77
    assert request_max_tokens({"max_output_tokens": 99}) == 99


def test_usage_normalisation():
    u = usage_from_chat({"prompt_tokens": 50, "completion_tokens": 7, "prompt_cache_hit_tokens": 20})
    assert (u.input, u.cached, u.output) == (50, 20, 7)
    r = usage_from_responses({"input_tokens": 9, "output_tokens": 3, "input_tokens_details": {"cached_tokens": 4},
                              "output_tokens_details": {"reasoning_tokens": 2}})
    assert (r.input, r.cached, r.output, r.reasoning) == (9, 4, 3, 2)
    assert usage_from_chat(None) is None and usage_from_chat({"x": 1}) is None


def test_a_torn_line_does_not_swallow_the_next_one(tmp_path):
    p = tmp_path / "l.jsonl"
    seed(p, 1.0)
    with open(p, "ab") as f:
        f.write(b'{"kind": "reserve", "id": "dead", "reserv')  # a writer died mid-line
    led = Ledger(p, cap=95.0)
    assert led.admit(run_id="r", harness="h", model="m", provider="p", amount=2.0).ok
    led.close()
    led2 = Ledger(p, cap=95.0)
    assert led2.totals()["total"] == pytest.approx(3.0)
    assert led2.torn_lines == 1
    led2.close()


def test_every_eval_model_is_priced():
    from crazeeval.config import load_models

    prices = load_prices()
    for em in load_models().values():
        assert em.wire_model in prices, em.key
