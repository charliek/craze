import pytest

from ledger import InsufficientFunds, Ledger


def test_post_and_balance():
    book = Ledger()
    book.post("cash", 500)
    book.post("cash", -120)
    assert book.balance("cash") == 380


def test_transfer_moves_cents():
    book = Ledger()
    book.post("cash", 1000)
    book.transfer("cash", "fees", 250)
    assert book.balance("cash") == 750
    assert book.balance("fees") == 250


def test_transfer_whole_balance():
    book = Ledger()
    book.post("cash", 1000)
    book.transfer("cash", "fees", 1000)
    assert book.balance("cash") == 0
    assert book.balance("fees") == 1000


def test_transfer_more_than_balance_is_refused():
    book = Ledger()
    book.post("cash", 100)
    with pytest.raises(InsufficientFunds):
        book.transfer("cash", "fees", 101)


def test_transfer_needs_a_positive_amount():
    book = Ledger()
    book.post("cash", 100)
    with pytest.raises(ValueError):
        book.transfer("cash", "fees", 0)
