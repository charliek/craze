URL: https://docs.tallowmere.example/v3/reference/lockers
Title: Lockers - Tallowmere Locker API v3 reference

# Lockers

## List lockers

```
GET /v3/lockers?city={city}
```

The lockers your account can use, optionally in one city. Since 3.2, `near=<lat>,<lon>` and
`radius_m` (default 1000) find lockers around a point instead.

```json
{"lockers": [{"locker_id": "lk-utrecht-cs-04", "name": "Utrecht Centraal, hall 4",
  "city": "Utrecht", "lat": 52.0894, "lon": 5.1101, "open_24h": true}]}
```

## Free compartments

```
GET /v3/lockers/{locker_id}/availability
```

How many compartments of each size are free right now:
`{"S": 12, "M": 3, "L": 0}`. Availability is a snapshot; book the compartment itself with a
hold, which can still fail with `409` if another shipper took the last one.

Neither endpoint is paged.
