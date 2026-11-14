URL: https://docs.skerrywind.example
Title: Skerrywind telemetry API documentation

# Skerrywind telemetry API documentation

The Skerrywind telemetry API takes observations from automatic weather stations and serves
them back to networks, forecasters and researchers. The current version is 4; version 3
stops accepting uploads on 31 January 2027.

## Guides

- Sealing tranches: https://docs.skerrywind.example/v4/signing
- Units and fields: https://docs.skerrywind.example/v4/units
- Station keys: https://docs.skerrywind.example/v4/station-keys
- Errors: https://docs.skerrywind.example/v4/errors
- Rate limits: https://docs.skerrywind.example/v4/rate-limits
- SDKs and tools: https://docs.skerrywind.example/sdks

## Reference (v4)

- Upload a tranche: https://docs.skerrywind.example/v4/reference/tranches
- Look up a station: https://docs.skerrywind.example/v4/reference/stations

## Legacy

- Version 3 observations upload (deprecated): https://docs.skerrywind.example/v3/reference/observations

Uploads go to `https://ingest.skerrywind.example`; reads go to
`https://api.skerrywind.example`. The test network's ingest host is
`https://ingest.test.skerrywind.example`.
