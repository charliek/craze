# tallowmere

A small Python client for the Tallowmere parcel-locker API, used by the warehouse's
dispatch scripts to book a locker compartment for each outgoing parcel. Standard library
only: `urllib` for HTTP, `json` for bodies.

```python
from tallowmere import Client

client = Client("https://api.tallowmere.example", api_key="...")
r = client.reserve("lk-utrecht-cs-04", "M", "CN-88213")
print(r.locker, r.compartment, r.code, r.expires_at)
client.cancel(r.id)
```

Tallowmere's API documentation is at https://docs.tallowmere.example.

Run the tests with `python -m pytest`.
