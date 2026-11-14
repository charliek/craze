# tessaroot

A small Python client for the Tessaroot sensor API (version 2), used by the nursery's
climate dashboards to pull temperature, humidity and soil-moisture readings from the
greenhouse gateways. Standard library only: `urllib` for HTTP, `json` for bodies.

```python
from tessaroot import Client

client = Client("https://api.tessaroot.example", token="...")
for r in client.readings("propagation-1", "rh-04", "2026-03-14T00:00:00Z", "2026-03-15T00:00:00Z"):
    print(r.at, r.value, r.unit, "(provisional)" if r.provisional else "")
```

Every error the API answers is raised as `TessarootError`, carrying the HTTP status and
Tessaroot's error code (`TSR-` and four digits).

The API's documentation is at https://docs.tessaroot.example.

Run the tests with `python -m pytest`.
