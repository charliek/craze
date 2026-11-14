URL: https://docs.bindlewick.example/v1/reference/checkouts
Title: Checkouts (v1, deprecated) - Bindlewick Circulation API

# Checkouts (version 1)

> **Deprecated.** Version 1 stops answering on 31 October 2026. See the changelog for what
> replaces it.

Every request needs `Authorization: Bearer <key>` and the `branch` query parameter, the code
of the branch where the request happens.

## Check out an item

```
POST /v1/checkouts?branch={branch}

{"patron_id": "p-1042", "barcode": "31205000418823"}
```

`200 OK` with the checkout:

```json
{"checkout_id": "c-88213", "patron_id": "p-1042", "barcode": "31205000418823",
 "title": "The Lantern Orchard", "due_date": "2026-06-01"}
```

## List a patron's checkouts

```
GET /v1/patrons/{patron_id}/checkouts?branch={branch}
```

`200 OK` with `{"checkouts": [...]}`, oldest first.

## Renew a checkout

```
POST /v1/checkouts/{checkout_id}/renew?branch={branch}
```

`200 OK` with the checkout and its new `due_date`. `403` with the code `renew_limit` when
the patron has renewed it as often as the branch allows.
