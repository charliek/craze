URL: https://engineering.rookwell.example/2026/04/trying-tallowmere-locker-api-3-beta
Title: Trying the Tallowmere Locker API 3 beta - Rookwell Engineering

# Trying the Tallowmere Locker API 3 beta

*April 2026.* Our dispatch service books a few thousand locker compartments a day through
Tallowmere, so when the version 3 beta opened we ported a branch to it. Notes from the
first week:

- The key moves out of `Authorization` into its own header, `X-Tallowmere-Key`. Our API
  gateway strips `Authorization` on egress, so this was a welcome change for us.
- Reservations are called holds now, and the hold length is `hold_hours` rather than
  `ttl_hours`. The default of 72 hours went away: you have to send it.
- The PIN field has a new name in the response; we just read whichever of the two names is
  present.

Overall the port took an afternoon. We will move production over once 3.0 is final, and
we will re-check the changelog then: the beta is explicitly allowed to change.
