URL: https://docs.tessaroot.example/sdks
Title: SDKs and tools - Tessaroot API

# SDKs and tools

Tessaroot publishes official SDKs for TypeScript (`@tessaroot/api`) and Go
(`tessaroot.example/go/api`). There is no official Python SDK; the API is plain HTTPS and
JSON, so any HTTP client works.

Community libraries we know of:

- `pytessa` (Python), which wraps version 1 only and treats every error as fatal;
- `tessaroot-ex` (Elixir).

The Tessaroot CLI (`tsr`) can call any endpoint from a terminal:
`tsr get /v2/houses/propagation-1/sensors/rh-04/readings --from 2026-03-14 --to 2026-03-15`.
