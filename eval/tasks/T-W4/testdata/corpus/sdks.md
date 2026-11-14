URL: https://docs.tallowmere.example/sdks
Title: SDKs and tools - Tallowmere Locker API

# SDKs and tools

Tallowmere publishes official SDKs for TypeScript (`@tallowmere/lockers`) and Java
(`example.tallowmere:lockers-sdk`). Both were updated for version 3 in release 3.0.

There is no official Python SDK. The API is plain HTTPS and JSON, so any HTTP client works.
Community libraries we know of:

- `pylocker-tm` (Python), which supports version 2 only and is no longer maintained;
- `tallow-go` (Go), updated for version 3.

The `tmctl` command-line tool can call any endpoint from a terminal, for example
`tmctl get /v3/lockers?city=Utrecht`.
