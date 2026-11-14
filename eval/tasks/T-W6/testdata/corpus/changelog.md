URL: https://docs.orvelline.example/changelog
Title: Changelog - Orvelline Passes API

# Changelog

## 2.6 (1 September 2026)

- At busy times the fare engine now confirms fares asynchronously, so pass bookings no
  longer time out on the first of the month. `POST /v2/pass-bookings` can answer
  `409 booking.hold_pending`; see its troubleshooting guide for how to complete the
  booking.
- New error code `booking.resent_too_early` (`425`).
- The August sandbox preview of this answer carried `details.hold_token` and
  `details.retry_after_ms`; neither is sent any more. The troubleshooting guide documents
  the fields the released answer carries.

## 2.5 (12 June 2026)

- `GET /v2/passes` gains a `zone` filter.
- Booking answers include `ends_on`.

## 2.4 (3 April 2026)

- Version 1 is retired.
- The sandbox's rate limit is raised to 120 requests a minute.

## 2.3 (20 January 2026)

- `POST /v2/pass-bookings` refuses passes starting more than 60 days ahead
  (`400 starts_on.too_far`).
- Version 1's `Idempotency-Key` and `X-Orvelline-Token` headers are not read by version 2;
  see Idempotent requests and Authentication.
