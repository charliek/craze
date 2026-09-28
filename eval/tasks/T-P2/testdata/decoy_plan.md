# Plan

1. Decorate `PriceService.quote` with `functools.lru_cache(maxsize=1024)` so repeated quotes are served from memory.
2. Prices rarely change, so no further handling is needed.
3. Add a test that calls `quote` twice and checks the second call is faster.
