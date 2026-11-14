URL: https://docs.skerrywind.example/v4/rate-limits
Title: Rate limits - Skerrywind telemetry API v4

# Rate limits

Each station may upload 120 tranches per hour. Read tokens may make 1,000 requests per
minute. Every response carries the remaining allowance:

```
Skw-RateLimit-Remaining: 117
Skw-RateLimit-Reset: 1775124000
```

A request over the limit gets `429 Too Many Requests` with a `Retry-After` header giving the
number of seconds to wait. Gateways should collect samples into tranches (up to 500 per
tranche) rather than upload each one as it is taken.

The test network (`ingest.test.skerrywind.example`) allows 30 tranches per hour per station.
