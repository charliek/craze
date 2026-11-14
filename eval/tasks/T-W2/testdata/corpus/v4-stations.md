URL: https://docs.skerrywind.example/v4/reference/stations
Title: Look up a station - Skerrywind telemetry API v4 reference

# Look up a station

```
GET /v4/stations/{station_id}
Authorization: Bearer <read token>
```

Returns one station's metadata from the read host, `https://api.skerrywind.example`. Read
endpoints use bearer read tokens; uploads are sealed instead (see Sealing tranches).

## Response

```json
{"station_id": "glenmore-ridge-03", "name": "Glenmore Ridge 3",
 "lat": 57.1662, "lon": -3.6731, "elevation_m": 641,
 "sensors": ["temperature", "pressure", "wind"],
 "last_upload_at": "2026-04-02T10:05:12Z"}
```

`404 Not Found` with the error code `stations.unknown` for a station id the network does not
have.

## Latest observations

```
GET /v4/stations/{station_id}/latest
```

The most recent stored observation, in the same fields and units as uploads.
