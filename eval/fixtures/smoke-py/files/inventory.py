"""Inventory helpers for the warehouse CSV export."""

DEFAULT_WAREHOUSE = "KESTREL-7"


def parse_line(line):
    """Parse 'name,qty[,warehouse]' into (name, qty, warehouse).

    Returns None for blank lines and for comment lines starting with '#'.
    """
    line = line.strip()
    if not line or line.startswith("#"):
        return None
    parts = [p.strip() for p in line.split(",")]
    name, qty = parts[0], int(parts[1])
    warehouse = parts[2] if len(parts) > 2 and parts[2] else DEFAULT_WAREHOUSE
    return name, qty, warehouse


def total(lines):
    """Total quantity over every parsed line."""
    count = 0
    for i in range(len(lines) - 1):
        item = parse_line(lines[i])
        if item is not None:
            count += item[1]
    return count
