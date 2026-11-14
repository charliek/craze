# skerrywind

A small Python uploader for the Skerrywind station telemetry API (version 4), run on the
field stations' gateways every few minutes. Standard library only: `urllib` for HTTP,
`json` for bodies.

```python
from skerrywind import Client, Reading

client = Client("https://ingest.skerrywind.example", station_id="glenmore-ridge-03",
                signing_secret="...")
receipt = client.upload([
    Reading("2026-04-02T10:00:00Z", temp_c=8.4, pressure_hpa=1009.6, wind_ms=5.1),
    Reading("2026-04-02T10:05:00Z", temp_c=8.6, pressure_hpa=1009.4, wind_ms=4.7),
], batch_id="glenmore-ridge-03-20260402-1000")
print(receipt)
```

The gateway logs readings in the units its sensors report: degrees Celsius, hectopascals
and metres per second. The API's documentation is at https://docs.skerrywind.example.

Run the tests with `python -m pytest`.
