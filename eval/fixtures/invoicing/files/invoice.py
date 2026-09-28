"""Invoice totals."""

from dataclasses import dataclass
from decimal import ROUND_HALF_UP, Decimal

CENT = Decimal("0.01")


@dataclass(frozen=True)
class Line:
    sku: str
    quantity: int
    unit_price: Decimal


def _round(amount: Decimal) -> Decimal:
    return amount.quantize(CENT, rounding=ROUND_HALF_UP)


def subtotal(lines: list[Line]) -> Decimal:
    return sum((line.unit_price * line.quantity for line in lines), Decimal("0"))


def apply_discount(amount: Decimal, percent: Decimal) -> Decimal:
    """Take ``percent`` (0-100) off ``amount``."""
    if not Decimal("0") <= percent <= Decimal("100"):
        raise ValueError(f"discount must be between 0 and 100, got {percent}")
    return amount * (Decimal("100") - percent) / Decimal("100")


def total(lines: list[Line], discount_percent: Decimal = Decimal("0")) -> Decimal:
    """The amount due, rounded to cents."""
    return _round(apply_discount(subtotal(lines), discount_percent))
