URL: https://docs.quayside.example/v3/errors
Title: Errors - Quayside API v3

# Errors

Errors use standard HTTP status codes and a JSON body:

```json
{"error": {"code": "params.service_date", "message": "service_date must be YYYY-MM-DD"}}
```

| Status | When |
|--------|------|
| `400` | A parameter is missing or malformed. The code names the parameter. |
| `401` | The request is not authenticated. |
| `403` | The key may not use this endpoint (read-only keys and booking endpoints). |
| `404` | The resource does not exist, or your organisation cannot see it. |
| `409` | A booking conflicts with an existing one. |
| `429` | Too many requests; see Rate limits. |
| `5xx` | Something went wrong at Quayside; retry with backoff. |

Error codes are stable; messages are for people and may change.
