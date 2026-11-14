URL: https://docs.orvelline.example/v2/idempotent-requests
Title: Idempotent requests - Orvelline Passes API v2

# Idempotent requests

Network failures happen. So that a request that creates something (a pass booking, a
refund) can be sent again without creating it twice, Orvelline API v2 accepts a
de-duplication key on every `POST`.

## The Orvelline-Dedup-Key header

Send a key you generate in the `Orvelline-Dedup-Key` request header:

```
POST /v2/pass-bookings HTTP/1.1
Authorization: Bearer ok_live_...
Orvelline-Dedup-Key: 0f6c8f2e-3c55-4d0e-9a51-1b7e2f4c9d10
Content-Type: application/json
```

- Use a new, unique key for each booking you mean to make: a random UUID (version 4) is
  ideal. Keys are at most 64 characters.
- Reuse the key only when you send the *same* booking again.
- Keys are kept for 24 hours. A request repeating a key seen in that time, with the same
  body, gets the original answer replayed and creates nothing. The same key with a
  different body gets `422 Unprocessable Entity` with the code `booking.dedup_mismatch`.

The header is optional for most endpoints, but a pass booking sent without it cannot be
resumed if it is held (see
[409 booking.hold_pending](https://docs.orvelline.example/troubleshooting/409-booking-hold-pending)),
so every client that books passes should send one.

## Version 1

Version 1's `Idempotency-Key` header is not read by version 2; a version 2 request
carrying only `Idempotency-Key` is treated as having no key at all.
