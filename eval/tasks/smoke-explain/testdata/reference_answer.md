When a line has no warehouse column, `parse_line` falls back to `DEFAULT_WAREHOUSE`, which is `"KESTREL-7"` (`inventory.py:3`, used at `inventory.py:16`).

For a blank line (after stripping whitespace) and for a comment line starting with `#`, it returns `None` without parsing anything (`inventory.py:11-13`).
