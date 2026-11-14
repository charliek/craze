URL: https://docs.skerrywind.example/v3/reference/observations
Title: Upload observations (v3, deprecated) - Skerrywind telemetry API

# Upload observations (version 3)

> **Deprecated.** Version 3 stops accepting uploads on 31 January 2027. Use version 4.

```
POST /v3/observations
Authorization: SkwToken <station token>
Content-Type: application/json
```

Stores observations for the station the token belongs to.

## Body

```json
{"station": "glenmore-ridge-03",
 "readings": [{"time": "2026-04-02T10:00:00Z", "temp_c": 8.4,
               "pressure_hpa": 1009.6, "wind_ms": 5.1}]}
```

Up to 200 readings per request. Values are floating-point numbers in the units their
names say.

## Response

`201 Created` with `{"stored": 1}`. Sending the same readings twice stores them twice;
version 3 has no way to make a retry safe, so de-duplicate on your side.

## Errors

`401` for a missing or revoked token, `400` for a reading with a missing value.
