URL: https://docs.skerrywind.example/v4/reference/tranches
Title: Upload a tranche - Skerrywind telemetry API v4 reference

# Upload a tranche

```
POST /v4/stations/{station_id}/tranches
Content-Type: application/json
Skw-Tranche-Seal: s1=<seal>
```

A **tranche** is one upload of observations from one station: whatever a gateway has
collected since its last upload. This endpoint stores one tranche. It replaces version 3's
`POST /v3/observations`. Send it to the ingest host, `https://ingest.skerrywind.example`.
The seal is described on Sealing tranches.

## Path parameters

| Name | Description |
|------|-------------|
| `station_id` | The station's id, for example `glenmore-ridge-03`. |

## Body

```json
{
  "tranche_ref": "glenmore-ridge-03-20260402-1000",
  "samples": [
    {"taken_utc": "2026-04-02T10:00:00Z", "air_temp_dk": 2815,
     "station_pres_dapa": 10096, "wind_speed_cms": 510}
  ]
}
```

The body has exactly two members:

- `tranche_ref` (required): your own reference for the tranche, 1 to 64 characters. It
  makes uploads safe to retry; see Retries.
- `samples` (required): 1 to 500 samples, each with exactly the members `taken_utc`
  (RFC 3339, UTC), `air_temp_dk`, `station_pres_dapa` and `wind_speed_cms`. The
  measurement members are integers in the units on Units and fields.

## Response

`202 Accepted`:

```json
{"accepted": 1, "acknowledgement": {"id": "ack_5be7a1", "queued_at": "2026-04-02T10:00:04Z"}}
```

The tranche's receipt is `acknowledgement.id`. Keep it: support asks for it when tracing
an upload.

## Retries

A gateway that times out should send the same tranche again, with the same `tranche_ref`
and the same body. If Skerrywind already stored that tranche, the answer is
`409 Conflict` with the error code `tranche.replayed`, and the body carries the first
upload's acknowledgement:

```json
{"error": {"code": "tranche.replayed", "message": "tranche already stored"},
 "acknowledgement": {"id": "ack_5be7a1", "queued_at": "2026-04-02T10:00:04Z"}}
```

This is not a failure: the tranche is stored, exactly once. Treat it as success and use
that acknowledgement's `id` as the receipt.

A `tranche_ref` that was already used for a **different** body also gets `409`, with the
error code `tranche.ref_reused` and the acknowledgement of the tranche that holds the
reference. Nothing is stored: that is an error, whatever acknowledgement comes with it.
Only `tranche.replayed` means your tranche is stored.

## Errors

`401` for a missing or wrong seal (`seal.missing`, `seal.mismatch`), `404`
(`stations.unknown`) for a station the network does not have, `413` for more than 500
samples, `422` for a sample with a missing, unknown or non-integer member
(`samples.missing_field`, `samples.unknown_field`, `units.not_integer`).
