URL: https://forum.tramvik.example/t/hold-pending-on-season-tickets/2291
Title: "409 hold pending" when booking season tickets - Tramvik Developer Forum

# "409 hold pending" when booking season tickets

**railbot_dev:** Every month on the 1st our season-ticket bookings through the Tramvik
Tickets API fail with `409 HOLD_PENDING`. After a minute or so the same request works.
Anyone else?

**pernille_k:** Same here. We just catch the 409, sleep two seconds and POST again with a
fresh `Idempotency-Key`, up to five times. Works fine.

**tramvik_staff:** Please don't use a fresh key: send the original `Idempotency-Key` and
add `?await=true` to the URL, and the API will hold the connection open for up to 30
seconds until the ticket is issued. A fresh key can book twice.

**railbot_dev:** `?await=true` did it, thanks.
