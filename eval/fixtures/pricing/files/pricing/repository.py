"""Product data, read from the catalog database."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal
from typing import Protocol


@dataclass(frozen=True)
class Product:
    sku: str
    name: str
    list_price: Decimal
    currency: str = "USD"


@dataclass(frozen=True)
class VolumeTier:
    min_qty: int
    percent_off: Decimal


class Database(Protocol):
    def query(self, sql: str, *params) -> list[tuple]: ...

    def execute(self, sql: str, *params) -> None: ...


class ProductRepository:
    """Every read is a round trip to the catalog database (~40 ms at p50)."""

    def __init__(self, db: Database):
        self.db = db

    def get(self, sku: str) -> Product | None:
        rows = self.db.query("SELECT sku, name, list_price, currency FROM products WHERE sku = ?", sku)
        if not rows:
            return None
        sku, name, price, currency = rows[0]
        return Product(sku, name, Decimal(price), currency)

    def volume_tiers(self, sku: str) -> list[VolumeTier]:
        rows = self.db.query(
            "SELECT min_qty, percent_off FROM volume_tiers WHERE sku = ? ORDER BY min_qty", sku
        )
        return [VolumeTier(q, Decimal(p)) for q, p in rows]

    def set_list_price(self, sku: str, price: Decimal) -> None:
        self.db.execute("UPDATE products SET list_price = ? WHERE sku = ?", str(price), sku)

    def replace_volume_tiers(self, sku: str, tiers: list[VolumeTier]) -> None:
        self.db.execute("DELETE FROM volume_tiers WHERE sku = ?", sku)
        for t in tiers:
            self.db.execute(
                "INSERT INTO volume_tiers (sku, min_qty, percent_off) VALUES (?, ?, ?)", sku, t.min_qty, str(t.percent_off)
            )
