URL: https://docs.tallowmere.example/v2/authentication
Title: Authentication (v2, retired) - Tallowmere Locker API

# Authentication (version 2)

> **Retired.** This page describes version 2, which stopped answering on 1 October 2026.

Send your API key in the `Authorization` header with the `Token` scheme:

```
GET /v2/lockers HTTP/1.1
Host: api.tallowmere.example
Authorization: Token tmk_live_0b5f2a...
```

A request without a valid key gets `401 Unauthorized`. Keys are created in the Tallowmere
portal under **Account → Developer access**.
