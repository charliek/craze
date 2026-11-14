# orvelline

A small Python client for the Orvelline Passes API (version 2), which our travel desk uses
to book regional rail passes for staff. Standard library only: `urllib` for HTTP, `json`
for bodies.

```python
from orvelline import Client

client = Client("https://api.orvelline.example", api_key="...")
booking = client.book_pass("tr_51c0aa", "ZONE-AB-7D", "2026-10-03")
print(booking.reference, booking.pass_code, booking.starts_on, booking.price_cents)
```

The API's documentation is at https://docs.orvelline.example.

Run the tests with `python -m pytest`.
