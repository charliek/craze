import pytest

from ledger import InsufficientFunds, Ledger


def test_fix():
    book = Ledger()
    book.post("cash", 4200)
    book.transfer("cash", "fees", 4200)
    assert book.balance("cash") == 0
    assert book.balance("fees") == 4200


def test_one_cent_over_is_still_refused():
    book = Ledger()
    book.post("cash", 4200)
    with pytest.raises(InsufficientFunds):
        book.transfer("cash", "fees", 4201)
    assert book.balance("cash") == 4200


def test_statement_after_emptying():
    book = Ledger()
    book.post("cash", 300)
    book.transfer("cash", "fees", 300)
    assert book.statement("cash") == [300, -300]
