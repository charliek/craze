# quayside

A small Python client for the Quayside harbour-operations API (version 3), used by the
port's planning scripts. Standard library only: `urllib` for HTTP, `json` for bodies.

```python
from quayside import Client

client = Client("https://api.quayside.example", api_key="...")
for slot in client.berth_schedule("rotterdam-maasvlakte", "2026-03-14"):
    print(slot.berth, slot.vessel, slot.start, slot.end)
```

The API's documentation is at https://docs.quayside.example.

Run the tests with `python -m pytest`.
