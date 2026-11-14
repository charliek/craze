URL: https://docs.tallowmere.example/v2/reference/reservations
Title: Reservations (v2, retired) - Tallowmere Locker API

# Reservations (version 2)

> **Retired.** Version 2 stopped answering on 1 October 2026; every `/v2/` request now gets
> `410 Gone`. See the changelog for what replaced this page.

## Reserve a compartment

```
POST /v2/lockers/{locker_id}/reservations
Authorization: Token <your key>
Content-Type: application/json
```

| Field | Required | Description |
|-------|----------|-------------|
| `size` | yes | `S`, `M` or `L`. |
| `parcel_ref` | yes | Your reference for the parcel. |
| `ttl_hours` | no | How long to keep the compartment, in hours (default 72). |

Fields the API does not know are ignored.

`201 Created`:

```json
{"reservation_id": "rs_81F0aa", "locker_id": "lk-utrecht-cs-04", "compartment": "B07",
 "size": "M", "pickup_code": "482913", "expires_at": "2026-05-21T10:30:00Z"}
```

## Cancel a reservation

```
DELETE /v2/reservations/{reservation_id}
```

`200 OK` with `{"cancelled": true}`.
