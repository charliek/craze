URL: https://docs.tallowmere.example/v3/reference/holds
Title: Holds - Tallowmere Locker API v3 reference

# Holds

A hold keeps one compartment of a locker free for one parcel until the courier drops it
off, or until the hold lapses. Holds replace version 2's reservations (see the changelog).
Every request is authenticated as described on the Authentication page.

## Create a hold

```
POST /v3/lockers/{locker_id}/holds
Content-Type: application/json
```

### Path parameters

| Name | Description |
|------|-------------|
| `locker_id` | The locker's id, for example `lk-utrecht-cs-04`. |

### Body

| Field | Required | Description |
|-------|----------|-------------|
| `size` | yes | The compartment size: `S`, `M` or `L`. |
| `consignment_id` | yes | Your reference for the parcel, up to 64 characters. It is printed on the courier's drop-off screen. |
| `hold_minutes` | yes | How long the compartment is held, in whole minutes, from 30 to 4320 (three days). |

Any other field is rejected with `400 Bad Request` and the error code `params.unknown`.

```json
{"size": "M", "consignment_id": "CN-88213", "hold_minutes": 4320}
```

### Response

`201 Created` with the hold:

```json
{
  "hold_id": "hd_5Qm2Tz",
  "locker_id": "lk-utrecht-cs-04",
  "compartment": "B07",
  "compartment_door": 7,
  "size": "M",
  "release_pin": "482913",
  "expires_at": "2026-10-09T10:30:00Z"
}
```

`release_pin` is the six-digit code the recipient types at the locker to open the
compartment. `expires_at` is when the hold lapses if nothing has been dropped off.

### Errors

`400` for a missing or malformed field (the code names it, for example
`params.hold_minutes`), `404` (`lockers.unknown`) for a locker your account cannot use, `409`
(`lockers.full`) when no compartment of that size is free.

## Look up a hold

```
GET /v3/holds/{hold_id}
```

`200 OK` with the hold, as above, plus `state`: `held`, `dropped_off`, `collected` or
`lapsed`.

## Cancel a hold

```
DELETE /v3/holds/{hold_id}
```

Gives the compartment back. `204 No Content` with an empty body. A hold whose parcel was
already dropped off cannot be cancelled (`409`, `holds.dropped_off`); an unknown hold gets
`404` (`holds.unknown`).
