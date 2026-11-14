URL: https://docs.skerrywind.example/sdks
Title: SDKs and tools - Skerrywind telemetry API

# SDKs and tools

Skerrywind publishes an official gateway agent for Linux (`skw-gateway`, a Debian package)
and a C library for microcontroller stations. There is no official Python library; the
API is plain HTTPS and JSON, so any HTTP client works.

Community libraries we know of:

- `pyskerry` (Python), which speaks version 3 only and has not been updated for
  version 4's seals or units;
- `skerrywind-rs` (Rust), read endpoints only.

The `skw` command-line tool can replay a gateway's log against the test network:
`skw replay --station glenmore-ridge-03 gateway.log`.
