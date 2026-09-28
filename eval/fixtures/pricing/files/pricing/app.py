"""Wiring: one PriceService and one PriceAdmin per process."""

from __future__ import annotations

from pricing.admin import PriceAdmin
from pricing.repository import Database, ProductRepository
from pricing.service import PriceService


def build(db: Database, audit_log: list[str]) -> tuple[PriceService, PriceAdmin]:
    products = ProductRepository(db)
    return PriceService(products), PriceAdmin(products, audit_log)
