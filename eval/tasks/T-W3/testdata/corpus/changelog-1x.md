URL: https://docs.bindlewick.example/changelog/1.x
Title: Changelog, version 1 releases - Bindlewick Circulation API

# Changelog: version 1 releases

## 1.12.0 (2026-02-16)

- Added: renewal limits per branch. A renewal over the limit gets `403` with the code
  `renew_limit`.
- Changed: `due_date` is the branch's calendar day, not UTC's.

## 1.9.0 (2025-10-07)

- Breaking: the `branch` query parameter is required on every request. Requests without it
  get `400` with the code `branch_required`.
- Added: `GET /v1/patrons/{patron_id}/checkouts` lists a patron's checkouts.

## 1.4.0 (2025-03-11)

- Added: `POST /v1/checkouts/{checkout_id}/renew`.
- Changed: API keys go in `Authorization: Bearer <key>`; the `X-Bindlewick-Key` header is no
  longer accepted.

## 1.0.0 (2024-11-04)

- First release: `POST /v1/checkouts` and `POST /v1/returns`.
