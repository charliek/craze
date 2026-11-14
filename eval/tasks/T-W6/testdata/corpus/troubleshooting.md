URL: https://docs.orvelline.example/troubleshooting
Title: Troubleshooting - Orvelline Passes API

# Troubleshooting

Start here when a request fails and the error code alone does not tell you why. Each guide
explains what the error means and how to fix it.

## Booking errors

- [409 booking.hold_pending](https://docs.orvelline.example/troubleshooting/409-booking-hold-pending):
  the fare is still being confirmed.
- [409 traveller.pass_overlap](https://docs.orvelline.example/troubleshooting/409-traveller-pass-overlap):
  the traveller already holds a pass for those days.
- 422 pass.zone_mismatch: the pass does not cover the traveller's registered home zone.
  Book a pass whose zones include it, or update the traveller's home zone first.
- 422 pass.unknown: the pass code does not exist or was withdrawn. List current passes with
  `GET /v2/passes`.

## Authentication errors

- 401 auth.invalid_token: the key is wrong, revoked or from the other environment. Sandbox
  keys start `ok_test_`, live keys `ok_live_`.
- 403 auth.scope: the key is read-only. Booking needs a key with the `bookings:write` scope.

## Rate limits

- 429 rate.exceeded: more than 300 requests a minute for one key. Wait for the number of
  seconds in the `Retry-After` header.

## Outages

- 503 fares.unavailable: the fare engine is down for maintenance (announced on the status
  page). Nothing was held or booked: retry later as a new request.
