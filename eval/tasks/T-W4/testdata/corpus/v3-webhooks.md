URL: https://docs.tallowmere.example/v3/webhooks
Title: Webhooks - Tallowmere Locker API v3

# Webhooks

Tallowmere can POST an event to your endpoint whenever a parcel moves. Register an endpoint
in the portal under **Account → Developer access → Webhooks**.

## Events

| Event | When |
|-------|------|
| `parcel.dropped_off` | The courier closed the compartment door on the parcel. |
| `parcel.collected` | The recipient opened the compartment with the PIN. |
| `parcel.returned` | The parcel was taken back after the collection period. |

Each event body names the locker, the compartment and the time:

```json
{"event": "parcel.collected", "locker_id": "lk-utrecht-cs-04", "compartment": "B07",
 "at": "2026-10-08T17:02:11Z"}
```

## Verifying events

Every event carries a `TM-Signature` header: `t=<unix time>,v1=<hex HMAC-SHA256>` of
`<t>.<raw body>`, keyed with your webhook secret (not your API key). Reject events whose
signature does not match or whose `t` is more than five minutes old.

Respond with any `2xx` within ten seconds; otherwise the event is retried with backoff for
up to 24 hours.
