URL: https://docs.quayside.example/v2/reference/berth-schedule
Title: Berth schedule (v2, retired) - Quayside API

# Berth schedule (version 2)

> **Retired.** Version 2 stopped answering on 30 June 2026. Use version 3.

```
GET /v2/berths/schedule?harbor={harbor}&date={date}
Authorization: QuaysideKey <your key>
```

Returns the berth schedule for one harbour and day.

## Parameters

| Name | Description |
|------|-------------|
| `harbor` | The harbour's slug. |
| `date` | The day, `YYYY-MM-DD`. |
| `offset` | How many entries to skip (default 0). |
| `limit` | How many entries to return, at most 100 (default 100). |

## Response

```json
{"total": 2, "schedule": [{"berth_id": "B7", "ship": "Nordic Star",
  "from": "2026-03-14T06:00:00Z", "to": "2026-03-14T14:00:00Z"}]}
```

Page with `offset` and `limit` until `offset` reaches `total`.
