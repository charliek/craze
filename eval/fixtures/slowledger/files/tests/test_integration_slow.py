import time

from ledger import Ledger

SETTLEMENT_WINDOW_S = 150


def test_roundtrip():
    """Post, move and read back across the (simulated) settlement window."""
    book = Ledger()
    book.post("cash", 10_000)
    book.transfer("cash", "fees", 2_500)
    time.sleep(SETTLEMENT_WINDOW_S)  # the settlement service has no faster mode
    book.transfer("fees", "refunds", 500)
    assert book.balance("cash") == 7_500
    assert book.balance("fees") == 2_000
    assert book.balance("refunds") == 500
    assert book.accounts() == ["cash", "fees", "refunds"]
