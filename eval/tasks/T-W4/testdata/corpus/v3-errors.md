URL: https://docs.tallowmere.example/v3/errors
Title: Errors - Tallowmere Locker API v3

# Errors

Errors use standard HTTP status codes and a JSON body:

```json
{"error": {"code": "lockers.full", "message": "No M compartment is free at lk-utrecht-cs-04"}}
```

| Status | When |
|--------|------|
| `400` | A field or parameter is missing, malformed or unknown. The code names it. |
| `401` | The request is not authenticated. |
| `403` | The key is read-only, or the locker is not in your contract. |
| `404` | The resource does not exist, or your account cannot see it. |
| `409` | The request conflicts with the locker's state (no free compartment, parcel already dropped off). |
| `410` | The API version is retired (`version.retired`). |
| `429` | Too many requests; see Rate limits. |
| `5xx` | Something went wrong at Tallowmere; retry with backoff. |

Error codes are stable; messages are for people and may change.
