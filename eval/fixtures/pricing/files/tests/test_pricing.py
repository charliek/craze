from decimal import Decimal

import pytest

from pricing.app import build
from pricing.repository import VolumeTier
from pricing.service import UnknownProduct


class FakeDB:
    """Just enough SQL for the repository's statements."""

    def __init__(self):
        self.products = {"NB-A5": ("NB-A5", "Notebook A5", "4.50", "USD")}
        self.tiers = {"NB-A5": [(10, "5"), (100, "12.5")]}
        self.queries = 0

    def query(self, sql, *params):
        self.queries += 1
        sku = params[0]
        if "FROM products" in sql:
            return [self.products[sku]] if sku in self.products else []
        return list(self.tiers.get(sku, []))

    def execute(self, sql, *params):
        if sql.startswith("UPDATE products"):
            price, sku = params
            p = self.products[sku]
            self.products[sku] = (p[0], p[1], price, p[3])
        elif sql.startswith("DELETE FROM volume_tiers"):
            self.tiers[params[0]] = []
        elif sql.startswith("INSERT INTO volume_tiers"):
            sku, q, pct = params
            self.tiers.setdefault(sku, []).append((q, pct))


@pytest.fixture
def world():
    db = FakeDB()
    log = []
    service, admin = build(db, log)
    return db, service, admin, log


def test_quote_without_tier(world):
    _, service, _, _ = world
    q = service.quote("NB-A5", 2)
    assert (q.unit_price, q.total) == (Decimal("4.50"), Decimal("9.00"))


def test_best_tier_applies(world):
    _, service, _, _ = world
    assert service.quote("NB-A5", 150).unit_price == Decimal("3.94")


def test_unknown_product(world):
    _, service, _, _ = world
    with pytest.raises(UnknownProduct):
        service.quote("NOPE", 1)


def test_price_update_is_seen_by_quotes(world):
    _, service, admin, log = world
    admin.update_price("NB-A5", Decimal("5.00"), "sam")
    assert service.quote("NB-A5", 1).unit_price == Decimal("5.00")
    assert log == ["sam set NB-A5 to 5.00"]


def test_tier_update_is_seen_by_quotes(world):
    _, service, admin, _ = world
    admin.update_tiers("NB-A5", [VolumeTier(2, Decimal("50"))], "sam")
    assert service.quote("NB-A5", 2).unit_price == Decimal("2.25")
