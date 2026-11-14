URL: https://docs.bindlewick.example/v2/errors
Title: Errors - Bindlewick Circulation API v2

# Errors

Errors use standard HTTP status codes and a JSON body:

```json
{"error": {"code": "patrons.unknown", "message": "no patron p-9999 at this site"}}
```

| Status | When |
|--------|------|
| `400` | A parameter or body field is missing, unknown or malformed. |
| `401` | The key is missing, revoked or expired. |
| `403` | The key may not act at this site. |
| `404` | The resource does not exist at this site. |
| `409` | The request conflicts with the item's or the patron's current state. |
| `429` | Too many requests: wait for the number of seconds in `Retry-After`. |
| `5xx` | Something went wrong at Bindlewick; retry with backoff. |

Codes are stable and listed on each endpoint's reference page; messages are for people and
may change. Branch the client's behaviour on the code, never on the message.
