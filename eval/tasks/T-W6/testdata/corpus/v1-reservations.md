URL: https://docs.orvelline.example/v1/reference/reservations
Title: Reservations (v1, retired) - Orvelline Passes API

# Reservations (version 1)

> **Retired.** Version 1 stopped answering on 31 March 2026. Use version 2's pass bookings.

```
POST /v1/reservations
X-Orvelline-Token: <key>
Idempotency-Key: <a unique string>
```

```json
{"customer": "tr_51c0aa", "product": "ZONE-AB-7D", "valid_from": "2026-03-03"}
```

## Pending reservations

When the fare service was slow, version 1 answered `202 Accepted` with
`{"status": "pending", "poll": "/v1/reservations/RV-88213"}`. Poll that path every two
seconds until `status` is `confirmed` or `failed`.

If a request timed out, send it again with the same `Idempotency-Key` and the header
`X-Orvelline-Retry: 1`; the original reservation is returned if it was made.
