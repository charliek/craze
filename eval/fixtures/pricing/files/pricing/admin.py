"""The merchandising team's price changes (called from the admin UI)."""

from __future__ import annotations

from decimal import Decimal

from pricing.repository import ProductRepository, VolumeTier


class PriceAdmin:
    def __init__(self, products: ProductRepository, audit_log: list[str]):
        self.products = products
        self.audit_log = audit_log

    def update_price(self, sku: str, price: Decimal, user: str) -> None:
        if price <= 0:
            raise ValueError("price must be positive")
        self.products.set_list_price(sku, price)
        self.audit_log.append(f"{user} set {sku} to {price}")

    def update_tiers(self, sku: str, tiers: list[VolumeTier], user: str) -> None:
        self.products.replace_volume_tiers(sku, sorted(tiers, key=lambda t: t.min_qty))
        self.audit_log.append(f"{user} replaced the volume tiers of {sku}")
