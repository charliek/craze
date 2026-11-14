URL: https://docs.tessaroot.example/v2/errors
Title: Errors - Tessaroot API v2

# Errors

Errors use standard HTTP status codes and a JSON body naming a Tessaroot error code:

```json
{"error": {"code": "TSR-4001", "message": "to must be after from"}}
```

| Code | Status | Meaning |
|------|--------|---------|
| `TSR-4001` | 400 | The range is missing, malformed or longer than 31 days. |
| `TSR-4011` | 401 | The token has expired. See its troubleshooting page. |
| `TSR-4013` | 401 | The token is unknown or was revoked. |
| `TSR-4019` | 400 | A request option is not valid for this request. |
| `TSR-4040` | 404 | No such house or sensor, or your organisation cannot see it. |
| `TSR-7702` | 409 | The sensor was retired. See its troubleshooting page. |
| `TSR-7719` | 409 | The range is held. See its troubleshooting page. |
| `TSR-4290` | 429 | Too many requests; wait for the `Retry-After` seconds. |
| `TSR-5000` | 500 | Something went wrong at Tessaroot; retry with backoff. |

Codes are stable; messages are for people and may change. A `409` is never fixed by
retrying the same request unchanged.
