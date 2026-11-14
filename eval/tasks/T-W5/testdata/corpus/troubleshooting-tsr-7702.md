URL: https://docs.tessaroot.example/v2/troubleshooting/tsr-7702
Title: TSR-7702: the sensor was retired - Tessaroot API v2 troubleshooting

# TSR-7702: the sensor was retired

```
HTTP/1.1 409 Conflict
{"error": {"code": "TSR-7702", "message": "sensor retired", "retired_at": "2026-02-01T00:00:00Z"}}
```

## What it means

The sensor was removed from the house in the Tessaroot console (it failed, or was moved to
another bench), and its id was retired. A retired sensor's readings are kept, but the
readings endpoint will not answer a range that starts after `retired_at`.

## The fix

Ask for a range that ends before `retired_at`, or read the replacement sensor instead: the
console's sensor history lists which sensor took over a retired one's position. There is no
request option that reads a retired sensor past its retirement, and retrying gets the same
answer.

## Related

- Error codes: https://docs.tessaroot.example/v2/errors
