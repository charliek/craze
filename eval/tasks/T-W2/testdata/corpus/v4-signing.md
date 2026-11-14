URL: https://docs.skerrywind.example/v4/signing
Title: Sealing tranches - Skerrywind telemetry API v4

# Sealing tranches

Version 4 has no bearer tokens for uploads. Each station has a **signing secret** (create
and rotate it in the Skerrywind console under **Stations → Keys**), and every upload
carries a seal made with that secret. A secret seals only its own station's tranches.

## The seal header

Send the seal in the `Skw-Tranche-Seal` request header:

```
Skw-Tranche-Seal: s1=<seal>
```

The `s1=` prefix names the scheme and is required. `<seal>` is the lowercase hexadecimal
HMAC-SHA256, keyed with the station's signing secret (its UTF-8 bytes), of this message:

1. the station id named in the request's path, as plain text before any percent-encoding
   (its UTF-8 bytes),
2. one line feed (`0x0A`),
3. the **exact bytes of the request body**.

The station id is part of the message so that a seal cannot be replayed against another
station. Seal the bytes you send: serialising the JSON again after sealing (with different
spacing or key order) changes the bytes, and the seal no longer matches.

In Python:

```python
message = station_id.encode() + b"\n" + body_bytes
seal = hmac.new(secret.encode(), message, hashlib.sha256).hexdigest()
headers["Skw-Tranche-Seal"] = "s1=" + seal
```

## Failures

A request with no seal header gets `401 Unauthorized` with the error code `seal.missing`;
a seal that does not match the station and body, or that was made with a rotated-out
secret, gets `401` with `seal.mismatch`. The `Authorization` header is ignored by the
ingest host.
