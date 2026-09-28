**No.** An empty string makes `parse_batch` raise a plain `ValueError`:

```
$ python3 -c 'import batch; print(batch.parse_batch(""))'
Traceback (most recent call last):
  ...
  File "batch.py", line 32, in parse_batch
    header, *rows = text.splitlines()
ValueError: not enough values to unpack (expected at least 1, got 0)
```

The first line of the function is `header, *rows = text.splitlines()`, and `"".splitlines()` is `[]`, so the unpacking fails before the header check ever runs. It is not the module's `ExportError`, so a caller catching `ExportError` won't catch it.

For comparison (also run):

- `parse_batch("sku|qty|location")` (header only) → `[]`
- `parse_batch("\n")` → `ExportError: unexpected header: ''`

So the docstring's "an export with no records yields an empty list" is true for a header-only export but not for an empty file. I didn't change anything.
