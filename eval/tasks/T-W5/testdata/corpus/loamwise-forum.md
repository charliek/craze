URL: https://forum.loamwise.example/t/error-7719-from-the-climate-api/2231
Title: Error 7719 from the climate API - Loamwise Community

# Error 7719 from the climate API

**bench_tomas:** Since last week the Loamwise Climate API gives me `409` with error `LW-7719`
for some of my greenhouses, mostly the ones where the gateway was offline over the weekend.
Anyone know what it means?

**seedling_ana:** Same here. It's the gateway still syncing. I just retry with exponential
backoff (start at 30 seconds, double each time) and after a while it goes through.

**loamwise_support:** That's right for `LW-7719`: the range is being re-synced. You can also
pass `?wait=true` and the request blocks for up to a minute until the sync finishes. Please
send `X-Loamwise-Client` with your app's name so we can see who is affected.

**bench_tomas:** Backoff it is, thanks.
