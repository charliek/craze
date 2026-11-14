URL: https://docs.orvelline.example/troubleshooting/409-traveller-pass-overlap
Title: 409 traveller.pass_overlap: the traveller already has a pass - Orvelline Passes API troubleshooting

# 409 traveller.pass_overlap

```
HTTP/1.1 409 Conflict

{"error": {"code": "traveller.pass_overlap",
           "message": "Traveller tr_51c0aa already holds ZONE-AB-7D from 2026-10-01",
           "details": {"conflicting_booking": "OB-2190"}}}
```

## What it means

The traveller already holds a pass that covers at least one day of the pass you asked for,
in at least one of the same zones. Orvelline does not sell overlapping passes.

## How to fix it

- Choose a `starts_on` date after the existing pass ends, or
- cancel the existing booking (`DELETE /v2/pass-bookings/{reference}`) if it was a mistake,
  then book again.

Retrying the same request will always fail the same way: this error is permanent for that
body. Report it to the person booking rather than retrying.
