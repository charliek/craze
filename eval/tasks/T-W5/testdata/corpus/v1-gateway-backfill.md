URL: https://docs.tessaroot.example/v1/guides/gateway-backfill
Title: Gateway backfill (v1, deprecated) - Tessaroot API

# Gateway backfill (version 1)

> **Deprecated.** Version 1 stops answering on 31 January 2027. Version 2 handles a
> gateway catch-up differently; see the version 2 troubleshooting pages.

When a gateway reconnects after an outage it uploads the readings it buffered. While it does,
version 1's `GET /v1/readings?house={house}&sensor={sensor}&start={start}&end={end}` answers
`503 Service Unavailable` with the error `E_BACKFILL` and a `Retry-After` header.

## Handling it

Wait for the number of seconds in `Retry-After` and repeat the request, at most five times.
If the gateway is still catching up after that, you can ask for what has arrived so far by
adding `partial=1` and the header `X-Tessaroot-Wait: 0`; partial readings carry
`"partial": true`.

```
GET /v1/readings?house=propagation-1&sensor=rh-04&start=...&end=...&partial=1
X-Tessaroot-Wait: 0
```
