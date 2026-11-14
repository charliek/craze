URL: https://docs.tessaroot.example/v2/troubleshooting/tsr-4011
Title: TSR-4011: the token has expired - Tessaroot API v2 troubleshooting

# TSR-4011: the token has expired

```
HTTP/1.1 401 Unauthorized
{"error": {"code": "TSR-4011", "message": "token expired"}}
```

## What it means

API tokens are valid for 90 days. After that every request made with the token gets
`TSR-4011`, whatever the endpoint.

## The fix

Create a new token in the Tessaroot console under **Organisation → API tokens** and replace
the old one wherever it is configured. Tokens cannot be extended. Send the token as
`Authorization: Bearer <token>` on every request; version 2 does not accept a token in the
query string.

A token that was revoked by an administrator gets `TSR-4013` instead, and needs the same fix.

## Related

- Error codes: https://docs.tessaroot.example/v2/errors
