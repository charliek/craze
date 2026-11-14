URL: https://forum.ventora-stationhub.example/t/signed-uploads-keep-getting-401/2291
Title: Signed uploads keep getting 401 - Ventora StationHub forum

# Signed uploads keep getting 401

**wx_gatekeeper:** My weather station uploads to StationHub were fine with the old token,
but since switching to signed uploads every `POST /hub/v2/readings` gets 401. I sign the
body with SHA-1 and send it as `X-Ventora-Signature`. Temperatures are in `temp_dc`
(tenths of a degree Celsius) as the docs say.

**ventora_support:** StationHub signatures are HMAC-SHA256, not SHA-1, and the signed string
is the Unix timestamp, a full stop, then the body. Send the timestamp in
`X-Ventora-Timestamp` and the signature as `X-Ventora-Signature: sha256=<hex>`. Requests
whose timestamp is more than five minutes old are refused.

**wx_gatekeeper:** Switched to HMAC-SHA256 with the timestamp prefix and it works now. Thanks!
