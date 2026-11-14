URL: https://docs.tessaroot.example/v2/reference/readings
Title: Sensor readings - Tessaroot API v2 reference

# Sensor readings

```
GET /v2/houses/{house}/sensors/{sensor}/readings?from={start}&to={end}
Authorization: Bearer <token>
```

Every reading one sensor took in one greenhouse from `from` up to (not including) `to`,
oldest first. A range may span at most 31 days.

## Path parameters

| Name | Description |
|------|-------------|
| `house` | The greenhouse's slug, for example `propagation-1`. |
| `sensor` | The sensor's id within the house, for example `rh-04`. |

## Query parameters

| Name | Required | Description |
|------|----------|-------------|
| `from` | yes | Start of the range, an ISO 8601 timestamp in UTC. |
| `to` | yes | End of the range (exclusive), an ISO 8601 timestamp in UTC. |
| `resolution` | no | `raw` (default), `5m` or `1h`; coarser resolutions average the readings. |

## Response

`200 OK` with a JSON object:

```json
{
  "house": "propagation-1",
  "sensor": "rh-04",
  "readings": [
    {"at": "2026-03-14T06:00:00Z", "value": 81.5, "unit": "pct_rh"}
  ]
}
```

Each reading has `at`, `value` and `unit` (`degC`, `pct_rh`, `ppm`, `lux` or `pct_vwc`).

## Errors

`400` for a missing or malformed range (`TSR-4001`), `401` without a valid token, `404` for a
house or sensor your organisation cannot see (`TSR-4040`), `409` when the range cannot be
answered as asked; see the troubleshooting pages for the code in the body.
