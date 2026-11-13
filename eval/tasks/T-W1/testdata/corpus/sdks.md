URL: https://docs.quayside.example/sdks
Title: SDKs and tools - Quayside API

# SDKs and tools

Quayside publishes official SDKs for TypeScript (`@quayside/api`) and Go
(`quayside.example/go/api`). There is no official Python SDK yet; the API is plain HTTPS and
JSON, so any HTTP client works.

Community libraries we know of:

- `pyquay` (Python), which wraps version 2 only and has not been updated for version 3;
- `quayside-rb` (Ruby).

The Quayside CLI (`qs`) can call any endpoint from a terminal: `qs get /v3/vessels/9321483`.
