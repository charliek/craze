URL: https://docs.quayside.example/v3/reference/berth-slots
Title: List berth slots - Quayside API v3 reference

# List berth slots

```
GET /v3/harbors/{harbor}/berth-slots
```

Lists the berth slots booked at one harbour on one service date, in the order the harbour
master scheduled them. Replaces version 2's `GET /v2/berths/schedule`.

## Path parameters

| Name | Description |
|------|-------------|
| `harbor` | The harbour's slug, for example `rotterdam-maasvlakte`. |

## Query parameters

| Name | Required | Description |
|------|----------|-------------|
| `service_date` | yes | The service date, `YYYY-MM-DD`, in the harbour's local time. |
| `page_mark` | no | Where the page starts: the `next_page_mark` of the previous page. Omit it for the first page. |
| `berth` | no | Only this berth's slots, for example `B7`. |

## Paging

A page holds at most 50 slots. When more remain, the response carries `next_page_mark`, an
opaque string: request the next page with the same `service_date` and
`page_mark=<next_page_mark>`. On the last page `next_page_mark` is `null`. Marks expire after
ten minutes; an expired or unknown mark gets `400 Bad Request` with the error code
`paging.bad_mark`.

## Response

`200 OK` with a JSON object:

```json
{
  "harbor": "rotterdam-maasvlakte",
  "service_date": "2026-03-14",
  "slots": [
    {"berth": "B7", "vessel": "Nordic Star", "start": "2026-03-14T06:00:00Z",
     "end": "2026-03-14T14:00:00Z", "pilot": "required"}
  ],
  "next_page_mark": "pm_8f2c41"
}
```

Each slot has `berth`, `vessel`, `start` and `end`; `pilot` is `required`, `optional` or
`none`.

## Errors

`400` for a missing or malformed `service_date` (`params.service_date`), `401` without a valid
key (see Authentication), `404` for a harbour your organisation cannot see.
