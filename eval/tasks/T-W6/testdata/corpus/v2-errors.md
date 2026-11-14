URL: https://docs.orvelline.example/v2/errors
Title: Errors - Orvelline Passes API v2

# Errors

Errors use standard HTTP status codes and a JSON body. `details` is present only for codes
that document it:

```json
{"error": {"code": "pass.unknown", "message": "No pass with code ZONE-QX-1D"}}
```

| Status | When |
|--------|------|
| `400` | The body is not JSON, or a field is missing or malformed. |
| `401` | The request is not authenticated. |
| `403` | The key may not use this endpoint. |
| `404` | The resource does not exist, or your account cannot see it. |
| `409` | The request conflicts with the current state: see the code. |
| `410` | Something you asked to continue no longer exists. |
| `422` | The request is well formed but cannot be done: see the code. |
| `425` | A request arrived too early. |
| `429` | Too many requests. |
| `5xx` | Something went wrong at Orvelline. |

Error codes are stable; messages are for people and may change. For the fix for a
particular code, see [Troubleshooting](https://docs.orvelline.example/troubleshooting).

Never retry a `4xx` answer unchanged unless its troubleshooting guide says to: most of them
fail the same way every time.
