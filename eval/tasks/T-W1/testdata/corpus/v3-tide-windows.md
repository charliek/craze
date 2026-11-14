URL: https://docs.quayside.example/v3/reference/tide-windows
Title: Tide windows - Quayside API v3 reference

# Tide windows

```
GET /v3/harbors/{harbor}/tide-windows?from={date}&days={n}
```

The windows in which a vessel of a given draught can enter or leave the harbour, for up to
seven days from `from`. Add `draught_m` to compute windows for a specific draught; without it
the harbour's design draught is used.

## Response

```json
{"harbor": "rotterdam-maasvlakte",
 "windows": [{"opens": "2026-03-14T04:10:00Z", "closes": "2026-03-14T07:40:00Z",
              "max_draught_m": 16.5}]}
```

Tide windows are computed, not booked, so this endpoint is never paged: a response always
holds every window in the range.
