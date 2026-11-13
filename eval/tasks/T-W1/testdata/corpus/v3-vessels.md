URL: https://docs.quayside.example/v3/reference/vessels
Title: Look up a vessel - Quayside API v3 reference

# Look up a vessel

```
GET /v3/vessels/{imo}
```

Returns one vessel by its seven-digit IMO number. Authenticate as described on the
Authentication page.

## Response

```json
{"imo": "9321483", "name": "Nordic Star", "flag": "NO", "length_m": 294,
 "beam_m": 32.2, "draught_m": 12.1, "type": "container"}
```

`404 Not Found` (error code `vessels.unknown_imo`) for an IMO number Quayside has no record
of. This endpoint is not paged.

## Searching by name

```
GET /v3/vessels?name={prefix}
```

Up to 20 vessels whose name starts with `prefix`, case-insensitively, as `{"vessels": [...]}`.
