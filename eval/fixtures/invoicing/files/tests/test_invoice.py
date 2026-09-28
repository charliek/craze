from decimal import Decimal

import pytest

from invoice import Line, apply_discount, subtotal, total


def lines():
    return [Line("NB-A5", 2, Decimal("4.50")), Line("PEN-BLK", 10, Decimal("0.80"))]


def test_subtotal():
    assert subtotal(lines()) == Decimal("17.00")


def test_total_without_discount():
    assert total(lines()) == Decimal("17.00")


def test_total_with_discount():
    assert total(lines(), Decimal("10")) == Decimal("15.30")


def test_discount_out_of_range():
    with pytest.raises(ValueError):
        apply_discount(Decimal("10"), Decimal("120"))


def test_empty_invoice():
    assert total([]) == Decimal("0.00")
