URL: https://docs.quayside.example/v3/rate-limits
Title: Rate limits - Quayside API v3

# Rate limits

Each API key may make 600 requests per minute, with bursts of up to 50 requests per second.
Every response carries the remaining allowance:

```
Quayside-RateLimit-Remaining: 587
Quayside-RateLimit-Reset: 1773468000
```

A request over the limit gets `429 Too Many Requests` with a `Retry-After` header giving the
number of seconds to wait. Clients should wait at least that long before retrying, and
should not retry a `429` more than three times in a row.

Requests against the sandbox (`sandbox.quayside.example`) have a separate limit of 60 per
minute.
