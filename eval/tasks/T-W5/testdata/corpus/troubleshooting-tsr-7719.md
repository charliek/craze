URL: https://docs.tessaroot.example/v2/troubleshooting/tsr-7719
Title: TSR-7719: the range is held by a gateway catch-up - Tessaroot API v2 troubleshooting

# TSR-7719: the range is held by a gateway catch-up

```
HTTP/1.1 409 Conflict
{"error": {"code": "TSR-7719", "message": "range held", "backfill_ref": "bf_3a91c7"}}
```

## What it means

The greenhouse's gateway lost its uplink, kept recording on its own storage, and is now
uploading what it buffered. Until that upload finishes, the readings in the range you asked
for are **not settled**: some have not arrived yet and some may still be corrected. Rather
than return a range with silent gaps, the readings endpoint answers `409 Conflict` with
`TSR-7719`. The error body's `backfill_ref` names the upload that holds the range.

## Retrying does not help

A catch-up after a long outage takes hours (up to six hours for a gateway that was offline
for a week), and every request for the held range gets `TSR-7719` until it ends. Do not
retry with backoff, and do not shorten the range: any range that overlaps the held period is
refused.

## The fix: ask for the unsettled readings explicitly

Repeat the same request (same path, same `from` and `to`) once, with both of:

- the query parameter `unsettled=include`, and
- the header `Tessaroot-Backfill-Ref`, set to the `backfill_ref` from the error body.

```
GET /v2/houses/propagation-1/sensors/rh-04/readings?from=...&to=...&unsettled=include HTTP/1.1
Authorization: Bearer tk_live_...
Tessaroot-Backfill-Ref: bf_3a91c7
```

The answer is `200 OK` with every reading received so far, oldest first. Each reading then
carries a `settle_state`: `"final"` for a settled reading, `"pending"` for one that arrived in
the catch-up and may still be revised. Show pending readings as provisional.

`unsettled=include` without the header, or with a reference that is not the current one, is
refused with `400 Bad Request` (`TSR-4019`). The header is only accepted for a range that is
actually held: on any other request it is ignored.

## Related

- Gateway firmware 4.2 release notes: https://www.tessaroot.example/changelog/gateway-firmware-4-2
- Error codes: https://docs.tessaroot.example/v2/errors
