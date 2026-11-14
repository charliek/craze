URL: https://docs.bindlewick.example/v2/reference/loans
Title: Loans - Bindlewick Circulation API v2 reference

# Loans

A loan is one copy lent to one patron. Every request needs `Authorization: Bearer <key>`
and the `Bindlewick-Site` header naming the site (branch) where it happens, for example
`Bindlewick-Site: ELM`. Version 2 takes no query parameters on these endpoints.

## The loan object

```json
{"id": "ln_4KD8", "patron_id": "p-1042", "copy_ref": "31205000418823",
 "title": "The Lantern Orchard", "lent_at": "2026-11-10T14:02:11-05:00",
 "due_at": "2026-12-01T20:00:00-05:00", "renewals": 0, "status": "active"}
```

`due_at` is in the site's local time. The date part is the day the copy is due back.
`status` is `active` or `returned`.

## Lend a copy

```
POST /v2/loans
Content-Type: application/json

{"patron_id": "p-1042", "copy_ref": "31205000418823"}
```

`201 Created` with `{"loan": {...}}`. `404` (`patrons.unknown` or `copies.unknown`) for a
patron or copy the site does not know; `409` (`copies.on_loan`) for a copy that is already
out.

## List a patron's loans

```
GET /v2/patrons/{patron_id}/loans
```

`200 OK` with `{"loans": [...]}`: the patron's active loans, oldest first. `404`
(`patrons.unknown`) for an unknown patron.

## Renew a loan

```
POST /v2/loans/{loan_id}/renewals
Content-Type: application/json

{}
```

`201 Created` with `{"loan": {...}}`, its `due_at` moved on by the site's loan period.

| Status | Code | Meaning |
|--------|------|---------|
| `404` | `loans.unknown` | No such loan at this site. |
| `409` | `loans.renewal_cap` | The patron has renewed this loan as often as the site allows. |
| `409` | `loans.not_active` | The loan was already returned. |
| `409` | `holds.waiting` | Another patron has a hold on the title; the loan cannot be renewed until it is filled. |
