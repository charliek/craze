URL: https://docs.bindlewick.example/changelog
Title: Changelog - Bindlewick Circulation API

# Changelog

## 2.0.0 (2026-09-01)

The first release of version 2. Version 1 stops answering on 31 October 2026; until then
both versions run side by side. Breaking changes, for clients moving from version 1:

- **Checkouts are now loans.** Create one with `POST /v2/loans` (was `POST /v1/checkouts`)
  and list a patron's with `GET /v2/patrons/{patron_id}/loans` (was
  `GET /v1/patrons/{patron_id}/checkouts`). A loan's identifier is `id` (was
  `checkout_id`); new identifiers start `ln_`.
- **The branch is a header.** The `branch` query parameter is replaced by the
  `Bindlewick-Site` request header, required on every version 2 request and holding the same
  code (branches are called sites in version 2). A request that still sends `branch` gets
  `400` with the code `params.unknown`; one without the header gets `400` with
  `site.missing`.
- **Copies, not barcodes.** In request and response bodies `barcode` is renamed
  `copy_ref`. Its value is unchanged: the label printed on the copy.
- **Due timestamps.** `due_date` (a date) is replaced by `due_at`, an RFC 3339 timestamp in
  the site's local time with its UTC offset, for example `2026-12-01T20:00:00-05:00`. The
  day an item is due is the date part as written; converting to UTC first can move it to the
  next day.
- **One loan is wrapped.** A response holding a single loan returns it as
  `{"loan": {...}}`; a list is `{"loans": [...]}`.
- **Renewals are resources.** `POST /v1/checkouts/{id}/renew` is replaced by
  `POST /v2/loans/{loan_id}/renewals`, whose body is an empty JSON object. It answers
  `201 Created` with the renewed loan.
- **Renewal refusals.** A renewal past the patron's limit was `403` with the code
  `renew_limit`; it is now `409 Conflict` with the code `loans.renewal_cap`. `409` is also
  the answer, with the code `loans.not_active`, to renewing a loan that was already
  returned: that is not a refusal under the renewal limit.

Unchanged: authentication (`Authorization: Bearer <key>`), the base URL, the error body's
shape, and patron identifiers.

## 1.14.2 (2026-07-21)

- Fixed: `GET /v1/patrons/{patron_id}/checkouts` could list a checkout twice while it was
  being renewed.

## 1.14.0 (2026-06-02)

- Added `GET /v1/branches`, the branches your key may use.
- Deprecated: version 1. See 2.0.0 when it ships.

Older releases: https://docs.bindlewick.example/changelog/1.x
