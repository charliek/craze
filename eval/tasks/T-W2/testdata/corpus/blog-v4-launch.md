URL: https://www.skerrywind.example/blog/telemetry-api-v4
Title: Version 4 of the telemetry API is here - Skerrywind blog

# Version 4 of the telemetry API is here

Two winters of gateway logs taught us what goes wrong with station uploads: tokens copied
onto a hundred gateways, retries that store the same hour twice, and stations that round
temperatures differently from their neighbours.

Version 4 fixes all three. Uploads are sealed per station, so a leaked secret affects one
station and not your whole network. Each upload (a "tranche") carries your own reference,
so a gateway that retries after a timeout cannot store anything twice. And every
measurement travels as an integer in one fixed unit, so the network's archive no longer
depends on each station's firmware.

Thanks to everyone who tried the beta. We changed a few names on the way to the release,
so check the reference pages rather than your beta notes.

Version 3 keeps accepting uploads until 31 January 2027. The documentation has a reference
page for every version 4 endpoint, and the `skw` tool can replay your gateway logs against
the test network before you switch.
