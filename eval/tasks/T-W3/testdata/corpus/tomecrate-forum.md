URL: https://community.tomecrate.example/t/renewal-returns-409-what-does-it-mean/2231
Title: Renewal returns 409, what does it mean? - Tomecrate Community

# Renewal returns 409, what does it mean?

**kiosk_dev_m:** Our kiosk calls `PUT /api/v3/circulation/items/{itemId}/renew` on the
Tomecrate API and some renewals come back `409`. Is that the renewal limit?

**tomecrate_staff:** Not always. Check `error.reason`: `MAX_RENEWALS` is the limit,
`ITEM_REQUESTED` means another patron has requested the item. Both are 409. Also, since 3.2
the location goes in the `X-Tomecrate-Location` header rather than `?location=`.

**kiosk_dev_m:** That explains it, we were treating every 409 as the limit. Thanks!
