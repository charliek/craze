URL: https://docs.bindlewick.example/v2/reference/holds
Title: Holds - Bindlewick Circulation API v2 reference

# Holds

A hold reserves a title for a patron: when a copy comes back, it is set aside at the
patron's pickup site instead of returning to the shelf.

## Place a hold

```
POST /v2/holds

{"patron_id": "p-1042", "work_id": "w-55190", "pickup": "OAK"}
```

`201 Created` with `{"hold": {"id": "hd_91LC", "position": 3, "status": "waiting"}}`.
`position` is the patron's place in the queue for that title.

## List a patron's holds

```
GET /v2/patrons/{patron_id}/holds
```

`200 OK` with `{"holds": [...]}`, in queue order. A hold's `status` is `waiting`, `ready`
(a copy is on the pickup shelf, see `ready_until`) or `expired`.

## Cancel a hold

```
DELETE /v2/holds/{hold_id}
```

`204 No Content`. Cancelling a `ready` hold sends the copy to the next patron in the queue.
