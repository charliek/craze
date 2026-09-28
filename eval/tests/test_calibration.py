"""Calibration gates and their enforcement (plan 029 §3.1.6; review r1-c2 §7): a gate
passes only with every requested pair judged in both orders, and batch judging obeys
the decisions (luna only after its agreement gate, both orders unless the flip gate
passed), with a recorded override."""

from __future__ import annotations

import json

import pytest

from crazeeval import judging as jg
from crazeeval.judging import (
    FAIL,
    INCOMPLETE,
    PASS,
    CalibrationRefusal,
    agreement,
    enforce_calibration,
    flip_rate,
    gate_decisions,
    load_calibration,
    padding_preference,
)


def both(pair: str, w1: str, w2: str, conf="high") -> dict:
    """A both-order record whose orders said w1 and w2 (x/y/tie)."""
    orders = [{"verdict": None if w is None else {"winner": w, "confidence": conf}} for w in (w1, w2)]
    res = None if None in (w1, w2) else {"winner": w1 if w1 == w2 else "tie", "agree": w1 == w2, "confidence": conf}
    return {"pair": pair, "orders": orders, "result": res}


def full(n: int, flips: int = 0, winner: str = "x") -> list[dict]:
    return [both(f"p{i}", winner, "y" if i < flips else winner) for i in range(n)]


def test_complete_calibration_passes_its_gates():
    sol, luna = full(30, flips=3), full(30, flips=3)
    pad = [both(f"q{i}", "x", "x") for i in range(10)]
    g = gate_decisions(flip_rate(sol, 30), agreement(sol, luna, 30), padding_preference(pad, 10))
    assert (g["flip_gate"], g["luna_gate"], g["padding_gate"]) == (PASS, PASS, PASS) and g["complete"]
    assert not g["both_orders_for_all_pairs"] and g["luna_judges_bulk_pairs"] and g["padding_ok"]


def test_failing_gates():
    sol = full(30, flips=7)  # 23% flip
    luna = [both(f"p{i}", "y", "y") for i in range(30)]  # disagrees with sol everywhere
    pad = [both(f"q{i}", "y", "y") for i in range(3)] + [both(f"q{i}", "x", "x") for i in range(3, 10)]
    g = gate_decisions(flip_rate(sol, 30), agreement(sol, luna, 30), padding_preference(pad, 10))
    assert (g["flip_gate"], g["luna_gate"], g["padding_gate"]) == (FAIL, FAIL, FAIL)
    assert g["both_orders_for_all_pairs"] and not g["luna_judges_bulk_pairs"] and not g["padding_ok"]


@pytest.mark.parametrize("case", ["one-luna-verdict", "missing-order", "too-few-pairs", "no-padding"])
def test_incomplete_calibration_fails_every_gate_it_touches(case):
    sol, luna = full(30), full(30)
    pad = [both(f"q{i}", "x", "x") for i in range(10)]
    if case == "one-luna-verdict":
        # One agreeing luna verdict out of 30 would read 100% agreement.
        luna = [both("p0", "x", "x")] + [both(f"p{i}", None, None) for i in range(1, 30)]
    elif case == "missing-order":
        sol = full(29) + [both("p29", "x", None)]
    elif case == "too-few-pairs":
        sol, luna = full(12), full(12)
    elif case == "no-padding":
        pad = [both(f"q{i}", None, "x") for i in range(10)]  # zero usable padding verdicts
    flip, agree, padd = flip_rate(sol, 30), agreement(sol, luna, 30), padding_preference(pad, 10)
    g = gate_decisions(flip, agree, padd)
    assert not g["complete"]
    if case == "one-luna-verdict":
        assert agree["rate"] == 1.0 and g["luna_gate"] == INCOMPLETE and not g["luna_judges_bulk_pairs"]
    elif case in ("missing-order", "too-few-pairs"):
        assert g["flip_gate"] == INCOMPLETE and g["both_orders_for_all_pairs"]
        assert g["luna_gate"] == INCOMPLETE and not g["luna_judges_bulk_pairs"]
    else:
        assert padd["padded_preferred"] == 0 and g["padding_gate"] == INCOMPLETE and not g["padding_ok"]


def _write_cal(tmp_path, gates: dict, jhash="H") -> object:
    p = tmp_path / "calibration.json"
    p.write_text(json.dumps({"judge_hash": jhash, "gates": gates}))
    return p


def test_enforcement_follows_the_calibration(tmp_path):
    passed = load_calibration(_write_cal(tmp_path, {"flip_gate": PASS, "luna_gate": PASS, "complete": True}), "H")
    assert enforce_calibration("luna", "single", passed)[0] == "single"
    assert enforce_calibration("sol", "single", passed)[0] == "single"

    tripped = load_calibration(_write_cal(tmp_path, {"flip_gate": FAIL, "luna_gate": FAIL, "complete": True}), "H")
    mode, rec = enforce_calibration("sol", "single", tripped)
    assert mode == "both" and rec["forced_both_orders"]
    with pytest.raises(CalibrationRefusal):
        enforce_calibration("luna", "both", tripped)
    assert enforce_calibration("sol", "success-bar", tripped)[0] == "success-bar"

    # An override is honoured and recorded.
    mode, rec = enforce_calibration("luna", "single", tripped, override="owner asked for a luna pass")
    assert mode == "single" and rec["override"] == "owner asked for a luna pass"


@pytest.mark.parametrize("how", ["missing", "stale", "incomplete"])
def test_no_valid_calibration_means_both_orders_and_no_luna(tmp_path, how):
    if how == "missing":
        cal = load_calibration(tmp_path / "nope.json", "H")
    elif how == "stale":
        cal = load_calibration(_write_cal(tmp_path, {"flip_gate": PASS, "luna_gate": PASS}, jhash="OLD"), "H")
    else:
        cal = load_calibration(_write_cal(tmp_path, {"flip_gate": INCOMPLETE, "luna_gate": INCOMPLETE,
                                                     "complete": False}), "H")
    assert cal["both_orders_required"] and not cal["luna_allowed"]
    assert enforce_calibration("sol", "single", cal)[0] == "both"
    with pytest.raises(CalibrationRefusal):
        enforce_calibration("luna", "single", cal)


def test_judge_batch_cli_refuses_luna_without_calibration(tmp_path, capsys):
    from crazeeval.cli import main

    batch = tmp_path / "b"
    (batch / "runs").mkdir(parents=True)
    assert main(["judge-batch", "--batch", str(batch), "--judge", "luna"]) == 2
    assert "refused" in capsys.readouterr().err
    assert jg.CALIBRATION_FILE == "calibration.json"
