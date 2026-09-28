The change came in **46d165a "Share one money helper across invoice and export"**.

Before it, `invoice.total` rounded with `_round`, which called `amount.quantize(CENT, rounding=ROUND_HALF_UP)`. That commit replaced `_round` with `money.to_cents`:

```python
def to_cents(amount: Decimal) -> Decimal:
    return amount.quantize(CENT)
```

With no `rounding=` argument, `quantize` uses the decimal context's default, `ROUND_HALF_EVEN` (banker's rounding). The two only disagree on an exact half cent, which is why it is not every invoice: 3 × 0.75 = 2.25, less 10% is 2.025; half-up gives 2.03, half-even gives 2.02 (2 is even).

Evidence:

```
$ git log --oneline v1.1..v1.3
d102a87 Release 1.3
309ee6d Add invoice numbering
2853b8c Fix typo in export header
46d165a Share one money helper across invoice and export
c856921 Support per-customer tax rates
$ git bisect run python -c "..."   # total([Line('NB',3,Decimal('0.75'))], Decimal('10')) == Decimal('2.03')
46d165a is the first bad commit
```

At `c856921` the example still gives 2.03; at `46d165a` it gives 2.02. The same commit also moved the CSV export's line amounts to `to_cents`, so exported half-cent amounts changed too. The README still says halves round up.

I ran `git bisect reset` afterwards; nothing in the working tree changed.
