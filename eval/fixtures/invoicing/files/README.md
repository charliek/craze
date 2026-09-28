# invoicing

Small helpers for computing invoice totals.

```python
from decimal import Decimal
from invoice import Line, total

total([Line("NB-A5", 2, Decimal("4.50"))], discount_percent=Decimal("10"))  # Decimal("8.10")
```

Run the tests with `pytest`.
