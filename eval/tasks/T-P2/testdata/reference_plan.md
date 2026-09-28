# Plan: a cache for product reads

## Where it sits

Cache at the repository seam, not the quote: add `CachedProductRepository` in `pricing/cache.py`, wrapping a `ProductRepository` and exposing the same four methods. `get(sku)` and `volume_tiers(sku)` read through the cache; `set_list_price` and `replace_volume_tiers` write to the database and then evict that SKU. `app.build` wraps the repository once, so `PriceService` and `PriceAdmin` share the same cache in a process.

Not `PriceService.quote`: its result depends on `qty`, so the key space is much larger, and it would still need invalidation for both price and tier changes.

## Entries

- Keys: `("product", sku)` and `("tiers", sku)`.
- Cache misses too: `get` returning `None` for an unknown SKU, and an empty tier list, are stored so repeated bad SKUs don't hit the database.
- Bounded LRU (e.g. 5,000 entries — traffic is concentrated on a few hundred SKUs), guarded by a lock; optionally a per-key single-flight so a cold popular SKU causes one read, not a stampede.

## Invalidation

- In-process: `PriceAdmin.update_price` / `update_tiers` go through the cached repository, which evicts the SKU after the write succeeds.
- Across processes: the README says the service runs as several processes, each with its own `build`, and an admin change lands on one of them. The others would keep serving the old price. Two options:
  1. A TTL on every entry (say 30–60 s): simplest; other processes are stale for at most the TTL. Price changes are a few per hour, so this is usually acceptable.
  2. A shared cache (Redis) or a pub/sub invalidation message on price change, if staleness must be near zero.
  I'd start with (1), make the TTL configurable, and note it in the README.

## Tests

- Hits: two quotes for the same SKU → `FakeDB.queries` grows by 2, not 4.
- Invalidation: `update_price` then `quote` sees the new price (the existing `test_price_update_is_seen_by_quotes` and `test_tier_update_is_seen_by_quotes` must keep passing through the cache).
- Unknown SKU cached; `UnknownProduct` still raised.
- TTL: an injected clock; after the TTL a quote re-reads.
- Eviction when over capacity.

No code changes made.
