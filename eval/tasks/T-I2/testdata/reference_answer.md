It's test-order dependence through shared package state.

- `limits.go` has a package-level `var Defaults = Limits{PerMinute: 60, Burst: 10}`, and `New()` starts every bucket from a copy of it.
- `TestConfigureOverrides` (`limits_test.go`) calls `Configure(map[string]string{"burst": "3", "per_minute": "30"})`, which overwrites `Defaults` for the whole process, and never puts it back.
- `TestBurstAllowsTenRequests` builds `New()` and expects 10 requests to pass. If it runs after `TestConfigureOverrides`, its bucket has burst 3 and request 4 is refused — exactly the CI message.

Why only in CI: the Makefile's `test` target (what CI runs) is `go test -race -shuffle=on -count=1 ./...`. With `-shuffle=on` the order changes every run. A plain `go test ./...` runs tests in source order, and `bucket_test.go` comes before `limits_test.go`, so it always passes locally.

Reproduced:

```
$ go test -count=1 -shuffle=2 ./...
-test.shuffle 2
--- FAIL: TestBurstAllowsTenRequests (0.00s)
    bucket_test.go:15: request 4 was refused, want allowed
FAIL
$ go test -count=1 -shuffle=1 ./...
ok  	example.com/quota
```

`go test -count=2 ./...` fails too (the second pass sees the mutated Defaults). It isn't timing: `Allow` takes an explicit time and the test passes a fixed `t0`, and `-race` reports nothing.

Fix options (not applied): restore `Defaults` in the test with `t.Cleanup(func() { Defaults = saved })`, or avoid the global — have `Configure` return `Limits` and pass them to `New`.
