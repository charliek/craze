URL: https://docs.tallowmere.example/changelog
Title: API changelog - Tallowmere Locker API

# API changelog

Every change to the Tallowmere Locker API, newest first. Breaking changes only ever arrive
with a new major version; minor releases are additive.

## 3.2 (2026-09-15)

- Lockers: `GET /v3/lockers` accepts `near=<lat>,<lon>` and `radius_m`.
- Webhooks: a `hold.expired` event is sent when a hold lapses without a drop-off.

## 3.1 (2026-07-20)

- Holds: the response carries `compartment_door`, the door number printed on the locker
  bank, beside `compartment`.
- Errors: `409` responses carry `retry_after_s` when a compartment of the size may free up
  soon.

## 3.0 (2026-06-02) - breaking

Version 3 is a new major version. Version 2 keeps answering until **30 September 2026**;
from 1 October 2026 every `/v2/` request gets `410 Gone` with the error code
`version.retired`.

### Breaking changes

1. **Authentication.** The `Authorization: Token <key>` scheme is removed. Send your API
   key in the `TM-Access-Key` request header instead. A version 3 request that carries only
   an `Authorization` header gets `401` (`auth.missing_key`). See Authentication.
2. **Reservations are now holds.** The resource is renamed and its endpoints moved:

   | Version 2 | Version 3 |
   |-----------|-----------|
   | `POST /v2/lockers/{locker_id}/reservations` | `POST /v3/lockers/{locker_id}/holds` |
   | `GET /v2/reservations/{reservation_id}` | `GET /v3/holds/{hold_id}` |
   | `DELETE /v2/reservations/{reservation_id}` | `DELETE /v3/holds/{hold_id}` |

3. **Request body.** `parcel_ref` is renamed `consignment_id`. `ttl_hours` (optional,
   default 72) is replaced by `hold_minutes`, which is **required** and has no default:
   a whole number of minutes from 30 to 4320. Send `hold_minutes: 4320` to keep version 2's
   default of 72 hours. Unknown fields are now rejected with `400` (`params.unknown`), so a
   body that still sends `parcel_ref` or `ttl_hours` fails.
4. **Response body.** `reservation_id` is renamed `hold_id`, and `pickup_code` is renamed
   `release_pin` (still six digits, still a string). `locker_id`, `compartment`, `size` and
   `expires_at` are unchanged.
5. **Cancelling.** `DELETE /v3/holds/{hold_id}` answers `204 No Content` with an empty body
   (version 2 answered `200` with `{"cancelled": true}`).

### Changes since the 3.0 beta

If you tried the public beta (March to May 2026): the beta's `X-Tallowmere-Key` header is
not accepted by 3.0, and the beta's `hold_hours` field was replaced by `hold_minutes` before
release. Everything else in the beta carried over.

## 2.9 (2026-02-10)

- Reservations: `size` accepts `L` at every locker bank (it was limited to banks with a
  large column).
- Deprecation notice: version 2 will be retired after version 3 is released.

## 2.8 (2025-11-03)

- Reservations: `ttl_hours` may be up to 96 at lockers in staffed stations.
