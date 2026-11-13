URL: https://docs.quayside.example/v3/authentication
Title: Authenticating requests - Quayside API v3

# Authenticating requests

Every request to the Quayside API v3 is authenticated with an API key. Create keys in the
Quayside console under **Settings → API keys**; a key belongs to one organisation and sees
only that organisation's harbours.

## Sending the key

Send the key in the `X-Quayside-Key` request header, on every request:

```
GET /v3/vessels/9321483 HTTP/1.1
Host: api.quayside.example
X-Quayside-Key: qs_live_5e1d0c...
Accept: application/json
```

Version 3 does not accept the key anywhere else. In particular:

- the `Authorization` header is ignored (version 2's `Authorization: QuaysideKey <key>`
  scheme was removed in version 3);
- a `key` or `api_key` query parameter is rejected with `400 Bad Request`, so keys never
  end up in proxy logs.

A request with no `X-Quayside-Key` header, or with a revoked key, gets `401 Unauthorized`
with the error code `auth.missing_key` or `auth.revoked_key`.

## Test keys

Keys starting `qs_test_` reach the sandbox environment at `https://sandbox.quayside.example`
and never see live data. Live keys start `qs_live_`.
