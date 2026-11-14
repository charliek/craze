URL: https://community.kibbleport.example/t/locker-reservations-401-after-api-upgrade/2290
Title: Locker reservations return 401 after the API upgrade - Kibbleport Community

# Locker reservations return 401 after the API upgrade

**fenwick_dispatch:** Since the API upgrade every locker reservation I make gets a 401. The
same key still works in the dashboard. Did the auth change?

**kibbleport_support:** Yes. Version 4 of the Kibbleport Box API takes the key as
`Authorization: Bearer <key>`; the old `X-Kibble-Token` header was removed. The endpoint
also moved, from `/api/v3/reservations` to `/api/v4/bookings`, and the response's
`pickup_code` is now `pickup_pin`.

**fenwick_dispatch:** Switching to Bearer and the new path fixed it, thanks.
