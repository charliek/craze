URL: https://docs.skerrywind.example/v4/errors
Title: Errors - Skerrywind telemetry API v4

# Errors

Errors use standard HTTP status codes and a JSON body:

```json
{"error": {"code": "stations.unknown", "message": "no station glenmore-ridge-99"}}
```

| Status | When |
|--------|------|
| `400` | The body is not JSON, or a query parameter is malformed. |
| `401` | The upload is not sealed, or the read is not authenticated. |
| `403` | The token may not read this station (private stations). |
| `404` | The station or resource does not exist. |
| `409` | The request conflicts with stored data; the code says how. |
| `413` | The body is too large. |
| `422` | The body is JSON but a sample in it is not valid. |
| `429` | Too many requests; see Rate limits. |
| `5xx` | Something went wrong at Skerrywind; retry with backoff. |

Error codes are stable; messages are for people and may change. Each endpoint's reference
page lists the codes it returns.
