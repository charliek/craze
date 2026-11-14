URL: https://docs.orvelline.example/troubleshooting/409-booking-hold-pending
Title: 409 booking.hold_pending: the fare is still being confirmed - Orvelline Passes API troubleshooting

# 409 booking.hold_pending

```
HTTP/1.1 409 Conflict
Content-Type: application/json

{"error": {"code": "booking.hold_pending",
           "message": "The fare for this pass is still being confirmed",
           "details": {"quota_claim": "qc_4c09e2b7", "settle_cs": 40, "quota_left": 12}}}
```

## What it means

At busy times (the first days of a month, the start of the school holidays) Orvelline's
fare engine confirms a pass's fare asynchronously. Your `POST /v2/pass-bookings` request
was accepted and a seat in the pass's quota is **claimed for you**, but the booking does
not exist yet. The claim is identified by `details.quota_claim`.

`details.settle_cs` is how long the fare needs to settle, in **centiseconds** (hundredths
of a second: `40` is 0.4 seconds, `250` is 2.5 seconds). `details.quota_left` is how many
seats remain in the pass's quota; it is for display only and has nothing to do with timing.

This is not a failure, and the request must not be treated as one: the booking completes
once you resend it as described below.

## How to fix it

Resend the **same request** to finish the booking:

1. Wait `details.settle_cs` centiseconds. The wait differs from claim to claim and from one
   answer to the next; always use the value in the answer you just received.
2. Resend promptly: a claim is kept for **3 seconds** after its wait has passed. A resend
   later than that gets `410 booking.hold_lapsed`.
3. Send `POST /v2/pass-bookings` again with exactly the same JSON body and the same
   `Orvelline-Dedup-Key` header value as the original request (see
   [Idempotent requests](https://docs.orvelline.example/v2/idempotent-requests)).
4. Add the header `Orvelline-Resume-Hold` whose value is the claim, `details.quota_claim`:

   ```
   POST /v2/pass-bookings HTTP/1.1
   Authorization: Bearer ok_live_...
   Orvelline-Dedup-Key: 0f6c8f2e-3c55-4d0e-9a51-1b7e2f4c9d10
   Orvelline-Resume-Hold: qc_4c09e2b7
   Content-Type: application/json
   ```

The resend answers one of:

| Status | Code | Meaning | What to do |
|--------|------|---------|------------|
| `201` | | The booking is made; the body is the usual `{"booking": {...}}`. | Done. |
| `409` | `booking.hold_pending` | Still confirming. The `quota_claim` is unchanged; `settle_cs` may differ. | Wait again and resend again. |
| `410` | `booking.hold_released` | The quota sold out while the fare was confirmed. Nothing was booked or charged. | Stop; report the error. |
| `410` | `booking.hold_lapsed` | You resent more than 3 seconds after the wait had passed. The claim is gone. | Stop; start a new booking later. |
| `425` | `booking.resent_too_early` | You resent before `settle_cs` had passed. The claim is released. | Stop; start a new booking later. |
| `422` | `booking.dedup_mismatch` | The resend's body or `Orvelline-Dedup-Key` differs from the original's, or the original had no `Orvelline-Dedup-Key`. | Fix the client. |

A claim is resumable only if the original request carried an `Orvelline-Dedup-Key`.
Claims made without one cannot be finished and expire after ten minutes.

The answer has no `Retry-After` header: that header belongs to `429 rate.exceeded` only.

## What not to do

- **Do not send a new booking request** (without `Orvelline-Resume-Hold`), even with the same
  body. A new request makes a second claim, and when both confirm the traveller has two
  passes and is charged twice.
- Do not resend in a tight loop: a resend before `settle_cs` has passed releases the claim.
- Do not read `settle_cs` as milliseconds or seconds.
- Do not generate a new `Orvelline-Dedup-Key` for the resend.
