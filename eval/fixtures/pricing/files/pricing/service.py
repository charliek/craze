"""Price quotes for the storefront and the order service."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import ROUND_HALF_UP, Decimal

from pricing.repository import ProductRepository


class UnknownProduct(LookupError):
    pass


@dataclass(frozen=True)
class Quote:
    sku: str
    qty: int
    unit_price: Decimal
    total: Decimal
    currency: str


class PriceService:
    def __init__(self, products: ProductRepository):
        self.products = products

    def quote(self, sku: str, qty: int) -> Quote:
        """Price ``qty`` units of ``sku``, with the best volume tier applied."""
        if qty < 1:
            raise ValueError("qty must be at least 1")
        product = self.products.get(sku)
        if product is None:
            raise UnknownProduct(sku)
        unit = product.list_price
        best = Decimal("0")
        for tier in self.products.volume_tiers(sku):
            if qty >= tier.min_qty:
                best = max(best, tier.percent_off)
        unit = (unit * (Decimal("100") - best) / Decimal("100")).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)
        return Quote(sku, qty, unit, unit * qty, product.currency)

    def quote_cart(self, items: dict[str, int]) -> list[Quote]:
        return [self.quote(sku, qty) for sku, qty in items.items()]
