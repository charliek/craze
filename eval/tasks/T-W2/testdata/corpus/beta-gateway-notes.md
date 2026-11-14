URL: https://blog.hillwatch-weather.example/2026/02/skerrywind-v4-beta-gateway
Title: Porting our gateways to the Skerrywind v4 beta - Hillwatch Weather blog

# Porting our gateways to the Skerrywind v4 beta

We run eleven stations on the Skerrywind network and moved two of them to the version 4
beta this month. Notes for anyone doing the same, as of beta 2:

- Uploads go to `POST /v4/stations/{station_id}/observation-batches` with a body of
  `{"batch_id": ..., "observations": [...]}`.
- Each observation has `observed_at`, `air_temp_dk`, `pressure_pa` and `wind_speed_cms`,
  all integers.
- The signature header is `Skw-Batch-Signature: v1=<hex>`, an HMAC-SHA256 of the body
  keyed with the station's secret.
- A retried batch comes back as `409` with `batch.duplicate`; we just log it and move on.

Our temperature conversion was off by one decikelvin for a week because the firmware
truncated instead of rounding. Round, don't truncate.

*Update, May 2026:* Skerrywind say the release will differ from beta 2 in places. We
have not ported to the release yet.
