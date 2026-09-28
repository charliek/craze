import pytest

from batch import ExportError, Record, parse_batch, total_quantity

EXPORT = "sku|qty|location\nNB-A5|12|A-03\n\nPEN-BLK|200|B-11\n"


def test_parse():
    assert parse_batch(EXPORT) == [Record("NB-A5", 12, "A-03"), Record("PEN-BLK", 200, "B-11")]


def test_header_only():
    assert parse_batch("sku|qty|location\n") == []


def test_bad_header():
    with pytest.raises(ExportError):
        parse_batch("id|count\n1|2\n")


def test_bad_qty():
    with pytest.raises(ExportError, match="line 2"):
        parse_batch("sku|qty|location\nX|many|A-01\n")


def test_total_quantity():
    assert total_quantity(parse_batch(EXPORT)) == 212
