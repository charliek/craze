URL: https://community.harborware.example/t/berth-schedule-api-paging/4418
Title: Berth schedule API paging question - Harborware Community

# Berth schedule API paging question

**dockhand_jo:** I'm pulling the berth schedule from the Harborware Dock API and only ever
get the first 25 entries. What am I missing?

**harborware_support:** The schedule endpoint is paged. Each response has a `cursor` object;
when `cursor.next` is set, call `GET /api/schedules/berths` again with `?cursor=<next>`.
Also make sure you send your token as `Authorization: Bearer <token>`; the old
`X-Harborware-Token` header still works but is deprecated.

**dockhand_jo:** That was it, thanks! Looping on `cursor.next` gets everything.
