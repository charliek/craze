URL: https://docs.tallowmere.example/v3/rate-limits
Title: Rate limits - Tallowmere Locker API v3

# Rate limits

Each API key may make 300 requests per minute. Every response says how many remain:

```
TM-RateLimit-Remaining: 287
TM-RateLimit-Reset: 1791540000
```

A request over the limit gets `429 Too Many Requests` with a `Retry-After` header in
seconds. Wait at least that long before retrying.

Sandbox keys have a separate allowance of 30 requests per minute.
