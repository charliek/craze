# pricing

The price-quote service behind the storefront and the order service.

- `pricing/repository.py` — `ProductRepository`: products and volume tiers from the
  catalog database.
- `pricing/service.py` — `PriceService.quote(sku, qty)`: list price with the best volume
  tier applied.
- `pricing/admin.py` — `PriceAdmin`: the merchandising team's price and tier changes.
- `pricing/app.py` — `build(db, audit_log)`: wires one of each per process.

The service runs as several processes behind a load balancer; each process has its own
`build`. Quote traffic is about 400 requests/s at peak, almost all for a few hundred
SKUs, and every quote costs two database round trips. Price changes are rare (a few per
hour) and are made through `PriceAdmin` on any one of the processes.

Run the tests with `pytest`.
