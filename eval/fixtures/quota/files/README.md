# quota

A token-bucket rate limiter for the API gateway.

```go
b := quota.New()             // Defaults: 60/min, burst 10
if !b.Allow(time.Now()) {
    // 429
}
```

At start-up the gateway calls `quota.Configure` with the `[quota]` section of its
config file (`per_minute`, `burst`).

Run the tests with `make test` (what CI runs).
