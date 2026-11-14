URL: https://docs.orvelline.example/v2/reference/pass-bookings
Title: Pass bookings - Orvelline Passes API v2 reference

# Pass bookings

## Book a pass

```
POST /v2/pass-bookings
Authorization: Bearer <key>
Content-Type: application/json
```

```json
{"traveller_id": "tr_51c0aa", "pass_code": "ZONE-AB-7D", "starts_on": "2026-10-03"}
```

| Field | Description |
|-------|-------------|
| `traveller_id` | The traveller, as registered with `POST /v2/travellers`. |
| `pass_code` | The pass product, for example `ZONE-AB-7D` (zones A and B, seven days). |
| `starts_on` | The first day of validity, `YYYY-MM-DD`, at most 60 days ahead. |

`201 Created` with the booking:

```json
{"booking": {"reference": "OB-2207", "traveller_id": "tr_51c0aa",
             "pass_code": "ZONE-AB-7D", "starts_on": "2026-10-03",
             "ends_on": "2026-10-09",
             "price": {"amount_cents": 4150, "currency": "EUR"}}}
```

Errors: `409 booking.hold_pending` at busy times (see Troubleshooting),
`409 traveller.pass_overlap`, `422 pass.unknown`, `422 pass.zone_mismatch`.

## Look up a booking

```
GET /v2/pass-bookings/{reference}
```

The booking, as above. `404 bookings.unknown` for a reference your account did not make.

## Cancel a booking

```
DELETE /v2/pass-bookings/{reference}
```

`204 No Content`. A pass can be cancelled until the day before `starts_on`; later, the
answer is `409 booking.started`. A cancelled booking is refunded in full.
