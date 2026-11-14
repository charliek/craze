URL: https://docs.tallowmere.example/v3/authentication
Title: Authentication - Tallowmere Locker API v3

# Authentication

Every request to version 3 of the Tallowmere Locker API carries your account's API key.
Create and revoke keys in the Tallowmere portal under **Account → Developer access**. A key
belongs to one shipper account and can use only the lockers that account has contracted.

## Sending the key

Send the key in the `TM-Access-Key` header on every request:

```
POST /v3/lockers/lk-utrecht-cs-04/holds HTTP/1.1
Host: api.tallowmere.example
TM-Access-Key: tmk_live_0b5f2a...
Content-Type: application/json
Accept: application/json
```

Version 3 reads the key from that header only:

- `Authorization` is ignored. Version 2's `Authorization: Token <key>` scheme was removed in
  3.0, and a request that sends only that header is unauthenticated.
- The beta's `X-Tallowmere-Key` header is not read.
- A key in the query string (`?access_key=`) is rejected with `400`, so that keys do not end
  up in access logs.

A request without a valid key gets `401 Unauthorized` with the error code
`auth.missing_key`, or `auth.revoked_key` for a key that was revoked.

## Key prefixes

Live keys start `tmk_live_`; test keys start `tmk_test_` and only reach the sandbox at
`https://sandbox.tallowmere.example`, whose lockers are simulated.
