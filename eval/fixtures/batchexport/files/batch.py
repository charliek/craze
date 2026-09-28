"""Parse the nightly batch export from the warehouse system.

An export is text: a header line, then one record per line, fields separated by
"|". Blank lines are ignored.

    sku|qty|location
    NB-A5|12|A-03
    PEN-BLK|200|B-11
"""

from __future__ import annotations

from dataclasses import dataclass


class ExportError(ValueError):
    pass


@dataclass(frozen=True)
class Record:
    sku: str
    qty: int
    location: str


EXPECTED_HEADER = ["sku", "qty", "location"]


def parse_batch(text: str) -> list[Record]:
    """Parse an export. An export with no records yields an empty list."""
    header, *rows = text.splitlines()
    if [h.strip().lower() for h in header.split("|")] != EXPECTED_HEADER:
        raise ExportError(f"unexpected header: {header!r}")
    records = []
    for n, row in enumerate(rows, start=2):
        if not row.strip():
            continue
        fields = [f.strip() for f in row.split("|")]
        if len(fields) != 3:
            raise ExportError(f"line {n}: expected 3 fields, got {len(fields)}")
        sku, qty, location = fields
        try:
            q = int(qty)
        except ValueError:
            raise ExportError(f"line {n}: qty {qty!r} is not a number") from None
        records.append(Record(sku, q, location))
    return records


def total_quantity(records: list[Record]) -> int:
    return sum(r.qty for r in records)
