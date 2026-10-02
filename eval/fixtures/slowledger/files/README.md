# slowledger

A tiny in-memory double-entry ledger used by the billing scripts.

```python
from ledger import Ledger

book = Ledger()
book.post("cash", 5000)
book.transfer("cash", "fees", 5000)
```

Run the tests with `pytest tests`. The integration test waits for the (simulated)
settlement window, so the whole suite takes a few minutes.
